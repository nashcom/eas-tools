package probe_test

import (
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

func TestListEventsReadsTheCalendar(t *testing.T) {
	srv, s := mailbox(t)
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	srv.SeedEvent("bob", easmockserver.Event{
		UID: "uid-1", Subject: "Planning", Location: "Room 4", Body: "agenda", Start: start, End: start.Add(time.Hour),
		Busy: 2, Reminder: 15, Meeting: 1, Organizer: "carol@example.com", OrganizerName: "Carol",
		Attendees: []easmockserver.Attendee{{Name: "Bob", Email: "bob@example.com", Status: 3, Type: 1}, {Email: "dave@example.com", Status: 5, Type: 2}},
	})

	cal, err := s.FindFolder("calendar")
	if err != nil || cal.ID != "8" {
		t.Fatalf("calendar folder = %+v, %v", cal, err)
	}
	list, err := s.ListEvents(t.Context(), cal.ID, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("%d events, %v; want 1", len(list), err)
	}
	e := list[0]
	if e.Subject != "Planning" || e.Location != "Room 4" || e.UID != "uid-1" || e.Body != "agenda" || e.Folder != "8" {
		t.Errorf("event = %+v", e)
	}
	if !e.Start.Equal(start) || !e.End.Equal(start.Add(time.Hour)) {
		t.Errorf("times %v - %v, want %v - %v", e.Start, e.End, start, start.Add(time.Hour))
	}
	if e.Busy != 2 || e.Reminder != 15 || e.Meeting != 1 || e.OrganizerEmail != "carol@example.com" || e.OrganizerName != "Carol" {
		t.Errorf("event = %+v", e)
	}
	if len(e.Attendees) != 2 || e.Attendees[0].Email != "bob@example.com" || e.Attendees[0].Status != 3 || e.Attendees[1].Type != 2 {
		t.Errorf("attendees = %+v", e.Attendees)
	}
	if e.Repeat != nil {
		t.Errorf("a single event has a repeat rule: %+v", e.Repeat)
	}
}

func TestListEventsRepeatRuleAndOrder(t *testing.T) {
	srv, s := mailbox(t)
	now := time.Now().Truncate(time.Second)
	until := now.Add(60 * 24 * time.Hour)
	srv.SeedEvent("bob", easmockserver.Event{Subject: "later", Start: now.Add(72 * time.Hour), End: now.Add(73 * time.Hour)})
	srv.SeedEvent("bob", easmockserver.Event{Subject: "weekly", Start: now.Add(24 * time.Hour), End: now.Add(25 * time.Hour),
		Repeat: &easmockserver.Repeat{Type: 1, Interval: 2, DayOfWeek: 2 | 8, Until: until}})

	list, err := s.ListEvents(t.Context(), "8", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("%d events, %v", len(list), err)
	}
	if list[0].Subject != "weekly" || list[1].Subject != "later" {
		t.Errorf("order: %q, %q; want by start time", list[0].Subject, list[1].Subject)
	}
	r := list[0].Repeat
	if r == nil || r.Type != 1 || r.Interval != 2 || r.DayOfWeek != 10 || !r.Until.Equal(until) {
		t.Errorf("repeat = %+v", r)
	}
}

func TestListEventsDateWindow(t *testing.T) {
	srv, s := mailbox(t)
	now := time.Now()
	srv.SeedEvent("bob", easmockserver.Event{Subject: "recent", Start: now.Add(-20 * 24 * time.Hour), End: now.Add(-20*24*time.Hour + time.Hour)})
	srv.SeedEvent("bob", easmockserver.Event{Subject: "upcoming", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)})

	for since, want := range map[string]int{"2w": 1, "1m": 2, "6m": 2} {
		f, _ := probe.ParseSince(since)
		list, err := s.ListEvents(t.Context(), "8", f)
		if err != nil || len(list) != want {
			t.Errorf("since %s: %d events, %v; want %d", since, len(list), err, want)
		}
	}
}
