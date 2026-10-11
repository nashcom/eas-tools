package client_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// writer is a manager on a mock mailbox that mails invitations, with the switches as given.
func writer(t *testing.T, write, send bool) (*easmockserver.Server, *client.Manager) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true, RequireOptions: true, EchoClientID: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	var accounts []client.Account
	for _, n := range []string{"bob", "carol"} {
		accounts = append(accounts, client.Account{Name: n, Server: ts.URL, Email: n + "@example.com", User: n, Password: easmockserver.Password})
	}
	m, err := client.NewManager(accounts, client.Options{AllowWrite: write, AllowSend: send})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Info(t.Context(), "carol"); err != nil { // carol is known to the mock: she can be invited
		t.Fatal(err)
	}
	return srv, m
}

func TestCreateEventAndItShowsInTheCalendar(t *testing.T) {
	_, m := writer(t, true, false) // write, but no send: an event without attendees needs no sending
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Minute)

	r, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "Review", Location: "Room 4", Body: "agenda",
		Start: start.Format(time.RFC3339), Duration: "90m", ReminderMinutes: 10, Busy: "tentative", Sensitivity: "private"})
	if err != nil || !r.OK || r.Action != "create" || r.ID == "" || r.Folder != "8" || len(r.Attendees) != 0 {
		t.Fatalf("create = %+v, %v", r, err)
	}
	g, err := m.Event(t.Context(), "bob", "", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := g.Event
	if e.Subject != "Review" || e.Location != "Room 4" || e.Body != "agenda" || e.Busy != "tentative" || e.Sensitivity != "private" || e.ReminderMinutes != 10 ||
		e.Start.Time != start.Format(time.RFC3339Nano) || e.End.Time != start.Add(90*time.Minute).Format(time.RFC3339Nano) || e.Meeting != "none" {
		t.Errorf("event = %+v", e)
	}

	// the default length is an hour, the default state busy
	r2, err := m.CreateEvent(t.Context(), "bob", "calendar", client.EventRequest{Subject: "Default", Start: start.Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	g2, _ := m.Event(t.Context(), "bob", "", r2.ID)
	if g2.Event.Busy != "busy" || g2.Event.End.Time != start.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("defaults: %+v", g2.Event)
	}
}

func TestCreateAllDayEventTakesDates(t *testing.T) {
	_, m := writer(t, true, false)
	day := time.Now().AddDate(0, 0, 20).UTC()
	first, last := day.Format("2006-01-02"), day.AddDate(0, 0, 2).Format("2006-01-02")

	r, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "Trip", AllDay: true, Start: first, End: last})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := m.Event(t.Context(), "bob", "", r.ID)
	wantEnd := day.AddDate(0, 0, 3).Format("2006-01-02") + "T00:00:00Z"
	if !e.Event.AllDay || !strings.HasPrefix(e.Event.Start.Time, first+"T00:00:00") || e.Event.End.Time != wantEnd {
		t.Errorf("all-day event = %+v, want it to start on %s and end at %s", e.Event, first, wantEnd)
	}
	one, _ := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "One day", AllDay: true, Start: first}) // no end: that day
	e1, _ := m.Event(t.Context(), "bob", "", one.ID)
	if e1.Event.End.Time != day.AddDate(0, 0, 1).Format("2006-01-02")+"T00:00:00Z" {
		t.Errorf("one day: %+v", e1.Event)
	}
}

