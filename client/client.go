package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/probe"
)

// Options of a Manager.
type Options struct {
	// AllowWrite lets the client change the mailbox: mark mail read or unread, move it and delete it. Reading is always
	// allowed.
	AllowWrite bool
	// AllowSend lets the client send mail. It is a switch of its own: a mail leaves the mailbox and cannot be taken back,
	// and AllowWrite does not imply it.
	AllowSend bool
	// SendDomains limits the domains that mail is sent to (send, reply, forward): "example.com" is that domain, "*.example.com" every domain
	// below it. Empty means no restriction.
	SendDomains []string
	// Store keeps the device state (policy key, sync keys, folders); nil keeps it in memory only.
	Store *probe.Store
	// Logger receives the log of the sessions; nil discards it.
	Logger *slog.Logger
	// Trace, when set, receives every request and response with the WBXML decoded (no password, no cookies).
	Trace *probe.Tracer
	// Insecure and CAFile are the defaults for accounts that do not say.
	Insecure bool
	CAFile   string
	// TimeZone is the zone that times without an offset are in, and that new calendar entries and their invitations carry: a name of the
	// zone database such as Europe/Berlin. Empty (or "local") is the zone of the machine. An account can have its own.
	TimeZone string
}

// Manager holds the accounts and one open session for each of them. It is safe for concurrent use: the operations on one
// account run one after the other, because a session is one device with one set of sync keys.
type Manager struct {
	opts    Options
	entries map[string]*entry
	names   []string
}

type entry struct {
	acct Account
	zone probe.Zone
	mu   sync.Mutex
	sess *probe.Session
}

// NewManager checks the accounts and prepares the manager. It does not connect: a session is opened by the first
// operation on the account.
func NewManager(accounts []Account, o Options) (*Manager, error) {
	if len(accounts) == 0 {
		return nil, errors.New("no account is configured")
	}
	m := &Manager{opts: o, entries: map[string]*entry{}}
	for _, a := range accounts {
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("account %q: %w", a.Name, err)
		}
		if _, dup := m.entries[a.Name]; dup {
			return nil, fmt.Errorf("account %q is defined twice", a.Name)
		}
		z, err := zoneOf(a.TimeZone, o.TimeZone)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", a.Name, err)
		}
		m.entries[a.Name] = &entry{acct: a, zone: z}
		m.names = append(m.names, a.Name)
	}
	sort.Strings(m.names)
	return m, nil
}

// Close closes the sessions.
func (m *Manager) Close() {
	for _, e := range m.entries {
		e.mu.Lock()
		if e.sess != nil {
			e.sess.Close()
			e.sess = nil
		}
		e.mu.Unlock()
	}
}

// Accounts lists the accounts. It does not connect.
func (m *Manager) Accounts() *AccountsResult {
	out := &AccountsResult{Accounts: []AccountInfo{}, AllowWrite: m.opts.AllowWrite, AllowSend: m.opts.AllowSend}
	for _, n := range m.names {
		a := m.entries[n].acct
		out.Accounts = append(out.Accounts, AccountInfo{Name: a.Name, Server: a.Server, Email: a.Email, User: a.User})
	}
	return out
}

// resolve returns the name of the account: the one given, or the only one there is.
func (m *Manager) resolve(name string) (*entry, error) {
	if name == "" {
		if len(m.names) == 1 {
			return m.entries[m.names[0]], nil
		}
		return nil, newError(http.StatusBadRequest, "account_required",
			fmt.Sprintf("several accounts are configured (%s): name one", strings.Join(m.names, ", ")))
	}
	e, ok := m.entries[name]
	if !ok {
		return nil, newError(http.StatusNotFound, "unknown_account", fmt.Sprintf("no account %q", name))
	}
	return e, nil
}

func (m *Manager) needWrite(what string) error {
	if m.opts.AllowWrite {
		return nil
	}
	return newError(http.StatusForbidden, "write_disabled", what+" changes the mailbox, and writing is not allowed: start with -write (write in the config)")
}

func (m *Manager) needSend() error {
	if m.opts.AllowSend {
		return nil
	}
	return newError(http.StatusForbidden, "send_disabled", "sending a mail is not allowed: start with -send (send in the config)")
}

