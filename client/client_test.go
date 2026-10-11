package client_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// setup starts a mock mailbox and a manager with the accounts "bob" and, when two is set, "carol".
func setup(t *testing.T, write bool, accounts ...string) (*easmockserver.Server, *client.Manager) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	var list []client.Account
	for _, n := range accounts {
		list = append(list, client.Account{Name: n, Server: ts.URL, Email: n + "@example.com", User: n, Password: easmockserver.Password})
	}
	m, err := client.NewManager(list, client.Options{AllowWrite: write, AllowSend: write})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return srv, m
}

func code(t *testing.T, err error) string {
	t.Helper()
	var e *client.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a client.Error", err)
	}
	return e.Code
}

func TestAccountsAreValidatedAndDoNotShowPasswords(t *testing.T) {
	good := client.Account{Name: "a", Server: "x", User: "u", Password: "secret"}
	for name, a := range map[string]client.Account{
		"no name":     {Server: "x", User: "u", Password: "p"},
		"bad name":    {Name: "a b", Server: "x", User: "u", Password: "p"},
		"no server":   {Name: "a", User: "u", Password: "p"},
		"no user":     {Name: "a", Server: "x", Password: "p"},
		"no password": {Name: "a", Server: "x", User: "u"},
	} {
		if _, err := client.NewManager([]client.Account{a}, client.Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := client.NewManager([]client.Account{good, good}, client.Options{}); err == nil {
		t.Error("a duplicate name was accepted")
	}

	m, _ := client.NewManager([]client.Account{good}, client.Options{})
	var buf bytes.Buffer
	if err := client.Encode(&buf, m.Accounts()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Errorf("the accounts show the password: %s", buf.String())
	}
}

func TestOneAccountNeedsNoName_SeveralDo(t *testing.T) {
	_, one := setup(t, false, "bob")
	if _, err := one.Folders(t.Context(), ""); err != nil {
		t.Errorf("one account, no name: %v", err)
	}

	_, two := setup(t, false, "bob", "carol")
	_, err := two.Folders(t.Context(), "")
	if code(t, err) != "account_required" {
		t.Errorf("two accounts, no name: %v", err)
	}
	_, err = two.Folders(t.Context(), "nobody")
	if code(t, err) != "unknown_account" {
		t.Errorf("unknown account: %v", err)
	}
}

func TestInfoAndFolders(t *testing.T) {
	_, m := setup(t, false, "bob")
	info, err := m.Info(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if info.Email != "bob@example.com" || !info.Device.Provisioned || info.Folders != 6 || info.Server.ProtocolVersion == "" {
		t.Errorf("info = %+v", info)
	}
	f, err := m.Folders(t.Context(), "bob")
	if err != nil || len(f.Folders) != 6 {
		t.Fatalf("folders = %+v, %v", f, err)
	}
}

func TestListGetAndTheErrorsOfFolderAndMail(t *testing.T) {
	srv, m := setup(t, false, "bob")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "hi", From: "a@example.com", Body: strings.Repeat("z", 3000), At: time.Now()})

	l, err := m.List(t.Context(), "bob", "inbox", client.ListParams{})
	if err != nil || l.Count != 1 || l.Folder.Type != "inbox" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if l.Messages[0].Body != "" || l.Messages[0].Date == nil || l.Messages[0].Importance != "normal" {
		t.Errorf("list entry = %+v: want no body without a preview, a date, importance normal", l.Messages[0])
	}
	p, _ := m.List(t.Context(), "bob", "inbox", client.ListParams{Preview: 40})
	if len(p.Messages[0].Body) != 40 || !p.Messages[0].BodyTruncated {
		t.Errorf("preview = %d bytes, truncated %v", len(p.Messages[0].Body), p.Messages[0].BodyTruncated)
	}

	g, err := m.Get(t.Context(), "bob", "inbox", id)
	if err != nil || len(g.Message.Body) != 3000 || g.Message.BodyTruncated {
		t.Fatalf("get = %+v, %v", g, err)
	}

	_, err = m.Get(t.Context(), "bob", "inbox", "5:99999")
	if code(t, err) != "mail_not_found" {
		t.Errorf("unknown mail: %v", err)
	}
	_, err = m.List(t.Context(), "bob", "nothing", client.ListParams{})
	if code(t, err) != "folder_not_found" {
		t.Errorf("unknown folder: %v", err)
	}
	_, err = m.List(t.Context(), "bob", "", client.ListParams{})
	if code(t, err) != "folder_required" {
		t.Errorf("no folder: %v", err)
	}
	// the session survives a request that was wrong
	if _, err := m.List(t.Context(), "bob", "inbox", client.ListParams{}); err != nil {
		t.Errorf("after the errors: %v", err)
	}
}

func TestWritesAreRefusedUnlessAllowed(t *testing.T) {
	srv, m := setup(t, false, "bob")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "keep"})

	checks := map[string]func() error{
		"send": func() error {
			_, e := m.Send(t.Context(), "bob", client.SendRequest{To: []string{"x@example.com"}, Subject: "s"})
			return e
		},
		"read":   func() error { _, e := m.SetRead(t.Context(), "bob", "inbox", id, true); return e },
		"move":   func() error { _, e := m.Move(t.Context(), "bob", "inbox", id, "archive"); return e },
		"delete": func() error { _, e := m.Delete(t.Context(), "bob", "inbox", id); return e },
	}
	want := map[string]string{"send": "send_disabled", "read": "write_disabled", "move": "write_disabled", "delete": "write_disabled"}
	for name, f := range checks {
		err := f()
		var ce *client.Error
		if !errors.As(err, &ce) || ce.Code != want[name] || ce.Status != http.StatusForbidden {
			t.Errorf("%s: %v, want %s", name, err, want[name])
		}
	}
	if n := len(srv.Messages("bob", "5")); n != 1 {
		t.Errorf("a refused write changed the mailbox: %d mails in the Inbox", n)
	}
	if srv.SentCount() != 0 {
		t.Error("a refused send reached the server")
	}
}

