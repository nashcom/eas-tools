// Package easmockserver is a minimal ActiveSync server, good enough to run the
// probe flow (OPTIONS, Provision, FolderSync, Sync, Settings, Ping). It
// speaks real WBXML through the same codec the client uses. It is used by
// the tests and, through cmd/easmockserver, as a stand-in for Traveler in demos.
package easmockserver

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// Credentials used by the tests. The server accepts any user name with
// this password unless Options.Password says otherwise.
const (
	User     = "alice"
	Password = "secret"
)

// Options shapes the behavior of the mock server. The zero value is what the
// tests use: immediate answers, one new mail per incremental Sync, a Ping
// that answers at once.
type Options struct {
	// Password every account uses; default "secret".
	Password string
	// Latency is added before every answer.
	Latency time.Duration
	// ErrorRate is the share (0..1) of requests answered with HTTP 503.
	ErrorRate float64
	// MailEvery makes a new mail arrive for every device each interval.
	// Zero: every incremental Sync returns exactly one mail.
	MailEvery time.Duration
	// HoldPing makes Ping wait for the heartbeat (or new mail, which ends
	// it with status 2) like a real push server.
	HoldPing bool
	// DropMail accepts SendMail but never delivers the mail, to see how a
	// lost mail shows up.
	DropMail bool
	// KeepSentCopy makes the server keep a copy of every sent mail in the sender's Sent Items (folder 6), whatever
	// SaveInSentItems says, like the Traveler of the lab. The folder is only offered when this is on.
	KeepSentCopy bool
	// MailDelay delivers a sent mail only after this time, to see a mail that is late.
	MailDelay time.Duration
	// MinHeartbeat / MaxHeartbeat (seconds, 0 = no limit) make Ping answer with
	// status 5 and the limit when the requested heartbeat is outside the range,
	// like a server with heartbeat limits.
	MinHeartbeat, MaxHeartbeat int
	// RequireOptions (in the mailbox mode: any Sync without Options) makes a Sync that carries Commands but no Options answer
	// with status 3 and sync key 0, the way HCL Traveler does (seen on the wire).
	RequireOptions bool
	// Sim makes the server honor the X-Mock-Sim request header (see SimHeader):
	// the request itself tells the server how to misbehave.
	Sim bool
	// MinTruncation, in the mailbox mode, answers a Sync that asks for a body cut to fewer bytes with an empty reply (HTTP
	// 200, no body). A Traveler was seen to answer a list like this; the cause is suspected, not confirmed.
	MinTruncation int
	// SendInvitations, in the mailbox mode, makes the server mail an invitation (subject "Invitation: ...") to every attendee of a new
	// calendar entry that is a known user. Whether a real Traveler does this is not known; this is how the tests see the mails.
	SendInvitations bool
	// EchoClientID makes the answer to an Add of a calendar entry carry the client id as the server id, as a Traveler does (seen on
	// the wire); the entry has its real id when it is read again.
	EchoClientID bool
	// Mailbox makes the mock a small real mail store: folders, mails that stay until moved or deleted, Sync of changes,
	// ItemOperations Fetch, MoveItems and SendMail with a copy in Sent Items (see mailbox.go).
	Mailbox bool
}

// Server is the mock EAS endpoint; it implements http.Handler.
type Server struct {
	opts Options
	reg  *wbxml.Registry

	mu           sync.Mutex
	counts       map[string]int
	devices      map[string]*device
	seen         map[string]bool       // device ids that sent a command
	users        map[string]bool       // user names that logged in: the mock directory
	mailbox      map[string][]realMail // delivered mail per user (SendMail), oldest first
	seq          map[string]int        // last mail sequence number per user
	sentbox      map[string][]realMail // copies in Sent Items per user (KeepSentCopy)
	sentSeq      map[string]int
	sent         int    // SendMail commands received
	policyKey    string // key expected after provisioning
	settingsUA   string
	settingsType string
	demand449    bool
	cookieHits   int
	gen          int              // server state generation: see Reset
	syncSeq      int              // sync keys issued in generation > 0
	inject       map[string][]int // one-shot status answers per command
	simUsed      map[string]int   // how often a counted X-Mock-Sim directive was used
	simHits      int              // requests an X-Mock-Sim directive acted on

	boxes   map[string][]*boxMsg  // the mailbox mode: mails per user, in all folders
	cursors map[string]*boxCursor // the mailbox mode: what each device knows of each folder
	boxSeq  int
}