// open brings a session to the point where it can read and write: protocol version, registration, device information
// and the folder list. The client has a device of its own, so it does not share sync keys with the probe or the monitor.
func (m *Manager) open(ctx context.Context, a Account) (*probe.Session, error) {
	password, err := a.password()
	if err != nil {
		return nil, err
	}
	server, err := probe.NormalizeServerURL(a.Server)
	if err != nil {
		return nil, err
	}
	insecure, ca := m.opts.Insecure, m.opts.CAFile
	if a.Insecure != nil {
		insecure = *a.Insecure
	}
	if a.CAFile != "" {
		ca = a.CAFile
	}
	headers, err := probe.ParseHeaders(a.headerLines())
	if err != nil {
		return nil, err
	}
	login := a.login()
	id := a.DeviceID
	if id == "" {
		id = probe.DeriveDeviceID(strings.ToLower(login), server+"#client")
	}
	s, err := probe.NewSession(probe.Config{
		ServerURL: server, Email: a.Email, Username: login, Password: password, DeviceID: id, Profile: a.Profile,
		Provision: a.Provision, Store: m.opts.Store, Insecure: insecure, CACertFile: ca, Headers: headers,
		StepTimeout: 30 * time.Second, Logger: m.opts.Logger, Trace: m.opts.Trace,
	})
	if err != nil {
		return nil, err
	}
	if err := s.Options(ctx, nil); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.Provision(ctx, nil); err != nil {
		s.Close()
		return nil, err
	}
	if s.HasDeviceInfo() {
		_ = s.DeviceInfo(ctx, nil) // like the other tools: not fatal
	}
	if err := s.FolderSync(ctx, nil); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// with runs f on the open session of an account. A session that failed is dropped, so that the next operation connects
// again.
func (m *Manager) with(ctx context.Context, name string, f func(a Account, s *probe.Session) error) error {
	e, err := m.resolve(name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.sess == nil {
		s, err := m.open(ctx, e.acct)
		if err != nil {
			return classify(err)
		}
		e.sess = s
	}
	if err := f(e.acct, e.sess); err != nil {
		var ce *Error
		if !errors.As(err, &ce) { // a failure of the connection or the server, not of the request
			e.sess.Close()
			e.sess = nil
		}
		return classify(err)
	}
	return nil
}

// classify turns a failure into an Error with a code and an HTTP status.
func classify(err error) error {
	var ce *Error
	var he *eas.HTTPError
	var se *eas.StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, context.DeadlineExceeded):
		return newError(http.StatusGatewayTimeout, "timeout", "the server did not answer in time")
	case errors.Is(err, context.Canceled):
		return newError(http.StatusRequestTimeout, "canceled", "the request was canceled")
	case errors.As(err, &he) && (he.StatusCode == 401 || he.StatusCode == 403):
		return newError(http.StatusBadGateway, "auth_failed", "the server refused the login: "+err.Error())
	case errors.As(err, &he):
		return newError(http.StatusBadGateway, "server_error", err.Error())
	case errors.As(err, &se):
		return newError(http.StatusBadGateway, "eas_status", err.Error())
	}
	return newError(http.StatusInternalServerError, "error", err.Error())
}

func folderOf(s *probe.Session, ref string) (probe.Folder, error) {
	if ref == "" {
		return probe.Folder{}, newError(http.StatusBadRequest, "folder_required", "a folder is required (an id, or inbox, sent, drafts, deleted, or its name)")
	}
	f, err := s.FindFolder(ref)
	if err != nil {
		code := "folder_not_found"
		status := http.StatusNotFound
		if strings.Contains(err.Error(), "use the id") {
			code, status = "folder_ambiguous", http.StatusBadRequest
		}
		return probe.Folder{}, newError(status, code, err.Error())
	}
	return f, nil
}

func folderJSON(f probe.Folder) Folder {
	return Folder{ID: f.ID, Parent: f.Parent, Name: f.Name, Type: f.Type}
}

func messageJSON(m probe.Message) Message {
	return Message{
		ID: m.ID, Folder: m.Folder, Subject: m.Subject, From: m.From, To: m.To, Cc: m.Cc, Date: stamp(m.Date),
		Read: m.Read, Flagged: m.Flagged, Importance: importanceName(m.Importance), HasAttachments: m.HasAttachments,
		Body: m.Body, BodySize: m.BodySize, BodyTruncated: m.BodyTruncated,
	}
}