func TestSendReadMoveDelete(t *testing.T) {
	srv, m := setup(t, true, "bob", "carol")
	if _, err := m.Info(t.Context(), "carol"); err != nil { // carol has logged in, so she is a recipient of the mock
		t.Fatal(err)
	}

	s, err := m.Send(t.Context(), "bob", client.SendRequest{To: []string{"carol@example.com"}, Subject: "hello", Body: "the text"})
	if err != nil || !s.Sent || s.From != "bob@example.com" {
		t.Fatalf("send = %+v, %v", s, err)
	}
	l, err := m.List(t.Context(), "carol", "inbox", client.ListParams{})
	if err != nil || l.Count != 1 || l.Messages[0].Subject != "hello" || l.Messages[0].Read {
		t.Fatalf("carol's Inbox = %+v, %v", l, err)
	}
	id := l.Messages[0].ID

	r, err := m.SetRead(t.Context(), "carol", "inbox", id, true)
	if err != nil || !r.OK || r.Action != "read" || !srv.Messages("carol", "5")[0].Read {
		t.Fatalf("read = %+v, %v", r, err)
	}

	mv, err := m.Move(t.Context(), "carol", "inbox", id, "archive")
	if err != nil || mv.NewID == "" || mv.To != "9" || len(srv.Messages("carol", "9")) != 1 {
		t.Fatalf("move = %+v, %v", mv, err)
	}

	d, err := m.Delete(t.Context(), "carol", "archive", mv.NewID)
	if err != nil || !d.OK || len(srv.Messages("carol", "7")) != 1 {
		t.Fatalf("delete = %+v, %v", d, err)
	}

	_, err = m.Send(t.Context(), "bob", client.SendRequest{Subject: "s"})
	if code(t, err) != "recipient_required" {
		t.Errorf("send without recipient: %v", err)
	}
	_, err = m.Send(t.Context(), "bob", client.SendRequest{To: []string{"a@b.c"}})
	if code(t, err) != "subject_required" {
		t.Errorf("send without subject: %v", err)
	}
}

