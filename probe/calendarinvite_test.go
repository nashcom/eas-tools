package probe_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

// A Traveler answered the Add of a calendar entry with the client id as the server id (seen on the wire). The id that the caller
// gets is the one of the entry when it is read.
func TestCreateEventReturnsTheRealIDWhenTheServerEchoesTheClientID(t *testing.T) {
	_, s, _ := calendarBox(t, easmockserver.Options{EchoClientID: true})
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	id, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "echo", Start: start, End: start.Add(time.Hour), Busy: 2})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListEvents(t.Context(), "8", 0)
	if len(list) != 1 || list[0].ID != id || !strings.HasPrefix(id, "8:") {
		t.Errorf("id = %q, the calendar has %+v", id, list)
	}

	// the same subject and time twice: each one gets its own id
	id2, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "echo", Start: start, End: start.Add(time.Hour), Busy: 2})
	if err != nil || id2 == id {
		t.Errorf("second id = %q, %v; the first was %q", id2, err, id)
	}
}

func meetingEvent() probe.Event {
	start := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	return probe.Event{ID: "8:1", Folder: "8", UID: "meeting-1@eas-client.invalid", Subject: "Kickoff", Location: "Room 4", Body: "agenda", Start: start,
		End: start.Add(time.Hour), Busy: 2, Reminder: 15, Meeting: 1, OrganizerEmail: "bob@example.com", OrganizerName: "Bob",
		Attendees: []probe.Attendee{{Name: "Carol", Email: "carol@example.com", Type: 1}, {Email: "dave@example.com", Type: 2}}}
}

func TestICalendarMethodRequestAndCancel(t *testing.T) {
	req := unfold(t, meetingEvent().ICalendarMethod(icalNow, "REQUEST", 0))
	mustHave(t, req, "METHOD:REQUEST", "SEQUENCE:0", "STATUS:CONFIRMED", "UID:meeting-1@eas-client.invalid", "ORGANIZER;CN=Bob:mailto:bob@example.com",
		"ATTENDEE;CN=Carol;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:carol@example.com",
		"ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:dave@example.com")
	for _, l := range req {
		if l == "BEGIN:VALARM" {
			t.Error("a reminder is a personal setting and does not belong in an invitation")
		}
	}

	cancel := unfold(t, meetingEvent().ICalendarMethod(icalNow, "CANCEL", 1))
	mustHave(t, cancel, "METHOD:CANCEL", "SEQUENCE:1", "STATUS:CANCELLED", "UID:meeting-1@eas-client.invalid")

	// the plain file has no method
	for _, l := range unfold(t, meetingEvent().ICalendar(icalNow)) {
		if strings.HasPrefix(l, "METHOD:") {
			t.Errorf("a file without a method has %q", l)
		}
	}
}

func TestSendInvitationMailsTheMeetingToTheAttendees(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{})

	if err := s.SendInvitation(t.Context(), "REQUEST", meetingEvent(), 0); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"carol", "dave"} {
		got := srv.Messages(u, "5")
		if len(got) != 1 || got[0].Subject != "Kickoff" || !strings.HasPrefix(got[0].From, "bob@example.com") {
			t.Fatalf("%s's Inbox = %+v", u, got)
		}
		for _, want := range []string{"Content-Type: text/plain", "Content-Type: text/calendar", "method=REQUEST", "METHOD:REQUEST", "Kickoff", "Room 4"} {
			if !strings.Contains(got[0].Body, want) {
				t.Errorf("%s's mail lacks %q:\n%s", u, want, got[0].Body)
			}
		}
	}
	if n := len(srv.Messages("bob", "6")); n != 1 {
		t.Errorf("%d copies in Sent Items, want 1", n)
	}

	if err := s.SendInvitation(t.Context(), "CANCEL", meetingEvent(), 1); err != nil {
		t.Fatal(err)
	}
	if got := srv.Messages("carol", "5"); len(got) != 2 || got[1].Subject != "Canceled: Kickoff" || !strings.Contains(got[1].Body, "METHOD:CANCEL") {
		t.Errorf("carol's Inbox after the cancellation = %+v", got)
	}

	noOne := meetingEvent()
	noOne.Attendees = nil
	if err := s.SendInvitation(t.Context(), "REQUEST", noOne, 0); err == nil {
		t.Error("an invitation without attendees was sent")
	}
	if err := s.SendInvitation(t.Context(), "REPLY", meetingEvent(), 0); err == nil {
		t.Error("an unknown method was accepted")
	}
}
