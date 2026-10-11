package probe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hstern/go-activesync/eas"
	"github.com/hstern/go-activesync/wbxml"
)

// This file is what a mail client needs of a Session: the folders, a mail list, one mail with its body, and the
// changes read, move and delete. The probe, the load worker and the mail monitor do not use it.

// Folder is a folder of the mailbox.
type Folder struct {
	ID     string
	Parent string
	Name   string
	Type   string // inbox, drafts, deleted, sent, outbox, calendar, contacts, tasks, notes, journal, mail, other
}

// Message is a mail as a client shows it.
type Message struct {
	ID             string
	Folder         string
	Subject        string
	From           string
	To             string
	Cc             string
	Date           time.Time // when the server received it; zero when it did not say
	Read           bool
	Flagged        bool
	Importance     int // 0 low, 1 normal, 2 high
	HasAttachments bool
	Body           string // plain text; in a list it is cut to a preview
	BodySize       int    // the size of the whole body in bytes, as the server estimates it
	BodyTruncated  bool
}

// folderKind names the type of a folder for the output and for finding folders by type.
func folderKind(t eas.FolderType) string {
	switch t {
	case eas.FolderTypeInbox:
		return "inbox"
	case eas.FolderTypeDrafts:
		return "drafts"
	case eas.FolderTypeDeletedItems:
		return "deleted"
	case eas.FolderTypeSentItems:
		return "sent"
	case eas.FolderTypeOutbox:
		return "outbox"
	case eas.FolderTypeCalendar, eas.FolderTypeUserCalendar:
		return "calendar"
	case eas.FolderTypeContacts, eas.FolderTypeUserContacts:
		return "contacts"
	case eas.FolderTypeTasks, eas.FolderTypeUserTasks:
		return "tasks"
	case eas.FolderTypeNotes, eas.FolderTypeUserNotes:
		return "notes"
	case eas.FolderTypeJournal, eas.FolderTypeUserJournal:
		return "journal"
	case eas.FolderTypeUserMail:
		return "mail"
	}
	return "other"
}