func TestSendWithCcAndNoCopyInSent(t *testing.T) {
	srv, m := setup(t, true, "bob", "carol", "dave")
	for _, n := range []string{"carol", "dave"} {
		if _, err := m.Info(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	no := false
	_, err := m.Send(t.Context(), "bob", client.SendRequest{To: []string{"carol@example.com"}, Cc: []string{"dave@example.com"},
		Subject: "cc", Body: "b", SaveInSent: &no})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Messages("carol", "5")) != 1 || len(srv.Messages("dave", "5")) != 1 {
		t.Error("To and Cc did not both get the mail")
	}
	if n := len(srv.Messages("bob", "6")); n != 0 {
		t.Errorf("%d copies in Sent Items although none was asked for", n)
	}
}

// The command line prints what the REST API answers: both call Encode on the same value, and the JSON of a result has the
// same keys whatever the way it got out.
func TestResultsAndErrorsAreJSON(t *testing.T) {
	srv, m := setup(t, false, "bob")
	srv.Seed("bob", easmockserver.Mail{Subject: "j"})
	l, err := m.List(t.Context(), "bob", "inbox", client.ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := client.Encode(&buf, l); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"account", "folder", "count", "messages"} {
		if _, ok := back[k]; !ok {
			t.Errorf("key %q is missing: %s", k, buf.String())
		}
	}

	_, err = m.List(t.Context(), "bob", "nothing", client.ListParams{})
	buf.Reset()
	if err := client.Encode(&buf, client.ErrorBody{Error: client.AsError(err)}); err != nil {
		t.Fatal(err)
	}
	want := "\"code\": \"folder_not_found\""
	if !strings.Contains(buf.String(), want) {
		t.Errorf("error body = %s, want %s in it", buf.String(), want)
	}
}

// Sending and changing the mailbox are two switches: one does not give the other.
func TestSendAndWriteAreSeparateSwitches(t *testing.T) {
	for _, c := range []struct {
		name        string
		write, send bool
	}{{"write only", true, false}, {"send only", false, true}} {
		srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
		ts := httptest.NewServer(srv)
		defer ts.Close()
		m, err := client.NewManager([]client.Account{{Name: "bob", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}},
			client.Options{AllowWrite: c.write, AllowSend: c.send})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		id := srv.Seed("bob", easmockserver.Mail{Subject: "x"})

		_, sendErr := m.Send(t.Context(), "bob", client.SendRequest{To: []string{"bob@example.com"}, Subject: "s", Body: "b"})
		_, delErr := m.Delete(t.Context(), "bob", "inbox", id)
		if c.send && sendErr != nil || !c.send && code(t, sendErr) != "send_disabled" {
			t.Errorf("%s: send = %v", c.name, sendErr)
		}
		if c.write && delErr != nil || !c.write && code(t, delErr) != "write_disabled" {
			t.Errorf("%s: delete = %v", c.name, delErr)
		}
		if a := m.Accounts(); a.AllowWrite != c.write || a.AllowSend != c.send {
			t.Errorf("%s: accounts say write %v, send %v", c.name, a.AllowWrite, a.AllowSend)
		}
	}
}

func TestListShowsTheDateWindowAndTakesAnother(t *testing.T) {
	srv, m := setup(t, false, "bob")
	srv.Seed("bob", easmockserver.Mail{Subject: "old", At: time.Now().Add(-60 * 24 * time.Hour)})

	l, err := m.List(t.Context(), "bob", "inbox", client.ListParams{})
	if err != nil || l.Count != 0 || l.Since != "2w" {
		t.Fatalf("default window: %+v, %v; want no mails and since 2w", l, err)
	}
	l, err = m.List(t.Context(), "bob", "inbox", client.ListParams{Since: "3m"})
	if err != nil || l.Count != 1 || l.Since != "3m" {
		t.Fatalf("3m: %+v, %v", l, err)
	}
	_, err = m.List(t.Context(), "bob", "inbox", client.ListParams{Since: "forever"})
	if code(t, err) != "bad_request" {
		t.Errorf("bad window: %v", err)
	}
}

func TestReplyAndForward(t *testing.T) {
	srv, m := setup(t, true, "bob", "carol", "dave")
	for _, n := range []string{"carol", "dave"} {
		if _, err := m.Info(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	id := srv.Seed("bob", easmockserver.Mail{Subject: "plan", From: "Carol <carol@example.com>", To: "bob@example.com", Body: "the plan"})

	r, err := m.Reply(t.Context(), "bob", "inbox", id, client.ReplyRequest{Body: "agreed"})
	if err != nil || r.Action != "reply" || r.Subject != "Re: plan" || r.Source == nil || r.Source.ID != id || r.Source.Folder != "5" {
		t.Fatalf("reply = %+v, %v", r, err)
	}
	if got := srv.Messages("carol", "5"); len(got) != 1 || !strings.Contains(got[0].Body, "agreed") {
		t.Errorf("carol's Inbox = %+v", got)
	}

	f, err := m.Forward(t.Context(), "bob", "inbox", id, client.ForwardRequest{To: []string{"dave@example.com"}, Body: "FYI"})
	if err != nil || f.Action != "forward" || f.Subject != "Fw: plan" {
		t.Fatalf("forward = %+v, %v", f, err)
	}
	if got := srv.Messages("dave", "5"); len(got) != 1 || !strings.Contains(got[0].Body, "the plan") {
		t.Errorf("dave's Inbox = %+v", got)
	}

	if _, err := m.Forward(t.Context(), "bob", "inbox", id, client.ForwardRequest{Body: "x"}); code(t, err) != "recipient_required" {
		t.Errorf("forward without recipient: %v", err)
	}
	noSender := srv.Seed("bob", easmockserver.Mail{Subject: "anonymous"})
	if _, err := m.Reply(t.Context(), "bob", "inbox", noSender, client.ReplyRequest{Body: "x"}); code(t, err) != "no_sender" {
		t.Errorf("reply to a mail without a sender: %v", err)
	}
	if _, err := m.Reply(t.Context(), "bob", "inbox", "5:99999", client.ReplyRequest{Body: "x"}); code(t, err) != "mail_not_found" {
		t.Errorf("reply to a mail that is not there: %v", err)
	}
	if _, err := m.List(t.Context(), "bob", "inbox", client.ListParams{}); err != nil {
		t.Errorf("the session did not survive the wrong requests: %v", err)
	}
}

func TestReplyAndForwardNeedTheSendSwitch(t *testing.T) {
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	m, err := client.NewManager([]client.Account{{Name: "bob", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}},
		client.Options{AllowWrite: true}) // write, but no send
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id := srv.Seed("bob", easmockserver.Mail{Subject: "s", From: "carol@example.com"})
	if _, err := m.Reply(t.Context(), "bob", "inbox", id, client.ReplyRequest{Body: "x"}); code(t, err) != "send_disabled" {
		t.Errorf("reply: %v", err)
	}
	if _, err := m.Forward(t.Context(), "bob", "inbox", id, client.ForwardRequest{To: []string{"a@b.c"}}); code(t, err) != "send_disabled" {
		t.Errorf("forward: %v", err)
	}
	if srv.SentCount() != 0 {
		t.Error("a refused reply reached the server")
	}
}

// A mail id of Traveler starts with the id of its folder, so the folder can be left out.
func TestTheFolderCanBeLeftOutWhenTheIdSaysIt(t *testing.T) {
	srv, m := setup(t, true, "bob", "carol")
	if _, err := m.Info(t.Context(), "carol"); err != nil {
		t.Fatal(err)
	}
	id := srv.Seed("bob", easmockserver.Mail{Subject: "x", From: "carol@example.com", Body: "text"})

	if g, err := m.Get(t.Context(), "bob", "", id); err != nil || g.Message.Folder != "5" || g.Message.Subject != "x" {
		t.Fatalf("get = %+v, %v", g, err)
	}
	if r, err := m.SetRead(t.Context(), "bob", "", id, true); err != nil || r.Folder != "5" || !srv.Messages("bob", "5")[0].Read {
		t.Fatalf("read = %+v, %v", r, err)
	}
	if s, err := m.Reply(t.Context(), "bob", "", id, client.ReplyRequest{Body: "ok"}); err != nil || s.Source.Folder != "5" {
		t.Fatalf("reply = %+v, %v", s, err)
	}
	mv, err := m.Move(t.Context(), "bob", "", id, "archive")
	if err != nil || mv.Folder != "5" || mv.To != "9" {
		t.Fatalf("move = %+v, %v", mv, err)
	}
	if _, err := m.Delete(t.Context(), "bob", "", mv.NewID); err != nil { // the new id says folder 9
		t.Fatalf("delete = %v", err)
	}
	if n := len(srv.Messages("bob", "7")); n != 1 {
		t.Errorf("%d mails in Deleted Items, want 1", n)
	}

	for _, bad := range []string{"nocolon", "999:12", ":5", "x:1"} {
		if _, err := m.Get(t.Context(), "bob", "", bad); code(t, err) != "folder_required" {
			t.Errorf("id %q: %v, want folder_required", bad, err)
		}
	}
	if _, err := m.Get(t.Context(), "bob", "", ""); code(t, err) != "id_required" {
		t.Error("an empty id was not refused")
	}
	// a folder that is given wins over the id
	if _, err := m.Get(t.Context(), "bob", "sent", id); code(t, err) != "mail_not_found" {
		t.Errorf("the id in the wrong folder: %v", err)
	}
}

func TestEMLIsTheRawMessage(t *testing.T) {
	srv, m := setup(t, false, "bob") // reading only
	id := srv.Seed("bob", easmockserver.Mail{Subject: "raw", From: "carol@example.com", Body: "stream"})

	raw, err := m.EML(t.Context(), "bob", "", id) // the folder from the id
	if err != nil || !strings.Contains(string(raw.Data), "Subject: raw") || !strings.HasSuffix(string(raw.Data), "stream") {
		t.Fatalf("eml = %q, %v", raw, err)
	}
	if raw.ContentType != "message/rfc822" || raw.Filename == "" {
		t.Errorf("content type %q, file name %q", raw.ContentType, raw.Filename)
	}
	if _, err := m.EML(t.Context(), "bob", "inbox", "5:99999"); code(t, err) != "mail_not_found" {
		t.Errorf("unknown mail: %v", err)
	}
}

func TestCalendarListAndGet(t *testing.T) {
	srv, m := setup(t, false, "bob")
	now := time.Now().Truncate(time.Second)
	idPlan := srv.SeedEvent("bob", easmockserver.Event{UID: "u1", Subject: "Planning", Location: "Room 4", Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour),
		Busy: 2, Reminder: 10, Meeting: 1, Organizer: "carol@example.com", OrganizerName: "Carol",
		Attendees: []easmockserver.Attendee{{Name: "Bob", Email: "bob@example.com", Status: 3, Type: 1}}})
	srv.SeedEvent("bob", easmockserver.Event{Subject: "weekly", Start: now.Add(-30 * 24 * time.Hour), End: now.Add(-30*24*time.Hour + time.Hour),
		Repeat: &easmockserver.Repeat{Type: 1, Interval: 1, DayOfWeek: 2 | 16}}) // forever
	srv.SeedEvent("bob", easmockserver.Event{Subject: "past", Start: now.Add(-5 * 24 * time.Hour), End: now.Add(-5*24*time.Hour + time.Hour)})
	srv.SeedEvent("bob", easmockserver.Event{Subject: "far", Start: now.Add(40 * 24 * time.Hour), End: now.Add(41 * 24 * time.Hour)})

	l, err := m.Events(t.Context(), "bob", "", client.EventParams{})
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, e := range l.Events {
		subjects = append(subjects, e.Subject)
	}
	if got := strings.Join(subjects, ","); got != "weekly,Planning,far" { // the past one is out, the repeating one is in, by start time
		t.Errorf("events = %s", got)
	}
	if l.Folder.Type != "calendar" || l.Since != "2w" || l.From == nil || l.To != nil {
		t.Errorf("window: folder %+v, since %q, from %v, to %v", l.Folder, l.Since, l.From, l.To)
	}

	p := l.Events[1]
	if p.Busy != "busy" || p.Meeting != "meeting" || p.ReminderMinutes != 10 || p.Organizer == nil || p.Organizer.Email != "carol@example.com" ||
		len(p.Attendees) != 1 || p.Attendees[0].Status != "accepted" || p.Attendees[0].Type != "required" || p.Recurring {
		t.Errorf("event = %+v", p)
	}
	if w := l.Events[0]; !w.Recurring || w.Repeat == nil || w.Repeat.Type != "weekly" || strings.Join(w.Repeat.DaysOfWeek, ",") != "monday,thursday" {
		t.Errorf("repeating event = %+v", w)
	}

	// a window with an end, and one that reaches into the past
	to := now.Add(48 * time.Hour).Format(time.RFC3339)
	if l, _ := m.Events(t.Context(), "bob", "", client.EventParams{To: to}); l == nil || l.Count != 2 {
		t.Errorf("to %s: %+v", to, l)
	}
	back := now.Add(-10 * 24 * time.Hour).Format("2006-01-02")
	if l, _ := m.Events(t.Context(), "bob", "", client.EventParams{From: back, Limit: -1}); l == nil || l.Count != 4 || l.Since != "2w" {
		t.Errorf("from %s: %+v", back, l)
	}
	if l, _ := m.Events(t.Context(), "bob", "", client.EventParams{Limit: 1}); l == nil || l.Count != 1 {
		t.Errorf("limit 1: %+v", l)
	}

	g, err := m.Event(t.Context(), "bob", "", idPlan) // the folder from the id
	if err != nil || g.Event.Subject != "Planning" || g.Event.UID != "u1" || g.Event.Location != "Room 4" {
		t.Fatalf("get = %+v, %v", g, err)
	}
	if _, err := m.Event(t.Context(), "bob", "calendar", "8:99999"); code(t, err) != "event_not_found" {
		t.Errorf("unknown event: %v", err)
	}
	for _, bad := range []client.EventParams{{From: "yesterday"}, {To: "soon"}, {Since: "forever"}, {From: "2026-10-10", To: "2026-10-01"}} {
		if _, err := m.Events(t.Context(), "bob", "", bad); code(t, err) != "bad_request" {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestICSIsTheEventAsICalendar(t *testing.T) {
	srv, m := setup(t, false, "bob") // reading only
	now := time.Now().Truncate(time.Second)
	id := srv.SeedEvent("bob", easmockserver.Event{UID: "u-ics", Subject: "Review, final", Location: "Room 2", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour),
		Organizer: "carol@example.com", OrganizerName: "Carol", Reminder: 5,
		Attendees: []easmockserver.Attendee{{Name: "Bob", Email: "bob@example.com", Status: 3, Type: 1}}})

	raw, err := m.ICS(t.Context(), "bob", "", id) // the folder from the id
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw.Data)
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "UID:u-ics\r\n", `SUMMARY:Review\, final` + "\r\n", "LOCATION:Room 2\r\n",
		"ORGANIZER;CN=Carol:mailto:carol@example.com\r\n", "TRIGGER:-PT5M\r\n", "END:VCALENDAR\r\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the calendar lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(raw.ContentType, "text/calendar") || !strings.HasSuffix(raw.Filename, ".ics") {
		t.Errorf("content type %q, file name %q", raw.ContentType, raw.Filename)
	}
	if _, err := m.ICS(t.Context(), "bob", "calendar", "8:99999"); code(t, err) != "event_not_found" {
		t.Errorf("unknown event: %v", err)
	}
}
