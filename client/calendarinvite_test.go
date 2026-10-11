package client_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// A Traveler creates a meeting through ActiveSync and invites nobody: the client mails the invitation, with the UID of the entry.
func TestMeetingMailsTheInvitationWithTheUIDOfTheEntry(t *testing.T) {
	srv, m := writer(t, true, true)
	start := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	r, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "Kickoff", Location: "Room 4", Start: start,
		Attendees: []client.AttendeeRequest{{Email: "carol@example.com", Name: "Carol"}}})
	if err != nil || !r.InvitationMail || len(r.Attendees) != 1 {
		t.Fatalf("create = %+v, %v", r, err)
	}
	got := srv.Messages("carol", "5")
	if len(got) != 1 || got[0].Subject != "Kickoff" {
		t.Fatalf("carol's Inbox = %+v, want exactly the invitation", got)
	}
	for _, want := range []string{"method=REQUEST", "METHOD:REQUEST", "Room 4", "ORGANIZER"} {
		if !strings.Contains(got[0].Body, want) {
			t.Errorf("the invitation lacks %q:\n%s", want, got[0].Body)
		}
	}
	e, err := m.Event(t.Context(), "bob", "", r.ID) // the id works although the server answered the Add with the client id
	if err != nil || e.Event.UID == "" || !strings.Contains(got[0].Body, strings.ReplaceAll(e.Event.UID, "=", "=3D")) {
		t.Errorf("the UID of the entry %q is not the one of the invitation:\n%v\n%s", e.Event.UID, err, got[0].Body)
	}
}

func TestMeetingWithoutInvitationMailLeavesItToTheServer(t *testing.T) {
	srv, m := writer(t, true, true)
	start := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	r, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "Quiet", Start: start, NoInviteMail: true,
		Attendees: []client.AttendeeRequest{{Email: "carol@example.com"}}})
	if err != nil || r.InvitationMail {
		t.Fatalf("create = %+v, %v", r, err)
	}
	if n := len(srv.Messages("carol", "5")); n != 0 {
		t.Errorf("%d mails although the client was told not to send the invitation", n)
	}
	if e, err := m.Event(t.Context(), "bob", "", r.ID); err != nil || len(e.Event.Attendees) != 1 {
		t.Errorf("the entry has to be there with its attendee: %+v, %v", e, err)
	}
}

func TestDeletingAMeetingMailsTheCancellationOnlyForTheOrganizer(t *testing.T) {
	srv, m := writer(t, true, true)
	start := time.Now().Add(24 * time.Hour)
	mine := srv.SeedEvent("bob", easmockserver.Event{UID: "mine", Subject: "Mine", Start: start, End: start.Add(time.Hour), Meeting: 1, Organizer: "bob@example.com",
		Attendees: []easmockserver.Attendee{{Email: "carol@example.com", Type: 1}}})
	theirs := srv.SeedEvent("bob", easmockserver.Event{UID: "theirs", Subject: "Theirs", Start: start, End: start.Add(time.Hour), Meeting: 3, Organizer: "carol@example.com",
		Attendees: []easmockserver.Attendee{{Email: "bob@example.com", Type: 1}}})

	d, err := m.DeleteEvent(t.Context(), "bob", "", theirs) // only removes the entry of the invited account
	if err != nil || d.InvitationMail {
		t.Fatalf("delete of an invitation = %+v, %v", d, err)
	}
	if n := len(srv.Messages("carol", "5")); n != 0 {
		t.Errorf("%d mails to the organizer of a meeting that bob removed from his calendar", n)
	}

	d, err = m.DeleteEvent(t.Context(), "bob", "", mine)
	if err != nil || !d.InvitationMail {
		t.Fatalf("delete of an own meeting = %+v, %v", d, err)
	}
	got := srv.Messages("carol", "5")
	if len(got) != 1 || got[0].Subject != "Canceled: Mine" || !strings.Contains(got[0].Body, "METHOD:CANCEL") || !strings.Contains(got[0].Body, "STATUS:CANCELLED") {
		t.Errorf("carol's Inbox = %+v", got)
	}

	// and without the cancellation mail
	again := srv.SeedEvent("bob", easmockserver.Event{UID: "quiet", Subject: "Quiet", Start: start, End: start.Add(time.Hour), Meeting: 1, Organizer: "bob@example.com",
		Attendees: []easmockserver.Attendee{{Email: "carol@example.com", Type: 1}}})
	if d, err := m.DeleteEventWith(t.Context(), "bob", "", again, false); err != nil || d.InvitationMail {
		t.Fatalf("delete without the mail = %+v, %v", d, err)
	}
	if n := len(srv.Messages("carol", "5")); n != 1 {
		t.Errorf("carol has %d mails, want only the first cancellation", n)
	}
}
