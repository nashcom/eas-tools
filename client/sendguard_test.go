package client_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// guarded is a mailbox "bob" whose manager may send, change and write, but only to example.com.
func guarded(t *testing.T) (*easmockserver.Server, *client.Manager) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{{Name: "bob", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}},
		client.Options{AllowWrite: true, AllowSend: true, SendDomains: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return srv, m
}

func errCode(err error) string {
	if err == nil {
		return ""
	}
	return client.AsError(err).Code
}

func TestAReplyGoesToTheSenderOfTheMailAndThatIsChecked(t *testing.T) {
	srv, m := guarded(t)
	ctx := t.Context()
	inside := srv.Seed("bob", easmockserver.Mail{Subject: "in", From: "alice@example.com", To: "bob@example.com", Body: "b"})
	outside := srv.Seed("bob", easmockserver.Mail{Subject: "out", From: "Mallory <mallory@evil.org>", To: "bob@example.com", Body: "b"})
	mixed := srv.Seed("bob", easmockserver.Mail{Subject: "mixed", From: "alice@example.com", To: "bob@example.com, carol@example.com", Cc: "dave@evil.org", Body: "b"})

	if _, err := m.Reply(ctx, "bob", "inbox", inside, client.ReplyRequest{Body: "thanks"}); err != nil {
		t.Errorf("a reply to a sender inside the list: %v", err)
	}
	if _, err := m.Reply(ctx, "bob", "inbox", outside, client.ReplyRequest{Body: "thanks"}); errCode(err) != "recipient_not_allowed" {
		t.Errorf("a reply to a sender outside the list: %v", err)
	}
	// reply to the sender only: fine; to all: the Cc of the mail is outside the list; the address of the account itself is not a recipient
	if _, err := m.Reply(ctx, "bob", "inbox", mixed, client.ReplyRequest{Body: "thanks"}); err != nil {
		t.Errorf("a reply to the sender of a mail with an outside Cc: %v", err)
	}
	if _, err := m.Reply(ctx, "bob", "inbox", mixed, client.ReplyRequest{Body: "thanks", All: true}); errCode(err) != "recipient_not_allowed" {
		t.Errorf("a reply to all with an outside recipient: %v", err)
	}
	both := srv.Seed("bob", easmockserver.Mail{Subject: "both", From: "alice@example.com", To: "bob@example.com, carol@example.com", Body: "b"})
	if _, err := m.Reply(ctx, "bob", "inbox", both, client.ReplyRequest{Body: "thanks", All: true}); err != nil {
		t.Errorf("a reply to all inside the list (the account itself is skipped): %v", err)
	}
}

func TestInvitationsAndCancellationsFollowTheListOfDomains(t *testing.T) {
	srv, m := guarded(t)
	ctx := t.Context()
	start := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339)
	meeting := func(email string) client.EventRequest {
		return client.EventRequest{Subject: "Meeting", Start: start, Attendees: []client.AttendeeRequest{{Email: email}}}
	}
	if _, err := m.CreateEvent(ctx, "bob", "", meeting("mallory@evil.org")); errCode(err) != "recipient_not_allowed" {
		t.Errorf("an attendee outside the list: %v", err)
	}
	if _, err := m.CreateEvent(ctx, "bob", "", meeting("carol@example.com")); err != nil {
		t.Errorf("an attendee inside the list: %v", err)
	}
	// even when the client leaves the invitation to the server: the server mails them
	r := meeting("mallory@evil.org")
	r.NoInviteMail = true
	if _, err := m.CreateEvent(ctx, "bob", "", r); errCode(err) != "recipient_not_allowed" {
		t.Errorf("an attendee outside the list, invitation left to the server: %v", err)
	}

	s := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Minute)
	id := srv.SeedEvent("bob", easmockserver.Event{UID: "u", Subject: "External", Start: s, End: s.Add(time.Hour), Meeting: 1, Organizer: "bob@example.com",
		Attendees: []easmockserver.Attendee{{Email: "mallory@evil.org"}}})
	if _, err := m.DeleteEvent(ctx, "bob", "", id); errCode(err) != "recipient_not_allowed" {
		t.Errorf("deleting a meeting with an attendee outside the list: %v", err)
	}
	if res, err := m.Events(ctx, "bob", "", client.EventParams{}); err != nil || res.Count == 0 {
		t.Errorf("the meeting was deleted although the cancellation may not go: %v %v", res, err)
	}
}