func TestCreateEventChecksTheRequest(t *testing.T) {
	srv, m := writer(t, true, true)
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for name, r := range map[string]client.EventRequest{
		"no subject":      {Start: start},
		"no start":        {Subject: "x"},
		"bad start":       {Subject: "x", Start: "tomorrow"},
		"bad end":         {Subject: "x", Start: start, End: "later"},
		"end and length":  {Subject: "x", Start: start, End: start, Duration: "1h"},
		"end before":      {Subject: "x", Start: start, End: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"bad duration":    {Subject: "x", Start: start, Duration: "soon"},
		"zero duration":   {Subject: "x", Start: start, Duration: "0s"},
		"bad busy":        {Subject: "x", Start: start, Busy: "away"},
		"bad sensitivity": {Subject: "x", Start: start, Sensitivity: "secret"},
		"bad type":        {Subject: "x", Start: start, Attendees: []client.AttendeeRequest{{Email: "carol@example.com", Type: "vip"}}},
		"bad address":     {Subject: "x", Start: start, Attendees: []client.AttendeeRequest{{Email: "carol"}}},
		"negative remind": {Subject: "x", Start: start, ReminderMinutes: -1},
		"last before":     {Subject: "x", AllDay: true, Start: "2026-12-24", End: "2026-12-20"},
	} {
		if _, err := m.CreateEvent(t.Context(), "bob", "", r); err == nil || client.AsError(err).Status != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if l, _ := m.Events(t.Context(), "bob", "", client.EventParams{Limit: -1}); l == nil || l.Count != 0 {
		t.Errorf("a wrong request reached the server: %+v", l)
	}
	_ = srv
}

func TestMeetingNeedsTheSwitchForSending(t *testing.T) {
	srv, m := writer(t, true, false) // write only
	start := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	req := client.EventRequest{Subject: "Kickoff", Start: start, Attendees: []client.AttendeeRequest{{Email: "carol@example.com"}}}

	_, err := m.CreateEvent(t.Context(), "bob", "", req)
	if code(t, err) != "send_disabled" {
		t.Fatalf("a meeting with write only: %v", err)
	}
	if n := len(srv.Messages("carol", "5")); n != 0 {
		t.Errorf("a refused meeting sent %d mails", n)
	}

	// with the switch it works and the attendee gets the invitation
	srv2, m2 := writer(t, true, true)
	req.Attendees = []client.AttendeeRequest{{Email: "carol@example.com", Name: "Carol"}, {Email: "dave@example.com", Type: "optional"}}
	r, err := m2.CreateEvent(t.Context(), "bob", "", req)
	if err != nil || len(r.Attendees) != 2 || r.Attendees[0] != "carol@example.com" {
		t.Fatalf("meeting = %+v, %v", r, err)
	}
	if got := srv2.Messages("carol", "5"); len(got) != 1 || got[0].Subject != "Kickoff" {
		t.Errorf("carol's Inbox = %+v", got)
	}
	e, _ := m2.Event(t.Context(), "bob", "", r.ID)
	if e.Event.Meeting != "meeting" || e.Event.Organizer == nil || e.Event.Organizer.Email != "bob@example.com" || len(e.Event.Attendees) != 2 ||
		e.Event.Attendees[1].Type != "optional" || e.Event.Attendees[0].Type != "required" {
		t.Errorf("meeting = %+v", e.Event)
	}
}

func TestDeleteEventAndTheGuardForMeetings(t *testing.T) {
	srv, m := writer(t, true, false) // write only
	start := time.Now().Add(24 * time.Hour)
	single := srv.SeedEvent("bob", easmockserver.Event{Subject: "single", Start: start, End: start.Add(time.Hour)})
	meeting := srv.SeedEvent("bob", easmockserver.Event{Subject: "meeting", Start: start, End: start.Add(time.Hour), Meeting: 1, Organizer: "bob@example.com",
		Attendees: []easmockserver.Attendee{{Email: "carol@example.com", Type: 1}}})

	d, err := m.DeleteEvent(t.Context(), "bob", "", single) // the folder from the id
	if err != nil || !d.OK || d.Action != "delete" || d.Folder != "8" {
		t.Fatalf("delete = %+v, %v", d, err)
	}
	if _, err := m.DeleteEvent(t.Context(), "bob", "calendar", meeting); code(t, err) != "send_disabled" {
		t.Errorf("a meeting with write only: %v", err)
	}
	l, _ := m.Events(t.Context(), "bob", "", client.EventParams{})
	if l.Count != 1 || l.Events[0].Subject != "meeting" {
		t.Errorf("events = %+v", l)
	}
	if _, err := m.DeleteEvent(t.Context(), "bob", "calendar", "8:99999"); code(t, err) != "event_not_found" {
		t.Errorf("unknown event: %v", err)
	}

	_, m2 := writer(t, true, true)
	_ = m2
	srvOnly, mReadOnly := writer(t, false, true) // send, but no write
	id := srvOnly.SeedEvent("bob", easmockserver.Event{Subject: "x", Start: start, End: start.Add(time.Hour)})
	if _, err := mReadOnly.DeleteEvent(t.Context(), "bob", "", id); code(t, err) != "write_disabled" {
		t.Errorf("delete without -write: %v", err)
	}
	if _, err := mReadOnly.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "x", Start: start.UTC().Format(time.RFC3339)}); code(t, err) != "write_disabled" {
		t.Errorf("create without -write: %v", err)
	}
}