// Info says what the server and the device are.
func (m *Manager) Info(ctx context.Context, account string) (*Info, error) {
	var out *Info
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		srv := s.Server()
		out = &Info{
			Account: a.Name, User: a.login(), Email: s.SelfAddress(),
			Server: ServerInfo{URL: srv.URL, Product: srv.Product, ProtocolVersion: srv.ProtocolVersion, Versions: srv.Versions,
				Commands: srv.Commands, Policy: srv.Policy},
			Device:   DeviceInfo{ID: s.DeviceID(), Provisioned: s.Provisioned()},
			Folders:  len(s.Folders()),
			TimeZone: m.zoneName(a),
		}
		return nil
	})
	return out, err
}

// Folders lists the folders of the mailbox.
func (m *Manager) Folders(ctx context.Context, account string) (*FoldersResult, error) {
	var out *FoldersResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		out = &FoldersResult{Account: a.Name, Folders: []Folder{}}
		for _, f := range s.Folders() {
			out.Folders = append(out.Folders, folderJSON(f))
		}
		return nil
	})
	return out, err
}

// ListParams shape a list.
type ListParams struct {
	// Limit is the number of mails, newest first (default 20; 0 is the default, -1 all).
	Limit int `json:"limit"`
	// Preview is how many bytes of the body each mail carries (default 0: none).
	Preview int `json:"preview"`
	// Since is the date window: 1d, 3d, 1w, 2w (default), 1m, 3m or 6m.
	Since string `json:"since"`
}

// List returns the mails of a folder, newest first.
func (m *Manager) List(ctx context.Context, account, folder string, p ListParams) (*ListResult, error) {
	var out *ListResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderOf(s, folder)
		if err != nil {
			return err
		}
		limit := p.Limit
		if limit == 0 {
			limit = 20
		} else if limit < 0 {
			limit = 0
		}
		filter, err := probe.ParseSince(p.Since)
		if err != nil {
			return newError(http.StatusBadRequest, "bad_request", err.Error())
		}
		msgs, err := s.ListMail(ctx, f.ID, probe.ListOptions{Limit: limit, Preview: p.Preview, Since: filter})
		if err != nil {
			return err
		}
		out = &ListResult{Account: a.Name, Folder: folderJSON(f), Since: probe.SinceName(filter), Count: len(msgs), Messages: []Message{}}
		for _, msg := range msgs {
			mj := messageJSON(msg)
			if p.Preview <= 0 {
				mj.Body, mj.BodyTruncated = "", false
			}
			out.Messages = append(out.Messages, mj)
		}
		return nil
	})
	return out, err
}

// Get returns one mail with its whole body.
func (m *Manager) Get(ctx context.Context, account, folder, id string) (*GetResult, error) {
	var out *GetResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderForMail(s, folder, id)
		if err != nil {
			return err
		}
		if id == "" {
			return newError(http.StatusBadRequest, "id_required", "the id of the mail is required")
		}
		msg, err := s.FetchMail(ctx, f.ID, id)
		if err != nil {
			return mailError(err, id)
		}
		out = &GetResult{Account: a.Name, Message: messageJSON(*msg)}
		return nil
	})
	return out, err
}

// mailError turns the failure of an operation on one mail into a "not found" when that is what the server said.
func mailError(err error, id string) error {
	switch {
	case errors.Is(err, probe.ErrNoSender):
		return newError(http.StatusUnprocessableEntity, "no_sender", "the mail has no sender to reply to")
	case errors.Is(err, probe.ErrNoRecipient):
		return newError(http.StatusUnprocessableEntity, "recipient_required", "the reply or forward would go to nobody")
	}
	var se *eas.StatusError
	if errors.As(err, &se) && strings.HasPrefix(se.Command, "ItemOperations") && (se.Code == 8 || se.Code == 6) {
		return newError(http.StatusNotFound, "mail_not_found", fmt.Sprintf("the server has no mail %s in that folder", id))
	}
	return err
}

