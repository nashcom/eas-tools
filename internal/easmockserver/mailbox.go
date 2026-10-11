package easmockserver

import (
	"io"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// The mailbox mode (Options.Mailbox) makes the mock a small real mail store, as far as a mail client needs it: several
// folders, mails that stay where they are until they are moved or deleted, a Sync that reports what changed since the
// last key, ItemOperations Fetch, MoveItems and SendMail with a copy in Sent Items. Without the option none of this
// exists and the older, simpler behavior of the mock is unchanged.

// Folder ids of the mailbox mode.
const (
	FolderInbox   = "5"
	FolderSent    = "6"
	FolderDeleted = "7"
	FolderArchive = "9"
)

// Mail is a message for Seed.
type Mail struct {
	Folder  string // default: Inbox
	From    string
	To      string
	Cc      string
	Subject string
	Body    string
	Read    bool
	Flagged bool
	At      time.Time // default: now
}

type boxMsg struct {
	id      string
	folder  string
	ver     int // changes with every change of the message, so that Sync reports it again
	subject string
	from    string
	to, cc  string
	body    string
	at      time.Time
	read    bool
	flagged bool
	ev      *Event   // a calendar entry; the fields above are only partly used then
	ct      *Contact // an address book entry
}

// boxCursor is what the server knows about one folder of one device: the key it issued last and the versions of the
// mails the device has been told about.
type boxCursor struct {
	key   int
	known map[string]int
}

func (f *Server) boxInit() {
	if f.boxes == nil {
		f.boxes = map[string][]*boxMsg{}
		f.cursors = map[string]*boxCursor{}
	}
}

func addressOf(user string) string {
	if strings.Contains(user, "@") {
		return user
	}
	return user + "@example.com"
}

// Seed puts a mail into the mailbox of a user (Options.Mailbox only) and returns its server id.
func (f *Server) Seed(user string, m Mail) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if m.Folder == "" {
		m.Folder = FolderInbox
	}
	if m.At.IsZero() {
		m.At = time.Now()
	}
	return f.boxPut(user, &boxMsg{folder: m.Folder, subject: m.Subject, from: m.From, to: m.To, cc: m.Cc, body: m.Body,
		at: m.At, read: m.Read, flagged: m.Flagged})
}

// Messages returns the mails of a user in a folder, oldest first (Options.Mailbox only).
func (f *Server) Messages(user, folder string) []Mail {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []Mail
	for _, m := range f.boxes[user] {
		if m.folder == folder {
			out = append(out, Mail{Folder: m.folder, From: m.from, To: m.to, Cc: m.cc, Subject: m.subject, Body: m.body,
				Read: m.read, Flagged: m.flagged, At: m.at})
		}
	}
	return out
}

// boxPut stores a message and gives it an id; call with f.mu held.
func (f *Server) boxPut(user string, m *boxMsg) string {
	f.boxInit()
	f.boxSeq++
	m.id = m.folder + ":" + strconv.Itoa(f.boxSeq)
	m.ver = 1
	f.boxes[user] = append(f.boxes[user], m)
	return m.id
}

func (f *Server) boxFind(user, id string) (int, *boxMsg) {
	for i, m := range f.boxes[user] {
		if m.id == id {
			return i, m
		}
	}
	return -1, nil
}

func (f *Server) boxCursorOf(deviceID, folder string) *boxCursor {
	k := deviceID + "/" + folder
	c := f.cursors[k]
	if c == nil {
		c = &boxCursor{known: map[string]int{}}
		f.cursors[k] = c
	}
	return c
}

// boxFolders is the folder list of the mailbox mode.
func boxFolders() [][3]string {
	return [][3]string{{FolderInbox, "Inbox", "2"}, {FolderSent, "Sent Items", "5"}, {FolderDeleted, "Deleted Items", "4"},
		{"8", "Calendar", "8"}, {FolderArchive, "Archive", "12"}, {FolderContacts, "Contacts", "9"}}
}