// Folders returns the folders the session knows (after FolderSync), sorted by name.
func (s *Session) Folders() []Folder {
	d := s.state
	d.mu.Lock()
	out := make([]Folder, 0, len(d.data.Folders))
	for id, f := range d.data.Folders {
		out = append(out, Folder{ID: id, Parent: f.ParentID, Name: f.Name, Type: folderKind(eas.FolderType(f.Type))})
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// FindFolder finds a folder by its id, by a type name (inbox, sent, drafts, deleted, outbox, calendar, contacts, tasks,
// notes) or by its display name, without regard to case. A name that two folders have is an error.
func (s *Session) FindFolder(ref string) (Folder, error) {
	all := s.Folders()
	for _, f := range all {
		if f.ID == ref {
			return f, nil
		}
	}
	want := strings.ToLower(strings.TrimSpace(ref))
	for _, f := range all {
		if f.Type == want && f.Type != "other" && f.Type != "mail" {
			return f, nil
		}
	}
	var hits []Folder
	for _, f := range all {
		if strings.ToLower(f.Name) == want {
			hits = append(hits, f)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Folder{}, fmt.Errorf("no folder %q", ref)
	}
	ids := make([]string, len(hits))
	for i, f := range hits {
		ids[i] = f.ID
	}
	return Folder{}, fmt.Errorf("%d folders are called %q (ids %s): use the id", len(hits), ref, strings.Join(ids, ", "))
}

func messageOf(folder string, m eas.EmailItem) Message {
	return Message{
		ID: m.ServerID, Folder: folder, Subject: m.Subject, From: m.From, To: m.To, Cc: m.Cc, Date: m.DateReceived,
		Read: m.Read, Flagged: m.Flagged(), Importance: m.Importance, HasAttachments: m.HasAttachments,
		Body: m.Body, BodySize: m.BodyEstimatedSize, BodyTruncated: m.BodyTruncated,
	}
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// sinceNames are the date windows a list can ask for. The library cannot ask for "all" (a filter of 0 is its default, two
// weeks), so 6 months is the widest.
var sinceNames = []struct {
	name   string
	filter eas.FilterType
}{{"1d", eas.FilterOneDay}, {"3d", eas.FilterThreeDay}, {"1w", eas.FilterOneWeek}, {"2w", eas.FilterTwoWeek}, {"1m", eas.FilterOneMonth},
	{"3m", eas.FilterThreeMonth}, {"6m", eas.FilterSixMonth}}

// ParseSince turns 1d, 3d, 1w, 2w, 1m, 3m or 6m into a date window; an empty text is the default (two weeks).
func ParseSince(s string) (eas.FilterType, error) {
	if s == "" {
		return eas.FilterTwoWeek, nil
	}
	for _, n := range sinceNames {
		if n.name == s {
			return n.filter, nil
		}
	}
	return 0, fmt.Errorf("since %q: use 1d, 3d, 1w, 2w, 1m, 3m or 6m", s)
}

// SinceName is the name of a date window; 0 (the default) is two weeks.
func SinceName(f eas.FilterType) string {
	if f == 0 {
		f = eas.FilterTwoWeek
	}
	for _, n := range sinceNames {
		if n.filter == f {
			return n.name
		}
	}
	return ""
}

// listBodyBytes is the body size a list asks the server for, whatever preview the caller wants.
const listBodyBytes = 256

// ListOptions shape ListMail.
type ListOptions struct {
	// Limit is the number of mails returned, newest first; 0 means all that the server reports.
	Limit int
	// Preview is how many bytes of the body each mail carries; 0 means none.
	Preview int
	// Since is the date window of the list: the mails that arrived within it (see ParseSince). 0 is the default, two weeks.
	Since eas.FilterType
}

// ListMail reads a mail folder from the start and returns its mails, newest first. EAS has no listing: a folder is
// read with a Sync from key 0, which the server answers with every mail of its date window (two weeks). The sync key of
// the folder is reset first, so the answer does not depend on what this device has seen before.
func (s *Session) ListMail(ctx context.Context, folder string, o ListOptions) ([]Message, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	if err := s.state.SetSyncKey(ctx, folder, "0"); err != nil {
		return nil, err
	}
	// The server is asked for the same body size as the probe does (256 bytes) at least: a Traveler answered a Sync with a
	// body cut to 1 byte with an empty reply, which looks like an empty folder. The body is cut to the preview here.
	ask := max(o.Preview, listBodyBytes)
	prev := s.windowOverride
	s.windowOverride = mailSyncWindow
	defer func() { s.windowOverride = prev }()

	var out []Message
	for page := 0; page < mailMaxPages; page++ {
		res, err := s.client.SyncEmail(ctx, folder, eas.EmailSyncOptions{
			WindowSize: s.window(), BodyType: eas.BodyTypePlain, BodyTruncationSize: ask, DateFilter: o.Since,
		})
		if err != nil {
			return nil, err
		}
		for _, m := range res.Added {
			msg := messageOf(folder, m)
			if len(msg.Body) > o.Preview {
				msg.Body, msg.BodyTruncated = cutUTF8(msg.Body, o.Preview), true
			}
			out = append(out, msg)
		}
		if !res.MoreAvailable {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	if o.Limit > 0 && len(out) > o.Limit {
		out = out[:o.Limit]
	}
	return out, nil
}

// FetchMail reads one mail with its whole body.
func (s *Session) FetchMail(ctx context.Context, folder, id string) (*Message, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	item, err := s.client.FetchEmail(ctx, folder, id, eas.FetchEmailOptions{BodyType: eas.BodyTypePlain})
	if err != nil {
		return nil, err
	}
	m := messageOf(folder, *item)
	return &m, nil
}

// MarkRead sets or clears the read flag of a mail.
func (s *Session) MarkRead(ctx context.Context, folder, id string, read bool) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	s.rec.setStep(StepSync)
	value := "0"
	if read {
		value = "1"
	}
	change := wbxml.E(wbxml.PageAirSync, "Change",
		wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text(id)),
		wbxml.E(wbxml.PageAirSync, "ApplicationData", wbxml.E(wbxml.PageEmail, "Read", wbxml.Text(value))))
	return s.commandInFolder(ctx, folder, id, change)
}

// DeleteMailFrom deletes a mail of any folder. The server moves it to Deleted Items; a mail that is in Deleted Items
// already is gone for good.
func (s *Session) DeleteMailFrom(ctx context.Context, folder, id string) error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	s.rec.setStep(StepSync)
	return s.deleteViaSync(ctx, folder, id)
}

// commandInFolder sends a change to a mail. The folder needs a current sync key, so a folder that was not read yet is
// read first, and a key the server refuses (status 3) is rebuilt once, like deleteViaSync does.
func (s *Session) commandInFolder(ctx context.Context, folder, id string, command *wbxml.Element) error {
	err := s.syncCommandOnce(ctx, folder, id, command)
	if statusIs(err, "Sync", 3) {
		s.log.Warn("Sync status 3 while changing a mail: rebuilding the sync key of the folder and trying again")
		if serr := s.state.SetSyncKey(ctx, folder, "0"); serr != nil {
			return serr
		}
		if serr := s.bootstrapFolder(ctx, folder); serr != nil {
			return serr
		}
		s.rec.setStep(StepSync)
		err = s.syncCommandOnce(ctx, folder, id, command)
	}
	return err
}

// MoveMail moves a mail to another folder and returns its new id.
func (s *Session) MoveMail(ctx context.Context, folder, id, dest string) (string, error) {
	if err := s.ensureClient(); err != nil {
		return "", err
	}
	s.rec.setStep(StepSync)
	res, err := s.client.MoveItems(ctx, folder, dest, []string{id})
	if err != nil {
		return "", err
	}
	if len(res) == 0 {
		return "", errors.New("MoveItems: the server answered nothing")
	}
	// MS-ASCMD: 1 invalid source, 2 invalid destination, 3 success, 4 source and destination the same, 5 failed, 7 locked
	switch res[0].Status {
	case 3:
		return res[0].DstServerID, nil
	case 1:
		return "", fmt.Errorf("move: the server does not know the mail %s in folder %s", id, folder)
	case 2:
		return "", fmt.Errorf("move: the server does not know the folder %s", dest)
	case 4:
		return "", errors.New("move: the mail is in that folder already")
	}
	return "", fmt.Errorf("move: status %d", res[0].Status)
}

// FetchMIME reads the whole message of a mail as the server has it: the headers, the text and the parts, the bytes of a
// mail file (.eml). It asks the server for the body type MIME, which a server may not offer.
func (s *Session) FetchMIME(ctx context.Context, folder, id string) ([]byte, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	item, err := s.client.FetchEmail(ctx, folder, id, eas.FetchEmailOptions{BodyType: eas.BodyTypeMIME})
	if err != nil {
		return nil, err
	}
	switch {
	case len(item.BodyMIME) > 0:
		return item.BodyMIME, nil
	case item.BodyType == eas.BodyTypeMIME && item.Body != "": // a server that sends the MIME as text
		return []byte(item.Body), nil
	}
	return nil, fmt.Errorf("the server did not send the message as MIME (it sent body type %d)", item.BodyType)
}