type device struct {
	first      time.Time
	delivered  int
	user       string
	cursor     int // sequence number of the last real mail delivered to this device
	sentCursor int // the same for Sent Items
}

// realMail is a mail that was sent with SendMail and delivered to a user.
type realMail struct {
	seq     int
	subject string
	from    string
	at      time.Time // when it reached the mailbox
}

// New returns a mock server with the default (test) behavior.
func New() *Server {
	return NewWithOptions(Options{})
}

// NewWithOptions returns a mock server with the given behavior.
func NewWithOptions(o Options) *Server {
	if o.Password == "" {
		o.Password = Password
	}
	return &Server{
		opts:    o,
		reg:     wbxml.DefaultRegistry(),
		counts:  map[string]int{},
		devices: map[string]*device{},
		seen:    map[string]bool{},
		users:   map[string]bool{},
		inject:  map[string][]int{},
		simUsed: map[string]int{},
		mailbox: map[string][]realMail{},
		seq:     map[string]int{},
		sentbox: map[string][]realMail{},
		sentSeq: map[string]int{},
	}
}

// Demand449 makes the server forget the policy and answer the next command
// with HTTP 449, as Traveler does when a policy changed.
func (f *Server) Demand449() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.demand449 = true
}

// Reset makes the server forget its state, as if its database was recreated:
// the policy key is gone (the next command gets HTTP 449) and every sync key
// issued before is invalid (FolderSync answers status 9, Sync status 3).
func (f *Server) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.gen++
	f.policyKey = ""
}

// Inject makes the next request of the command (e.g. "Sync", "Ping",
// "FolderSync") answer with just this status, once; call it several times to
// queue more.
func (f *Server) Inject(cmd string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inject[cmd] = append(f.inject[cmd], status)
}

func (f *Server) takeInjected(cmd string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	q := f.inject[cmd]
	if len(q) == 0 {
		return 0, false
	}
	f.inject[cmd] = q[1:]
	return q[0], true
}

// statusOnly is a reply that carries only a status.
func (f *Server) statusOnly(cmd string, status int) *wbxml.Element {
	st := strconv.Itoa(status)
	switch cmd {
	case "FolderSync":
		return wbxml.E(wbxml.PageFolderHierarchy, "FolderSync", txt(wbxml.PageFolderHierarchy, "Status", st))
	case "Ping":
		return wbxml.E(wbxml.PagePing, "Ping", txt(wbxml.PagePing, "Status", st))
	}
	if e := statusOnlyOther(cmd, st); e != nil {
		return e
	}
	return wbxml.E(wbxml.PageAirSync, "Sync", txt(wbxml.PageAirSync, "Status", st))
}

// SettingsSeen returns the User-Agent and DeviceType of the last Settings command.
func (f *Server) SettingsSeen() (userAgent, deviceType string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.settingsUA, f.settingsType
}

// CookieHits returns how many commands arrived with the session cookie the
// server handed out on OPTIONS.
func (f *Server) CookieHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.cookieHits
}

// DeviceCount returns how many different device ids sent commands.
func (f *Server) DeviceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.seen)
}

// Count returns how many times the command (e.g. "Sync") was received.
func (f *Server) Count(cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.counts[cmd]
}

func (f *Server) reply(w http.ResponseWriter, root *wbxml.Element) {
	b, err := wbxml.Marshal(&wbxml.Document{Root: root}, f.reg)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
	_, _ = w.Write(b)
}

func txt(page byte, name, v string) *wbxml.Element {
	return wbxml.E(page, name, wbxml.Text(v))
}

// dev returns the state of a device; call with f.mu held.
func (f *Server) dev(id string) *device {
	d := f.devices[id]
	if d == nil {
		d = &device{first: time.Now()}
		f.devices[id] = d
	}
	return d
}

