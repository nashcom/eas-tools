package probe_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

// mailbox starts a mock server in mailbox mode and a session that has opened the account.
func mailbox(t *testing.T) (*easmockserver.Server, *probe.Session) {
	srv, s, _ := mailboxAt(t)
	return srv, s
}

func mailboxAt(t *testing.T) (*easmockserver.Server, *probe.Session, string) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	s, err := probe.NewSession(probe.Config{ServerURL: ts.URL, Username: "bob", Email: "bob@example.com", Password: easmockserver.Password})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return srv, s, ts.URL
}

func TestFoldersAndFindFolder(t *testing.T) {
	_, s := mailbox(t)

	for ref, want := range map[string]string{"inbox": "5", "Sent": "6", "deleted": "7", "archive": "9", "5": "5", "CALENDAR": "8"} {
		f, err := s.FindFolder(ref)
		if err != nil || f.ID != want {
			t.Errorf("FindFolder(%q) = %+v, %v; want id %s", ref, f, err, want)
		}
	}
	if _, err := s.FindFolder("nothing"); err == nil {
		t.Error("an unknown folder was found")
	}
	if got := len(s.Folders()); got != 6 {
		t.Errorf("%d folders, want 6", got)
	}
}

func TestListMailNewestFirstWithLimitAndPreview(t *testing.T) {
	srv, s := mailbox(t)
	now := time.Now()
	for i, subj := range []string{"old", "middle", "new"} {
		srv.Seed("bob", easmockserver.Mail{Subject: subj, From: "a@example.com", To: "bob@example.com",
			Body: strings.Repeat("x", 100), At: now.Add(time.Duration(i) * time.Hour)})
	}

	list, err := s.ListMail(t.Context(), "5", probe.ListOptions{Preview: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Subject != "new" || list[2].Subject != "old" {
		t.Fatalf("list = %+v, want new, middle, old", list)
	}
	if len(list[0].Body) != 10 || !list[0].BodyTruncated || list[0].BodySize != 100 {
		t.Errorf("preview: body %d bytes, truncated %v, size %d; want 10, true, 100", len(list[0].Body), list[0].BodyTruncated, list[0].BodySize)
	}

	limited, err := s.ListMail(t.Context(), "5", probe.ListOptions{Limit: 2})
	if err != nil || len(limited) != 2 || limited[0].Subject != "new" {
		t.Fatalf("limited = %+v, %v", limited, err)
	}
}

func TestListMailReadsTheWholeFolderEveryTime(t *testing.T) {
	srv, s := mailbox(t)
	for i := 0; i < 12; i++ { // more than one page
		srv.Seed("bob", easmockserver.Mail{Subject: "m", At: time.Now()})
	}
	for round := 0; round < 2; round++ {
		list, err := s.ListMail(t.Context(), "5", probe.ListOptions{})
		if err != nil || len(list) != 12 {
			t.Fatalf("round %d: %d mails, %v; want 12 each time", round, len(list), err)
		}
	}
}

func TestFetchMailHasTheWholeBody(t *testing.T) {
	srv, s := mailbox(t)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "long", Body: strings.Repeat("y", 5000)})

	m, err := s.FetchMail(t.Context(), "5", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Body) != 5000 || m.BodyTruncated || m.Subject != "long" {
		t.Errorf("fetched %d bytes, truncated %v, subject %q", len(m.Body), m.BodyTruncated, m.Subject)
	}
	if _, err := s.FetchMail(t.Context(), "5", "5:999"); err == nil {
		t.Error("an unknown mail was fetched")
	}
}