// Send sends a mail from the account.
func (m *Manager) Send(ctx context.Context, account string, r SendRequest) (*SendResult, error) {
	if err := m.needSend(); err != nil {
		return nil, err
	}
	if len(r.To) == 0 {
		return nil, newError(http.StatusBadRequest, "recipient_required", "at least one recipient (to) is required")
	}
	if err := m.checkRecipients(r.To, r.Cc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Subject) == "" {
		return nil, newError(http.StatusBadRequest, "subject_required", "a subject is required")
	}
	var out *SendResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		save := r.SaveInSent == nil || *r.SaveInSent
		tag := fmt.Sprintf("client-%d", time.Now().UnixMilli())
		mail := probe.Mail{To: r.To, Cc: r.Cc, Subject: r.Subject, Body: r.Body, SkipSaveInSent: !save}
		if mail.Body == "" {
			mail.Body = "\r\n" // an empty body is not "no body": the filler of Mail would be used
		}
		if err := s.SendMail(ctx, nil, mail, tag); err != nil {
			return err
		}
		out = &SendResult{Account: a.Name, Action: "send", From: s.SelfAddress(), To: r.To, Cc: r.Cc, Subject: r.Subject, Sent: true, Time: stamp(time.Now())}
		return nil
	})
	return out, err
}

// SetRead marks a mail as read or unread.
func (m *Manager) SetRead(ctx context.Context, account, folder, id string, read bool) (*ChangeResult, error) {
	action := "unread"
	if read {
		action = "read"
	}
	if err := m.needWrite("marking a mail as " + action); err != nil {
		return nil, err
	}
	return m.change(ctx, account, folder, id, action, func(s *probe.Session, f probe.Folder) (string, error) {
		return "", s.MarkRead(ctx, f.ID, id, read)
	})
}

// Delete deletes a mail: it goes to Deleted Items, and a mail that is there already is gone for good.
func (m *Manager) Delete(ctx context.Context, account, folder, id string) (*ChangeResult, error) {
	if err := m.needWrite("deleting a mail"); err != nil {
		return nil, err
	}
	return m.change(ctx, account, folder, id, "delete", func(s *probe.Session, f probe.Folder) (string, error) {
		return "", s.DeleteMailFrom(ctx, f.ID, id)
	})
}

// Move moves a mail to another folder.
func (m *Manager) Move(ctx context.Context, account, folder, id, dest string) (*ChangeResult, error) {
	if err := m.needWrite("moving a mail"); err != nil {
		return nil, err
	}
	var to probe.Folder
	res, err := m.change(ctx, account, folder, id, "move", func(s *probe.Session, f probe.Folder) (string, error) {
		var err error
		if to, err = folderOf(s, dest); err != nil {
			return "", err
		}
		return s.MoveMail(ctx, f.ID, id, to.ID)
	})
	if res != nil {
		res.To = to.ID
	}
	return res, err
}

// change runs an operation on one mail. The mail has to be known to the sync state of its folder for the server to accept
// a change, so a folder that this device has not read yet is read once first.
func (m *Manager) change(ctx context.Context, account, folder, id, action string, do func(s *probe.Session, f probe.Folder) (string, error)) (*ChangeResult, error) {
	var out *ChangeResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderForMail(s, folder, id)
		if err != nil {
			return err
		}
		if id == "" {
			return newError(http.StatusBadRequest, "id_required", "the id of the mail is required")
		}
		newID, err := do(s, f)
		if err != nil {
			return mailError(err, id)
		}
		out = &ChangeResult{Account: a.Name, Action: action, Folder: f.ID, ID: id, NewID: newID, OK: true}
		return nil
	})
	return out, err
}

// Reply replies to a mail with the new text; the server adds the original. It needs the switch for sending.
func (m *Manager) Reply(ctx context.Context, account, folder, id string, r ReplyRequest) (*SendResult, error) {
	if err := m.needSend(); err != nil {
		return nil, err
	}
	if err := m.checkReplyRecipients(ctx, account, folder, id, r.All); err != nil {
		return nil, err
	}
	return m.smart(ctx, account, folder, id, "reply", func(s *probe.Session, f probe.Folder) (*probe.SmartResult, error) {
		return s.ReplyMail(ctx, f.ID, id, probe.ReplyOptions{All: r.All, Body: r.Body, SkipSaveInSent: r.SaveInSent != nil && !*r.SaveInSent})
	})
}

