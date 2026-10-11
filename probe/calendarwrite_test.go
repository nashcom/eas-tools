package probe_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

// calendarBox is a mailbox of the mock with the sessions of bob (who works) and carol and dave (who get the invitations).
func calendarBox(t *testing.T, o easmockserver.Options) (*easmockserver.Server, *probe.Session, string) {
	t.Helper()
	o.Mailbox = true
	srv := easmockserver.NewWithOptions(o)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	s, err := probe.NewSession(probe.Config{ServerURL: ts.URL, Username: "bob", Email: "bob@example.com", Password: easmockserver.Password})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) }, func() error { return s.Provision(ctx, nil) }, func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	login(t, ts.URL, "carol", "dave")
	return srv, s, ts.URL
}

func TestCreateEventAppearsInTheCalendar(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{})
	start := time.Now().Add(48 * time.Hour).Truncate(time.Second)

	id, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "Review", Location: "Room 4", Body: "the agenda",
		Start: start, End: start.Add(time.Hour), Busy: 2, Sensitivity: 2, Reminder: 15})
	if err != nil || id == "" {
		t.Fatalf("create = %q, %v", id, err)
	}
	list, err := s.ListEvents(t.Context(), "8", 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("%d events, %v", len(list), err)
	}
	e := list[0]
	if e.ID != id || e.Subject != "Review" || e.Location != "Room 4" || e.Body != "the agenda" || e.Busy != 2 || e.Sensitivity != 2 || e.Reminder != 15 ||
		!e.Start.Equal(start) || !e.End.Equal(start.Add(time.Hour)) || e.AllDay || len(e.Attendees) != 0 || e.Meeting != 0 {
		t.Errorf("event = %+v", e)
	}
	if n := len(srv.Messages("carol", "5")); n != 0 {
		t.Errorf("an event without attendees sent %d mails", n)
	}

	// a second one right after, and a list that follows
	if _, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "Second", Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListEvents(t.Context(), "8", 0); len(list) != 2 {
		t.Errorf("%d events after the second, want 2", len(list))
	}
}

func TestCreateEventWorksWithAServerThatNeedsOptions(t *testing.T) {
	_, s, _ := calendarBox(t, easmockserver.Options{RequireOptions: true})
	start := time.Now().Add(time.Hour).Truncate(time.Second)
	if _, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "needs options", Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListEvents(t.Context(), "8", 0); err != nil || len(list) != 1 {
		t.Errorf("%d events, %v", len(list), err)
	}
}

func TestCreateEventWithAttendeesIsAMeeting(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{SendInvitations: true})
	start := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	_, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "Kickoff", Start: start, End: start.Add(time.Hour), Busy: 2,
		Attendees: []probe.Attendee{{Name: "Carol", Email: "carol@example.com", Type: 1}, {Email: "dave@example.com", Type: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListEvents(t.Context(), "8", 0)
	if len(list) != 1 {
		t.Fatalf("%d events", len(list))
	}
	e := list[0]
	if e.Meeting != 1 || e.OrganizerEmail != "bob@example.com" || len(e.Attendees) != 2 ||
		e.Attendees[0].Email != "carol@example.com" || e.Attendees[0].Name != "Carol" || e.Attendees[0].Type != 1 || e.Attendees[1].Type != 2 {
		t.Errorf("meeting = %+v", e)
	}
	for _, u := range []string{"carol", "dave"} {
		got := srv.Messages(u, "5")
		if len(got) != 1 || got[0].Subject != "Invitation: Kickoff" {
			t.Errorf("%s's Inbox = %+v, want the invitation", u, got)
		}
	}
}

func TestCreateAllDayEvent(t *testing.T) {
	_, s, _ := calendarBox(t, easmockserver.Options{})
	day := time.Now().AddDate(0, 0, 10).UTC().Truncate(24 * time.Hour)
	if _, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "Holiday", AllDay: true, Start: day, End: day.AddDate(0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListEvents(t.Context(), "8", 0)
	if len(list) != 1 || !list[0].AllDay || !list[0].Start.Equal(day) || !list[0].End.Equal(day.AddDate(0, 0, 1)) {
		t.Errorf("events = %+v", list)
	}
}

func TestCreateEventChecksTheDraft(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{})
	now := time.Now().Add(time.Hour)
	for name, d := range map[string]probe.EventDraft{
		"no start":       {Subject: "x", End: now},
		"no end":         {Subject: "x", Start: now},
		"end before":     {Subject: "x", Start: now, End: now.Add(-time.Hour)},
		"no subject":     {Start: now, End: now.Add(time.Hour)},
		"bad attendee":   {Subject: "x", Start: now, End: now.Add(time.Hour), Attendees: []probe.Attendee{{Email: "not an address"}}},
		"empty attendee": {Subject: "x", Start: now, End: now.Add(time.Hour), Attendees: []probe.Attendee{{}}},
	} {
		if _, err := s.CreateEvent(t.Context(), "8", d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if list, _ := s.ListEvents(t.Context(), "8", 0); len(list) != 0 {
		t.Errorf("a wrong draft reached the server: %d events", len(list))
	}
	_ = srv
}

func TestDeleteEvent(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{})
	start := time.Now().Add(time.Hour)
	keep := srv.SeedEvent("bob", easmockserver.Event{Subject: "keep", Start: start, End: start.Add(time.Hour)})
	gone := srv.SeedEvent("bob", easmockserver.Event{Subject: "gone", Start: start, End: start.Add(time.Hour)})

	if err := s.DeleteEvent(t.Context(), "8", gone); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListEvents(t.Context(), "8", 0)
	if len(list) != 1 || list[0].ID != keep || strings.Contains(list[0].Subject, "gone") {
		t.Errorf("events after the delete = %+v", list)
	}
	if n := len(srv.Messages("bob", "7")); n != 0 {
		t.Errorf("the deleted event went to Deleted Items (%d mails)", n)
	}
}