// pending is the number of mails that arrived for the device and have not
// been delivered yet (MailEvery mode); call with f.mu held.
func (f *Server) pending(id string) int {
	d := f.dev(id)
	return int(time.Since(d.first)/f.opts.MailEvery) - d.delivered
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" || pass != f.opts.Password {
		w.Header().Set("WWW-Authenticate", `Basic realm="eas"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	f.users[user] = true
	f.mu.Unlock()

	if f.simulate(w, r) {
		return
	}

	if f.opts.Latency > 0 {
		select {
		case <-time.After(f.opts.Latency):
		case <-r.Context().Done():
			return
		}
	}

	if r.Method == http.MethodOptions {
		http.SetCookie(w, &http.Cookie{Name: "SessionID", Value: "ABC123", Path: "/", HttpOnly: true})
		w.Header().Set("Server", "nginx/1.29.4")
		w.Header().Set("X-IBM-TRAVELER-HTTP-TIME", "52")
		w.Header().Set("MS-Server-ActiveSync", "8.1")
		w.Header().Set("MS-ASProtocolVersions", "2.5,12.0,12.1,14.0,14.1")
		w.Header().Set("MS-ASProtocolCommands", "Sync,FolderSync,Provision,Ping,SendMail,Settings,Search,ResolveRecipients,ItemOperations,MoveItems,SmartReply,SmartForward")
		return
	}

	if f.opts.ErrorRate > 0 && rand.Float64() < f.opts.ErrorRate {
		http.Error(w, "injected error", http.StatusServiceUnavailable)
		return
	}

	cmd := r.URL.Query().Get("Cmd")
	deviceID := r.URL.Query().Get("DeviceId")
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.counts[cmd]++
	f.seen[deviceID] = true
	if deviceID != "" {
		f.dev(deviceID).user = user
	}
	if c, err := r.Cookie("SessionID"); err == nil && c.Value == "ABC123" {
		f.cookieHits++
	}
	f.mu.Unlock()

	if cmd != "Provision" {
		f.mu.Lock()
		need := f.demand449 || r.Header.Get("X-MS-PolicyKey") != f.policyKey
		if f.demand449 {
			f.demand449 = false
			f.policyKey = ""
		}
		f.mu.Unlock()
		if need {
			w.WriteHeader(449)
			return
		}
	}

	doc, err := wbxml.Unmarshal(body, f.reg)
	if err != nil || doc.Root == nil {
		http.Error(w, "bad wbxml", 400)
		return
	}

	if st, ok := f.takeInjected(cmd); ok {
		f.reply(w, f.statusOnly(cmd, st))
		return
	}

	switch cmd {
	case "Provision":
		key := "111"
		if doc.Root.Find("PolicyKey") != nil {
			key = "222"
			f.mu.Lock()
			f.policyKey = key
			f.mu.Unlock()
		}
		f.reply(w, wbxml.E(wbxml.PageProvision, "Provision",
			txt(wbxml.PageProvision, "Status", "1"),
			wbxml.E(wbxml.PageProvision, "Policies",
				wbxml.E(wbxml.PageProvision, "Policy",
					txt(wbxml.PageProvision, "PolicyType", "MS-EAS-Provisioning-WBXML"),
					txt(wbxml.PageProvision, "Status", "1"),
					txt(wbxml.PageProvision, "PolicyKey", key),
				))))

	case "FolderSync":
		in := doc.Root.Find("SyncKey").TextContent()
		f.mu.Lock()
		gen := f.gen
		f.mu.Unlock()
		prefix := fmt.Sprintf("g%d.", gen)
		if gen > 0 && in != "0" && !strings.HasPrefix(in, prefix) {
			// a key from before the server lost its state
			f.reply(w, f.statusOnly("FolderSync", 9))
			return
		}
		changes := wbxml.E(wbxml.PageFolderHierarchy, "Changes")
		next := "2"
		if gen > 0 {
			next = prefix + "2"
		}
		if in == "0" {
			next = "1"
			if gen > 0 {
				next = prefix + "1"
			}
			add := func(id, name, typ string) *wbxml.Element {
				return wbxml.E(wbxml.PageFolderHierarchy, "Add",
					txt(wbxml.PageFolderHierarchy, "ServerId", id),
					txt(wbxml.PageFolderHierarchy, "ParentId", "0"),
					txt(wbxml.PageFolderHierarchy, "DisplayName", name),
					txt(wbxml.PageFolderHierarchy, "Type", typ))
			}
			changes.Children = append(changes.Children, add("5", "Inbox", "2"), add("8", "Calendar", "8"))
			if f.opts.Mailbox {
				changes.Children = nil
				for _, fo := range boxFolders() {
					changes.Children = append(changes.Children, add(fo[0], fo[1], fo[2]))
				}
			} else if f.opts.KeepSentCopy {
				changes.Children = append(changes.Children, add("6", "Sent Items", "5"))
			}
		}
		f.reply(w, wbxml.E(wbxml.PageFolderHierarchy, "FolderSync",
			txt(wbxml.PageFolderHierarchy, "Status", "1"),
			txt(wbxml.PageFolderHierarchy, "SyncKey", next),
			changes))

	case "ItemOperations":
		if !f.opts.Mailbox {
			http.Error(w, "unknown cmd "+cmd, 400)
			return
		}
		f.reply(w, f.boxFetch(deviceID, doc.Root))

	case "MoveItems":
		if !f.opts.Mailbox {
			http.Error(w, "unknown cmd "+cmd, 400)
			return
		}
		f.reply(w, f.boxMove(deviceID, doc.Root))

	case "Sync":
		in := doc.Root.Find("SyncKey").TextContent()
		if f.opts.Mailbox && f.opts.MinTruncation > 0 && in != "0" {
			if t := doc.Root.Find("TruncationSize"); t != nil {
				if n, _ := strconv.Atoi(t.TextContent()); n < f.opts.MinTruncation {
					w.WriteHeader(http.StatusOK)
					return
				}
			}
		}
		if f.opts.Mailbox {
			f.reply(w, f.boxSync(deviceID, in, doc.Root))
			return
		}
		key := "s" + in
		f.mu.Lock()
		gen := f.gen
		if gen > 0 {
			f.syncSeq++
			key = fmt.Sprintf("g%d.s%d", gen, f.syncSeq)
		}
		f.mu.Unlock()
		if gen > 0 && in != "0" && !strings.HasPrefix(in, fmt.Sprintf("g%d.", gen)) {
			f.reply(w, f.statusOnly("Sync", 3)) // invalid sync key
			return
		}
		f.reply(w, f.syncReply(deviceID, in, key, doc.Root))

	case "SmartReply", "SmartForward":
		if !f.opts.Mailbox {
			http.Error(w, "unknown cmd "+cmd, 400)
			return
		}
		f.mu.Lock()
		sender := f.dev(deviceID).user
		f.mu.Unlock()
		f.boxSend(doc.Root, sender, true)
		w.WriteHeader(http.StatusOK)

	case "SendMail":
		f.mu.Lock()
		sender := f.dev(deviceID).user
		f.mu.Unlock()
		if f.opts.Mailbox {
			f.boxSend(doc.Root, sender, false)
		} else {
			f.deliver(doc.Root, sender)
		}
		w.WriteHeader(http.StatusOK)

	case "Settings":
		f.mu.Lock()
		f.settingsUA = r.UserAgent()
		f.settingsType = r.URL.Query().Get("DeviceType")
		f.mu.Unlock()
		f.reply(w, wbxml.E(wbxml.PageSettings, "Settings",
			txt(wbxml.PageSettings, "Status", "1"),
			wbxml.E(wbxml.PageSettings, "DeviceInformation",
				txt(wbxml.PageSettings, "Status", "1"))))

	case "Search":
		q := ""
		if v := doc.Root.Find("Query"); v != nil {
			q = strings.ToLower(v.TextContent())
		}
		f.reply(w, f.searchReply(q))

	case "Ping":
		f.ping(w, r, deviceID, doc.Root)

	default:
		http.Error(w, "unknown cmd "+cmd, 400)
	}
}

// searchReply answers a GAL search: every user that logged in is an entry;
// an entry matches when the query is part of its address.
func (f *Server) searchReply(q string) *wbxml.Element {
	f.mu.Lock()
	names := make([]string, 0, len(f.users))
	for u := range f.users {
		names = append(names, u)
	}
	f.mu.Unlock()
	sort.Strings(names)

	store := wbxml.E(wbxml.PageSearch, "Store", txt(wbxml.PageSearch, "Status", "1"))
	n := 0
	for _, u := range names {
		addr := u
		if !strings.Contains(addr, "@") {
			addr += "@example.com"
		}
		if !strings.Contains(strings.ToLower(addr), q) {
			continue
		}
		local, _, _ := strings.Cut(addr, "@")
		display := local
		if display != "" {
			display = strings.ToUpper(display[:1]) + display[1:]
		}
		store.Children = append(store.Children, wbxml.E(wbxml.PageSearch, "Result",
			wbxml.E(wbxml.PageSearch, "Properties",
				txt(wbxml.PageGAL, "DisplayName", display),
				txt(wbxml.PageGAL, "Alias", local),
				txt(wbxml.PageGAL, "EmailAddress", addr))))
		n++
	}
	if n > 0 {
		store.Children = append(store.Children,
			txt(wbxml.PageSearch, "Range", "0-"+strconv.Itoa(n-1)),
			txt(wbxml.PageSearch, "Total", strconv.Itoa(n)))
	}
	return wbxml.E(wbxml.PageSearch, "Search",
		txt(wbxml.PageSearch, "Status", "1"),
		wbxml.E(wbxml.PageSearch, "Response", store))
}

const syncWindow = 5

// pendingReal counts the real (SendMail) mails of the device's user that the
// device has not received yet; call with f.mu held.
func (f *Server) pendingReal(deviceID string) int {
	d := f.dev(deviceID)
	n := 0
	for _, m := range f.mailbox[d.user] {
		if m.seq > d.cursor {
			n++
		}
	}
	return n
}

// deliver puts a SendMail message into the mailbox of every known user that
// is among its recipients (an unknown address is dropped, like a bounce).
func (f *Server) deliver(root *wbxml.Element, sender string) {
	var raw []byte
	if m := root.Find("Mime"); m != nil {
		for _, c := range m.Children {
			if o, ok := c.(wbxml.Opaque); ok {
				raw = append(raw, o...)
			}
		}
		if len(raw) == 0 {
			raw = []byte(m.TextContent())
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return
	}

	var rcpts []*mail.Address
	for _, h := range []string{"To", "Cc"} {
		if l, err := msg.Header.AddressList(h); err == nil {
			rcpts = append(rcpts, l...)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent++
	if f.opts.KeepSentCopy {
		f.sentSeq[sender]++
		f.sentbox[sender] = append(f.sentbox[sender], realMail{seq: f.sentSeq[sender], subject: msg.Header.Get("Subject"), from: msg.Header.Get("From")})
	}
	if f.opts.DropMail {
		return
	}
	subject, from := msg.Header.Get("Subject"), msg.Header.Get("From")
	if f.opts.MailDelay > 0 {
		time.AfterFunc(f.opts.MailDelay, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.put(rcpts, subject, from)
		})
		return
	}
	f.put(rcpts, subject, from)
}

// put adds a mail to the mailbox of every known user among the recipients; call with f.mu held.
func (f *Server) put(rcpts []*mail.Address, subject, from string) {
	for _, r := range rcpts {
		want := strings.ToLower(r.Address)
		for user := range f.users {
			addr := user
			if !strings.Contains(addr, "@") {
				addr += "@example.com"
			}
			if strings.ToLower(addr) != want {
				continue
			}
			f.seq[user]++
			f.mailbox[user] = append(f.mailbox[user], realMail{seq: f.seq[user], subject: subject, from: from, at: time.Now()})
		}
	}
}

// deleteMail removes a real mail (ServerId "5:r<seq>") from a user's mailbox.
func (f *Server) deleteMail(user, serverID string) {
	_, tail, ok := strings.Cut(serverID, ":r")
	if !ok {
		return
	}
	seq, err := strconv.Atoi(tail)
	if err != nil {
		return
	}
	box := f.mailbox[user]
	for i, m := range box {
		if m.seq == seq {
			f.mailbox[user] = append(box[:i:i], box[i+1:]...)
			return
		}
	}
}

// SentCount returns how many SendMail commands arrived.
func (f *Server) SentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.sent
}

// MailboxCount returns how many real mails a user currently has.
func (f *Server) MailboxCount(user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.mailbox[user])
}

func (f *Server) syncReply(deviceID, in, key string, root *wbxml.Element) *wbxml.Element {
	if c := root.Find("CollectionId"); c != nil && "6" == c.TextContent() {
		return f.sentSyncReply(deviceID, in, key, root)
	}
	if f.opts.RequireOptions && root.Find("Commands") != nil && root.Find("Options") == nil {
		return wbxml.E(wbxml.PageAirSync, "Sync", wbxml.E(wbxml.PageAirSync, "Collections",
			wbxml.E(wbxml.PageAirSync, "Collection",
				txt(wbxml.PageAirSync, "SyncKey", "0"),
				txt(wbxml.PageAirSync, "CollectionId", "5"),
				txt(wbxml.PageAirSync, "Status", "3"))))
	}
	coll := wbxml.E(wbxml.PageAirSync, "Collection",
		txt(wbxml.PageAirSync, "SyncKey", key),
		txt(wbxml.PageAirSync, "CollectionId", "5"),
		txt(wbxml.PageAirSync, "Status", "1"))

	// Client-originated deletes (the cleanup of the load worker).
	f.mu.Lock()
	user := f.dev(deviceID).user
	if cmds := root.Find("Commands"); cmds != nil {
		for _, c := range cmds.Children {
			if el, ok := c.(*wbxml.Element); ok && el.Name == "Delete" {
				if id := el.Find("ServerId"); id != nil {
					f.deleteMail(user, id.TextContent())
				}
			}
		}
	}
	f.mu.Unlock()

	// A request without GetChanges only carries client changes: no delivery,
	// or the items would be lost for the client.
	if in != "0" && root.Find("GetChanges") != nil {
		cmds := wbxml.E(wbxml.PageAirSync, "Commands")
		more := false

		f.mu.Lock()
		n := 1
		first := 0
		if f.opts.MailEvery > 0 {
			n = f.pending(deviceID)
			if n > syncWindow {
				n, more = syncWindow, true
			}
			if n < 0 {
				n = 0
			}
			d := f.dev(deviceID)
			first = d.delivered
			d.delivered += n
		}
		for i := 0; i < n; i++ {
			cmds.Children = append(cmds.Children, wbxml.E(wbxml.PageAirSync, "Add",
				txt(wbxml.PageAirSync, "ServerId", "5:"+strconv.Itoa(first+i+1)),
				wbxml.E(wbxml.PageAirSync, "ApplicationData",
					txt(wbxml.PageEmail, "Subject", "hello"))))
		}

		// the mails that were really sent to this user
		d := f.dev(deviceID)
		room := syncWindow - n
		for _, m := range f.mailbox[d.user] {
			if m.seq <= d.cursor {
				continue
			}
			if room <= 0 {
				more = true
				break
			}
			cmds.Children = append(cmds.Children, wbxml.E(wbxml.PageAirSync, "Add",
				txt(wbxml.PageAirSync, "ServerId", "5:r"+strconv.Itoa(m.seq)),
				wbxml.E(wbxml.PageAirSync, "ApplicationData",
					txt(wbxml.PageEmail, "Subject", m.subject),
					txt(wbxml.PageEmail, "From", m.from),
					txt(wbxml.PageEmail, "DateReceived", m.at.UTC().Format("2006-01-02T15:04:05.000Z")))))
			d.cursor = m.seq
			room--
		}
		f.mu.Unlock()

		if len(cmds.Children) > 0 {
			coll.Children = append(coll.Children, cmds)
		}
		if more {
			coll.Children = append(coll.Children, wbxml.E(wbxml.PageAirSync, "MoreAvailable"))
		}
	}

	return wbxml.E(wbxml.PageAirSync, "Sync",
		txt(wbxml.PageAirSync, "Status", "1"),
		wbxml.E(wbxml.PageAirSync, "Collections", coll))
}

// ping answers a Ping. Without HoldPing it answers at once with "no
// changes". With HoldPing it waits for the heartbeat; a new mail (MailEvery
// mode) ends it early with status 2 and the Inbox as changed folder.
func (f *Server) ping(w http.ResponseWriter, r *http.Request, deviceID string, root *wbxml.Element) {
	status := "1"
	var changed *wbxml.Element

	hb := 60
	if v := root.Find("HeartbeatInterval"); v != nil {
		if n, err := strconv.Atoi(v.TextContent()); err == nil && n > 0 {
			hb = n
		}
	}
	// heartbeat limits: status 5 with the allowed value (MS-ASCMD)
	if f.opts.MinHeartbeat > 0 && hb < f.opts.MinHeartbeat {
		f.reply(w, wbxml.E(wbxml.PagePing, "Ping", txt(wbxml.PagePing, "Status", "5"),
			txt(wbxml.PagePing, "HeartbeatInterval", strconv.Itoa(f.opts.MinHeartbeat))))
		return
	}
	if f.opts.MaxHeartbeat > 0 && hb > f.opts.MaxHeartbeat {
		f.reply(w, wbxml.E(wbxml.PagePing, "Ping", txt(wbxml.PagePing, "Status", "5"),
			txt(wbxml.PagePing, "HeartbeatInterval", strconv.Itoa(f.opts.MaxHeartbeat))))
		return
	}

	if f.opts.HoldPing {
		deadline := time.NewTimer(time.Duration(hb) * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()

	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				break wait
			case <-tick.C:
				f.mu.Lock()
				n := f.pendingReal(deviceID)
				if f.opts.MailEvery > 0 {
					n += f.pending(deviceID)
				}
				f.mu.Unlock()
				if n > 0 {
					status = "2"
					changed = wbxml.E(wbxml.PagePing, "Folders", txt(wbxml.PagePing, "Folder", "5"))
					break wait
				}
			}
		}
	}

	out := wbxml.E(wbxml.PagePing, "Ping", txt(wbxml.PagePing, "Status", status))
	if changed != nil {
		out.Children = append(out.Children, changed)
	}
	f.reply(w, out)
}

// SentboxCount returns how many copies a user has in Sent Items (KeepSentCopy).
func (f *Server) SentboxCount(user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sentbox[user])
}

// sentSyncReply answers a Sync of the Sent Items folder (id 6): the copies the user has not been told about yet, and
// the deletes of the client.
func (f *Server) sentSyncReply(deviceID, in, key string, root *wbxml.Element) *wbxml.Element {
	coll := wbxml.E(wbxml.PageAirSync, "Collection",
		txt(wbxml.PageAirSync, "SyncKey", key),
		txt(wbxml.PageAirSync, "CollectionId", "6"),
		txt(wbxml.PageAirSync, "Status", "1"))

	f.mu.Lock()
	defer f.mu.Unlock()

	d := f.dev(deviceID)
	if cmds := root.Find("Commands"); cmds != nil {
		for _, c := range cmds.Children {
			el, ok := c.(*wbxml.Element)
			if !ok || el.Name != "Delete" || el.Find("ServerId") == nil {
				continue
			}
			_, tail, _ := strings.Cut(el.Find("ServerId").TextContent(), ":r")
			seq, _ := strconv.Atoi(tail)
			box := f.sentbox[d.user]
			for i, m := range box {
				if m.seq == seq {
					f.sentbox[d.user] = append(box[:i:i], box[i+1:]...)
					break
				}
			}
		}
	}

	if in != "0" && root.Find("GetChanges") != nil {
		cmds := wbxml.E(wbxml.PageAirSync, "Commands")
		for _, m := range f.sentbox[d.user] {
			if m.seq <= d.sentCursor {
				continue
			}
			cmds.Children = append(cmds.Children, wbxml.E(wbxml.PageAirSync, "Add",
				txt(wbxml.PageAirSync, "ServerId", "6:r"+strconv.Itoa(m.seq)),
				wbxml.E(wbxml.PageAirSync, "ApplicationData",
					txt(wbxml.PageEmail, "Subject", m.subject),
					txt(wbxml.PageEmail, "From", m.from))))
			d.sentCursor = m.seq
		}
		if len(cmds.Children) > 0 {
			coll.Children = append(coll.Children, cmds)
		}
	}

	return wbxml.E(wbxml.PageAirSync, "Sync",
		txt(wbxml.PageAirSync, "Status", "1"),
		wbxml.E(wbxml.PageAirSync, "Collections", coll))
}