func TestMarkReadAndUnread(t *testing.T) {
	srv, s := mailbox(t)
	srv.Seed("bob", easmockserver.Mail{Subject: "x"})
	list, err := s.ListMail(t.Context(), "5", probe.ListOptions{})
	if err != nil || len(list) != 1 || list[0].Read {
		t.Fatalf("list = %+v, %v", list, err)
	}

	if err := s.MarkRead(t.Context(), "5", list[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if got := srv.Messages("bob", "5"); !got[0].Read {
		t.Error("the mail is not read on the server")
	}
	if err := s.MarkRead(t.Context(), "5", list[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if got := srv.Messages("bob", "5"); got[0].Read {
		t.Error("the mail is still read on the server")
	}
}

func TestMoveMail(t *testing.T) {
	srv, s := mailbox(t)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "to archive"})
	if _, err := s.ListMail(t.Context(), "5", probe.ListOptions{}); err != nil {
		t.Fatal(err)
	}

	newID, err := s.MoveMail(t.Context(), "5", id, "9")
	if err != nil || newID == "" || newID == id {
		t.Fatalf("move = %q, %v", newID, err)
	}
	if n := len(srv.Messages("bob", "5")); n != 0 {
		t.Errorf("%d mails left in the Inbox", n)
	}
	if got := srv.Messages("bob", "9"); len(got) != 1 || got[0].Subject != "to archive" {
		t.Errorf("Archive = %+v", got)
	}
	if _, err := s.MoveMail(t.Context(), "5", id, "9"); err == nil {
		t.Error("moving a mail that is gone worked")
	}
	if _, err := s.MoveMail(t.Context(), "9", newID, "42"); err == nil {
		t.Error("moving to a folder that does not exist worked")
	}
}

func TestDeleteMovesToDeletedItemsAndThenRemoves(t *testing.T) {
	srv, s := mailbox(t)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "bye"})
	if _, err := s.ListMail(t.Context(), "5", probe.ListOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteMailFrom(t.Context(), "5", id); err != nil {
		t.Fatal(err)
	}
	deleted := srv.Messages("bob", "7")
	if len(srv.Messages("bob", "5")) != 0 || len(deleted) != 1 {
		t.Fatalf("Inbox %d, Deleted Items %d; want 0 and 1", len(srv.Messages("bob", "5")), len(deleted))
	}

	list, err := s.ListMail(t.Context(), "7", probe.ListOptions{})
	if err != nil || len(list) != 1 {
		t.Fatalf("Deleted Items list = %+v, %v", list, err)
	}
	if err := s.DeleteMailFrom(t.Context(), "7", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Messages("bob", "7")); n != 0 {
		t.Errorf("%d mails left in Deleted Items", n)
	}
}

func TestSendMailWithABodyDeliversAndKeepsACopy(t *testing.T) {
	srv, s, url := mailboxAt(t)
	// the recipient is a user of the mock when it has logged in
	r, err := probe.NewSession(probe.Config{ServerURL: url, Username: "carol", Password: easmockserver.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Options(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	err = s.SendMail(t.Context(), nil, probe.Mail{To: []string{"carol@example.com"}, Subject: "hello", Body: "the text"}, "t1")
	if err != nil {
		t.Fatal(err)
	}
	got := srv.Messages("carol", "5")
	if len(got) != 1 || got[0].Subject != "hello" || !strings.Contains(got[0].Body, "the text") {
		t.Errorf("carol's Inbox = %+v", got)
	}
	if sent := srv.Messages("bob", "6"); len(sent) != 1 {
		t.Errorf("%d copies in Sent Items, want 1", len(sent))
	}
}

// A Traveler answered the Sync of a list with an empty reply when the body was cut to 1 byte (suspected: the probe asks for
// 256 and works). A list without a body must not ask for a tiny body.
func TestListMailWorksWithAServerThatRefusesATinyBody(t *testing.T) {
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true, MinTruncation: 100})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	s, err := probe.NewSession(probe.Config{ServerURL: ts.URL, Username: "bob", Email: "bob@example.com", Password: easmockserver.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) }, func() error { return s.Provision(ctx, nil) }, func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	srv.Seed("bob", easmockserver.Mail{Subject: "there", Body: strings.Repeat("b", 500)})

	for _, preview := range []int{0, 10, 300} {
		list, err := s.ListMail(ctx, "5", probe.ListOptions{Preview: preview})
		if err != nil || len(list) != 1 {
			t.Fatalf("preview %d: %d mails, %v; want 1", preview, len(list), err)
		}
		if len(list[0].Body) != preview {
			t.Errorf("preview %d: body is %d bytes", preview, len(list[0].Body))
		}
		if preview < 500 && !list[0].BodyTruncated {
			t.Errorf("preview %d: the body is cut, and does not say so", preview)
		}
	}
}

func TestListPreviewDoesNotSplitACharacter(t *testing.T) {
	srv, s := mailbox(t)
	srv.Seed("bob", easmockserver.Mail{Subject: "u", Body: strings.Repeat("ä", 50)}) // 2 bytes each
	list, err := s.ListMail(t.Context(), "5", probe.ListOptions{Preview: 5})
	if err != nil || len(list) != 1 {
		t.Fatalf("%d mails, %v", len(list), err)
	}
	if list[0].Body != "ää" || !list[0].BodyTruncated {
		t.Errorf("preview = %q, want two whole characters", list[0].Body)
	}
}

func TestListMailAsksForADateWindow(t *testing.T) {
	srv, s := mailbox(t)
	now := time.Now()
	srv.Seed("bob", easmockserver.Mail{Subject: "today", At: now})
	srv.Seed("bob", easmockserver.Mail{Subject: "20 days", At: now.Add(-20 * 24 * time.Hour)})
	srv.Seed("bob", easmockserver.Mail{Subject: "100 days", At: now.Add(-100 * 24 * time.Hour)})

	for since, want := range map[string]int{"1d": 1, "": 1 /* the default is 2w */, "2w": 1, "1m": 2, "3m": 2, "6m": 3} {
		f, err := probe.ParseSince(since)
		if err != nil {
			t.Fatal(since, err)
		}
		list, err := s.ListMail(t.Context(), "5", probe.ListOptions{Since: f})
		if err != nil || len(list) != want {
			t.Errorf("since %q: %d mails, %v; want %d", since, len(list), err, want)
		}
	}
	if _, err := probe.ParseSince("yesterday"); err == nil {
		t.Error("an unknown window was accepted")
	}
	if probe.SinceName(0) != "2w" || probe.SinceName(5) != "1m" {
		t.Errorf("names: %q %q", probe.SinceName(0), probe.SinceName(5))
	}
}

// login makes a user known to the mock (a mail is only delivered to users that have logged in).
func login(t *testing.T, url string, users ...string) {
	t.Helper()
	for _, u := range users {
		s, err := probe.NewSession(probe.Config{ServerURL: url, Username: u, Email: u + "@example.com", Password: easmockserver.Password})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Options(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestReplyGoesToTheSenderWithReAndTheNewText(t *testing.T) {
	srv, s, url := mailboxAt(t)
	login(t, url, "carol", "dave")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "hello", From: "Carol <carol@example.com>",
		To: "bob@example.com, dave@example.com", Body: "the original"})

	res, err := s.ReplyMail(t.Context(), "5", id, probe.ReplyOptions{Body: "my answer"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "Re: hello" || len(res.To) != 1 || !strings.Contains(res.To[0], "carol@example.com") || len(res.Cc) != 0 {
		t.Errorf("result = %+v", res)
	}
	got := srv.Messages("carol", "5")
	if len(got) != 1 || got[0].Subject != "Re: hello" || !strings.Contains(got[0].Body, "my answer") {
		t.Fatalf("carol's Inbox = %+v", got)
	}
	if !strings.Contains(got[0].Body, "the original") {
		t.Error("the server did not add the original text") // what SmartReply is for: the client sends only the new text
	}
	if n := len(srv.Messages("dave", "5")); n != 0 {
		t.Errorf("dave got %d mails from a reply that was not to all", n)
	}
	if n := len(srv.Messages("bob", "6")); n != 1 {
		t.Errorf("%d copies in Sent Items, want 1", n)
	}
}

func TestReplyToAllLeavesOutTheAccountItself(t *testing.T) {
	srv, s, url := mailboxAt(t)
	login(t, url, "carol", "dave")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "Re: already", From: "carol@example.com",
		To: "bob@example.com; dave@example.com", Cc: "Bob <bob@example.com>"})

	res, err := s.ReplyMail(t.Context(), "5", id, probe.ReplyOptions{Body: "to all", All: true, SkipSaveInSent: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "Re: already" { // no second "Re:"
		t.Errorf("subject = %q", res.Subject)
	}
	all := strings.Join(append(res.To, res.Cc...), " ")
	if strings.Contains(all, "bob@example.com") || !strings.Contains(all, "carol@example.com") || !strings.Contains(all, "dave@example.com") {
		t.Errorf("recipients = %v / %v", res.To, res.Cc)
	}
	if len(srv.Messages("carol", "5")) != 1 || len(srv.Messages("dave", "5")) != 1 {
		t.Error("carol and dave did not both get the reply")
	}
	if n := len(srv.Messages("bob", "5")); n != 1 {
		t.Errorf("bob has %d mails in the Inbox: the reply came back to him", n)
	}
	if n := len(srv.Messages("bob", "6")); n != 0 {
		t.Errorf("%d copies in Sent Items although none was asked for", n)
	}
}

func TestReplyNeedsAnExistingMailWithASender(t *testing.T) {
	srv, s := mailbox(t)
	if _, err := s.ReplyMail(t.Context(), "5", "5:99999", probe.ReplyOptions{Body: "x"}); err == nil {
		t.Error("a reply to a mail that does not exist was sent")
	}
	id := srv.Seed("bob", easmockserver.Mail{Subject: "no sender"})
	if _, err := s.ReplyMail(t.Context(), "5", id, probe.ReplyOptions{Body: "x"}); err == nil {
		t.Error("a reply to a mail without a sender was sent")
	}
	if srv.SentCount() != 0 {
		t.Error("a failed reply reached the server")
	}
}

func TestForwardSendsToTheGivenAddressesWithFwAndTheOriginal(t *testing.T) {
	srv, s, url := mailboxAt(t)
	login(t, url, "carol", "dave")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "report", From: "carol@example.com", To: "bob@example.com", Body: "the numbers"})

	res, err := s.ForwardMail(t.Context(), "5", id, probe.ForwardOptions{To: []string{"dave@example.com"}, Body: "FYI"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject != "Fw: report" || len(res.To) != 1 || res.To[0] != "dave@example.com" {
		t.Errorf("result = %+v", res)
	}
	got := srv.Messages("dave", "5")
	if len(got) != 1 || got[0].Subject != "Fw: report" || !strings.Contains(got[0].Body, "FYI") || !strings.Contains(got[0].Body, "the numbers") {
		t.Fatalf("dave's Inbox = %+v", got)
	}
	if n := len(srv.Messages("carol", "5")); n != 0 {
		t.Errorf("the sender of the original got %d mails from a forward", n)
	}

	if _, err := s.ForwardMail(t.Context(), "5", id, probe.ForwardOptions{Body: "x"}); err == nil {
		t.Error("a forward without a recipient was sent")
	}
	again, err := s.ForwardMail(t.Context(), "5", id, probe.ForwardOptions{To: []string{"dave@example.com"}, SkipSaveInSent: true})
	if err != nil || again.Subject != "Fw: report" {
		t.Errorf("second forward = %+v, %v", again, err)
	}
}

func TestForwardKeepsAnExistingFwdPrefix(t *testing.T) {
	srv, s, url := mailboxAt(t)
	login(t, url, "dave")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "FWD: already", From: "carol@example.com"})
	res, err := s.ForwardMail(t.Context(), "5", id, probe.ForwardOptions{To: []string{"dave@example.com"}, Body: "x"})
	if err != nil || res.Subject != "FWD: already" {
		t.Errorf("subject = %+v, %v", res, err)
	}
}

func TestFetchMIMEGivesTheWholeMessage(t *testing.T) {
	srv, s := mailbox(t)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "eml test", From: "Carol <carol@example.com>", To: "bob@example.com",
		Cc: "dave@example.com", Body: "line one\r\nline two ä"})

	raw, err := s.FetchMIME(t.Context(), "5", id)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"From: Carol <carol@example.com>", "To: bob@example.com", "Cc: dave@example.com", "Subject: eml test", "\r\n\r\nline one\r\nline two ä"} {
		if !strings.Contains(text, want) {
			t.Errorf("the message lacks %q:\n%s", want, text)
		}
	}
	if _, err := s.FetchMIME(t.Context(), "5", "5:99999"); err == nil {
		t.Error("the MIME of a mail that does not exist was returned")
	}
}