// appData is the ApplicationData of a message. truncate > 0 cuts the body to that many bytes.
func appData(m *boxMsg, truncate int) *wbxml.Element {
	if m.ev != nil {
		return eventData(m)
	}
	if m.ct != nil {
		return contactData(m)
	}
	body, truncated := m.body, "0"
	if truncate > 0 && len(body) > truncate {
		body, truncated = body[:truncate], "1"
	}
	read, flag := "0", "0"
	if m.read {
		read = "1"
	}
	if m.flagged {
		flag = "2"
	}
	return wbxml.E(wbxml.PageAirSync, "ApplicationData",
		txt(wbxml.PageEmail, "To", m.to),
		txt(wbxml.PageEmail, "From", m.from),
		txt(wbxml.PageEmail, "Subject", m.subject),
		txt(wbxml.PageEmail, "DateReceived", m.at.UTC().Format("2006-01-02T15:04:05.000Z")),
		txt(wbxml.PageEmail, "Cc", m.cc),
		txt(wbxml.PageEmail, "Read", read),
		txt(wbxml.PageEmail, "Importance", "1"),
		wbxml.E(wbxml.PageEmail, "Flag", txt(wbxml.PageEmail, "FlagStatus", flag)),
		wbxml.E(wbxml.PageAirSyncBase, "Body",
			txt(wbxml.PageAirSyncBase, "Type", "1"),
			txt(wbxml.PageAirSyncBase, "EstimatedDataSize", strconv.Itoa(len(m.body))),
			txt(wbxml.PageAirSyncBase, "Truncated", truncated),
			txt(wbxml.PageAirSyncBase, "Data", body)))
}