// Forward forwards a mail to the recipients with the new text; the server adds the original. It needs the switch for
// sending.
func (m *Manager) Forward(ctx context.Context, account, folder, id string, r ForwardRequest) (*SendResult, error) {
	if err := m.needSend(); err != nil {
		return nil, err
	}
	if len(r.To) == 0 {
		return nil, newError(http.StatusBadRequest, "recipient_required", "at least one recipient (to) is required")
	}
	if err := m.checkRecipients(r.To, r.Cc); err != nil {
		return nil, err
	}
	return m.smart(ctx, account, folder, id, "forward", func(s *probe.Session, f probe.Folder) (*probe.SmartResult, error) {
		return s.ForwardMail(ctx, f.ID, id, probe.ForwardOptions{To: r.To, Cc: r.Cc, Body: r.Body, SkipSaveInSent: r.SaveInSent != nil && !*r.SaveInSent})
	})
}

func (m *Manager) smart(ctx context.Context, account, folder, id, action string, do func(s *probe.Session, f probe.Folder) (*probe.SmartResult, error)) (*SendResult, error) {
	var out *SendResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderForMail(s, folder, id)
		if err != nil {
			return err
		}
		if id == "" {
			return newError(http.StatusBadRequest, "id_required", "the id of the mail is required")
		}
		res, err := do(s, f)
		if err != nil {
			return mailError(err, id)
		}
		out = &SendResult{Account: a.Name, Action: action, Source: &MailRef{Folder: f.ID, ID: id}, From: s.SelfAddress(),
			To: res.To, Cc: res.Cc, Subject: res.Subject, Sent: true, Time: stamp(time.Now())}
		return nil
	})
	return out, err
}

// folderForMail finds the folder of a mail: the one that is given, or else the one that the id says. A mail id of Traveler
// (and of Exchange) starts with the id of its folder and a colon, "4:10207"; that is a habit of the servers and not part of
// the protocol, so the id counts only when the part before the colon is a folder of this mailbox.
func folderForMail(s *probe.Session, folder, id string) (probe.Folder, error) {
	if id == "" {
		return probe.Folder{}, newError(http.StatusBadRequest, "id_required", "the id of the mail is required")
	}
	if folder != "" {
		return folderOf(s, folder)
	}
	if prefix, _, ok := strings.Cut(id, ":"); ok {
		for _, f := range s.Folders() {
			if f.ID == prefix {
				return f, nil
			}
		}
	}
	return probe.Folder{}, newError(http.StatusBadRequest, "folder_required",
		fmt.Sprintf("the id %q does not tell its folder: give the folder (an id, or inbox, sent, ...)", id))
}

// EML returns the whole message of a mail, as the bytes of a mail file (.eml).
func (m *Manager) EML(ctx context.Context, account, folder, id string) (*Raw, error) {
	var out *Raw
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderForMail(s, folder, id)
		if err != nil {
			return err
		}
		data, err := s.FetchMIME(ctx, f.ID, id)
		if err != nil {
			return mailError(err, id)
		}
		out = &Raw{Data: data, ContentType: "message/rfc822", Filename: fileName(f.ID, id) + ".eml"}
		return nil
	})
	return out, err
}

// fileName makes a name for a file from the folder and the id of a mail: letters, digits, dots and dashes only.
func fileName(folder, id string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
				return r
			}
			return '_'
		}, s)
	}
	if prefix, _, ok := strings.Cut(id, ":"); ok && prefix == folder {
		return clean(id)
	}
	return clean(folder + "-" + id)
}

// zoneOf is the time zone of an account: its own, else the one of the options, else the one of the machine.
func zoneOf(account, global string) (probe.Zone, error) {
	name := account
	if name == "" {
		name = global
	}
	if name == "" || strings.EqualFold(name, "local") {
		return probe.LocalZone(), nil
	}
	return probe.LoadZone(name)
}

// TimeZone is the name of the time zone that an account uses ("" with several accounts and no name).
func (m *Manager) TimeZone(account string) string {
	e, err := m.resolve(account)
	if err != nil {
		return ""
	}
	return e.zone.Name
}

func (m *Manager) zoneName(a Account) string {
	if e, ok := m.entries[a.Name]; ok {
		return e.zone.Name
	}
	return ""
}