// boxSync answers a Sync in the mailbox mode.
func (f *Server) boxSync(deviceID, in string, root *wbxml.Element) *wbxml.Element {
	folder := FolderInbox
	if c := root.Find("CollectionId"); c != nil {
		folder = c.TextContent()
	}
	reply := func(status, key string, extra ...wbxml.Node) *wbxml.Element {
		coll := wbxml.E(wbxml.PageAirSync, "Collection",
			txt(wbxml.PageAirSync, "SyncKey", key),
			txt(wbxml.PageAirSync, "CollectionId", folder),
			txt(wbxml.PageAirSync, "Status", status))
		for _, e := range extra {
			coll.Children = append(coll.Children, e)
		}
		return wbxml.E(wbxml.PageAirSync, "Sync", wbxml.E(wbxml.PageAirSync, "Collections", coll))
	}
	// in the mailbox mode RequireOptions covers every Sync: a Traveler answered the contacts Sync of the EAS library, which has
	// no Options, with status 3 (invalid sync key), although the key was right
	if f.opts.RequireOptions && root.Find("Options") == nil {
		return reply("3", "0")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxInit()
	user := f.dev(deviceID).user
	cur := f.boxCursorOf(deviceID, folder)

	if in == "0" { // start again: the device knows nothing
		cur.key, cur.known = 1, map[string]int{}
		return reply("1", "1")
	}
	if in != strconv.Itoa(cur.key) {
		return reply("3", "0") // invalid sync key
	}

	// the commands of the client
	var answers []wbxml.Node // the Responses of Add commands
	if cmds := root.Find("Commands"); cmds != nil {
		for _, c := range cmds.Children {
			el, ok := c.(*wbxml.Element)
			if !ok {
				continue
			}
			if el.Name == "Add" && el.Find("StartTime") != nil { // a new calendar entry
				answers = append(answers, f.boxAddEvent(user, deviceID, folder, el))
				continue
			}
			id := ""
			if v := el.Find("ServerId"); v != nil {
				id = v.TextContent()
			}
			_, m := f.boxFind(user, id)
			if m == nil {
				continue
			}
			switch el.Name {
			case "Delete":
				if m.folder == FolderDeleted || m.ev != nil || m.ct != nil { // a calendar entry or contact is gone for good
					f.boxRemove(user, id)
				} else {
					m.folder, m.ver = FolderDeleted, m.ver+1
					m.id = FolderDeleted + ":" + strconv.Itoa(f.boxSeq+1)
					f.boxSeq++
				}
			case "Change":
				if v := el.Find("Read"); v != nil {
					m.read = v.TextContent() == "1"
					m.ver++
				}
				if v := el.Find("FlagStatus"); v != nil {
					m.flagged = v.TextContent() != "0"
					m.ver++
				}
			}
		}
	}

	cur.key++
	next := strconv.Itoa(cur.key)
	if g := root.Find("GetChanges"); g != nil && g.TextContent() == "0" {
		return reply("1", next, responsesOf(answers)...)
	}

	window := 5
	if w := root.Find("WindowSize"); w != nil {
		if n, err := strconv.Atoi(w.TextContent()); err == nil && n > 0 {
			window = n
		}
	}
	truncate := 0
	if t := root.Find("TruncationSize"); t != nil {
		truncate, _ = strconv.Atoi(t.TextContent())
	}

	// the date window of the request (FilterType): 0 is no limit
	var oldest time.Time
	if v := root.Find("FilterType"); v != nil {
		days := map[string]int{"1": 1, "2": 3, "3": 7, "4": 14, "5": 30, "6": 90, "7": 180}[v.TextContent()]
		if days > 0 {
			oldest = time.Now().AddDate(0, 0, -days)
		}
	}

	cmds := wbxml.E(wbxml.PageAirSync, "Commands")
	more := false
	n := 0
	inFolder := map[string]bool{}
	for _, m := range f.boxes[user] {
		if m.folder != folder || (!oldest.IsZero() && m.at.Before(oldest)) {
			continue
		}
		inFolder[m.id] = true
		ver, seen := cur.known[m.id]
		if seen && ver == m.ver {
			continue
		}
		if n >= window {
			more = true
			continue
		}
		name := "Add"
		if seen {
			name = "Change"
		}
		cmds.Children = append(cmds.Children, wbxml.E(wbxml.PageAirSync, name,
			txt(wbxml.PageAirSync, "ServerId", m.id), appData(m, truncate)))
		cur.known[m.id] = m.ver
		n++
	}
	var gone []string
	for id := range cur.known {
		if !inFolder[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		cmds.Children = append(cmds.Children, wbxml.E(wbxml.PageAirSync, "Delete", txt(wbxml.PageAirSync, "ServerId", id)))
		delete(cur.known, id)
	}

	var extra []wbxml.Node
	extra = append(extra, responsesOf(answers)...)
	if len(cmds.Children) > 0 {
		extra = append(extra, cmds)
	}
	if more {
		extra = append(extra, wbxml.E(wbxml.PageAirSync, "MoreAvailable"))
	}
	return reply("1", next, extra...)
}

// boxRemove deletes a message for good; call with f.mu held.
func (f *Server) boxRemove(user, id string) {
	if i, m := f.boxFind(user, id); m != nil {
		f.boxes[user] = append(f.boxes[user][:i:i], f.boxes[user][i+1:]...)
	}
}

// boxFetch answers ItemOperations Fetch.
func (f *Server) boxFetch(deviceID string, root *wbxml.Element) *wbxml.Element {
	id := ""
	if v := root.Find("ServerId"); v != nil {
		id = v.TextContent()
	}
	truncate := 0
	if t := root.Find("TruncationSize"); t != nil {
		truncate, _ = strconv.Atoi(t.TextContent())
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxInit()
	_, m := f.boxFind(f.dev(deviceID).user, id)
	if m == nil {
		return wbxml.E(wbxml.PageItemOperations, "ItemOperations", txt(wbxml.PageItemOperations, "Status", "1"),
			wbxml.E(wbxml.PageItemOperations, "Response", wbxml.E(wbxml.PageItemOperations, "Fetch",
				txt(wbxml.PageItemOperations, "Status", "8"), txt(wbxml.PageAirSync, "ServerId", id))))
	}
	props := appData(m, truncate)
	if bp := root.Find("BodyPreference"); bp != nil && bp.Find("Type") != nil && bp.Find("Type").TextContent() == "4" {
		props = appDataMIME(m)
	}
	props.Name, props.Codepage = "Properties", wbxml.PageItemOperations
	return wbxml.E(wbxml.PageItemOperations, "ItemOperations", txt(wbxml.PageItemOperations, "Status", "1"),
		wbxml.E(wbxml.PageItemOperations, "Response", wbxml.E(wbxml.PageItemOperations, "Fetch",
			txt(wbxml.PageItemOperations, "Status", "1"),
			txt(wbxml.PageAirSync, "ServerId", m.id),
			props)))
}

// boxMove answers MoveItems. Status 3 is success (MS-ASCMD).
func (f *Server) boxMove(deviceID string, root *wbxml.Element) *wbxml.Element {
	out := wbxml.E(wbxml.PageMove, "MoveItems")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxInit()
	user := f.dev(deviceID).user
	for _, c := range root.Children {
		el, ok := c.(*wbxml.Element)
		if !ok || el.Name != "Move" {
			continue
		}
		src, dst := el.Find("SrcMsgId").TextContent(), el.Find("DstFldId").TextContent()
		r := wbxml.E(wbxml.PageMove, "Response", txt(wbxml.PageMove, "SrcMsgId", src))
		_, m := f.boxFind(user, src)
		switch {
		case m == nil:
			r.Children = append(r.Children, txt(wbxml.PageMove, "Status", "1")) // invalid source
		case !f.boxHasFolder(dst):
			r.Children = append(r.Children, txt(wbxml.PageMove, "Status", "2")) // invalid destination
		default:
			f.boxSeq++
			m.folder, m.id, m.ver = dst, dst+":"+strconv.Itoa(f.boxSeq), m.ver+1
			r.Children = append(r.Children, txt(wbxml.PageMove, "DstMsgId", m.id), txt(wbxml.PageMove, "Status", "3"))
		}
		out.Children = append(out.Children, r)
	}
	return out
}

func (f *Server) boxHasFolder(id string) bool {
	for _, fo := range boxFolders() {
		if fo[0] == id {
			return true
		}
	}
	return false
}

// boxSend stores a SendMail message: in the mailbox of every known recipient, and as a copy in Sent Items of the sender
// unless the request says not to save one.
func (f *Server) boxSend(root *wbxml.Element, sender string, reply bool) {
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
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return
	}
	body, _ := io.ReadAll(msg.Body)

	var rcpts []*mail.Address
	for _, h := range []string{"To", "Cc"} {
		if l, err := msg.Header.AddressList(h); err == nil {
			rcpts = append(rcpts, l...)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxInit()
	f.sent++

	// SmartReply: the server adds the original to the text, as far as this mock imitates it
	if reply {
		if src := root.Find("ItemId"); src != nil {
			if _, orig := f.boxFind(sender, src.TextContent()); orig != nil {
				body = append(body, []byte("\r\n\r\n-----Original Message-----\r\n"+orig.body)...)
			}
		}
	}

	mk := func(folder string) *boxMsg {
		return &boxMsg{folder: folder, subject: msg.Header.Get("Subject"), from: msg.Header.Get("From"),
			to: msg.Header.Get("To"), cc: msg.Header.Get("Cc"), body: string(body), at: time.Now()}
	}
	if root.Find("SaveInSentItems") != nil {
		f.boxPut(sender, mk(FolderSent))
	}
	if f.opts.DropMail {
		return
	}
	for _, r := range rcpts {
		want := strings.ToLower(r.Address)
		for user := range f.users {
			if strings.ToLower(addressOf(user)) == want {
				m := mk(FolderInbox)
				m.read = false
				f.boxPut(user, m)
			}
		}
	}
}

// rfc822 is the message as a mail file: the headers a mail has and its text.
func rfc822(m *boxMsg) []byte {
	var b strings.Builder
	b.WriteString("From: " + m.from + "\r\n")
	b.WriteString("To: " + m.to + "\r\n")
	if m.cc != "" {
		b.WriteString("Cc: " + m.cc + "\r\n")
	}
	b.WriteString("Subject: " + m.subject + "\r\n")
	b.WriteString("Date: " + m.at.UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: <" + strings.ReplaceAll(m.id, ":", ".") + "@mock.invalid>\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(m.body)
	return []byte(b.String())
}

// appDataMIME is appData with the whole message as the body (body type 4, MIME).
func appDataMIME(m *boxMsg) *wbxml.Element {
	e := appData(m, 0)
	raw := rfc822(m)
	for i, c := range e.Children {
		if el, ok := c.(*wbxml.Element); ok && el.Name == "Body" && el.Codepage == wbxml.PageAirSyncBase {
			e.Children[i] = wbxml.E(wbxml.PageAirSyncBase, "Body",
				txt(wbxml.PageAirSyncBase, "Type", "4"),
				txt(wbxml.PageAirSyncBase, "EstimatedDataSize", strconv.Itoa(len(raw))),
				txt(wbxml.PageAirSyncBase, "Truncated", "0"),
				wbxml.E(wbxml.PageAirSyncBase, "Data", wbxml.Opaque(raw)))
		}
	}
	return e
}
