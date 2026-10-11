package client_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// zoned is a manager with the time zone given in its options and in the account of carol.
func zoned(t *testing.T, zone, carolZone string) (*easmockserver.Server, *client.Manager) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true, RequireOptions: true, EchoClientID: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{
		{Name: "bob", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password},
		{Name: "carol", Server: ts.URL, Email: "carol@example.com", User: "carol", Password: easmockserver.Password, TimeZone: carolZone},
	}, client.Options{AllowWrite: true, AllowSend: true, TimeZone: zone})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, err := m.Info(t.Context(), "carol"); err != nil {
		t.Fatal(err)
	}
	return srv, m
}

func TestTimesWithoutAZoneAreTimesInTheConfiguredZone(t *testing.T) {
	srv, m := zoned(t, "Europe/Berlin", "")
	r, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "local", Start: "2026-11-20T11:00:00", Duration: "1h"}) // 10:00 UTC
	if err != nil {
		t.Fatal(err)
	}
	e, _ := srv.EventOf("bob", r.ID)
	if !e.Start.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("start on the server = %v, want 10:00 UTC", e.Start)
	}
	tz, err := eas.DecodeTimeZone(e.TimeZone)
	if err != nil || tz.BiasMinutes != -60 || tz.DaylightDate.Month != 3 {
		t.Errorf("the entry has the zone %+v, %v", tz, err)
	}

	// a time with an offset stays what it says; summer time is summer time
	r2, _ := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "summer", Start: "2027-07-01T12:00:00"})
	if e2, _ := srv.EventOf("bob", r2.ID); !e2.Start.Equal(time.Date(2027, 7, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("a summer time = %v, want 10:00 UTC (UTC+2)", e2.Start)
	}
	r3, _ := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "offset", Start: "2026-11-20T11:00:00+05:00"})
	if e3, _ := srv.EventOf("bob", r3.ID); !e3.Start.Equal(time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("a time with an offset = %v, want 06:00 UTC", e3.Start)
	}
}

func TestAnAccountHasItsOwnZone(t *testing.T) {
	srv, m := zoned(t, "Europe/Berlin", "America/New_York")
	r, err := m.CreateEvent(t.Context(), "carol", "", client.EventRequest{Subject: "ny", Start: "2026-11-20T09:00:00"}) // 14:00 UTC
	if err != nil {
		t.Fatal(err)
	}
	e, _ := srv.EventOf("carol", r.ID)
	if !e.Start.Equal(time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v, want 14:00 UTC", e.Start)
	}
	if tz, _ := eas.DecodeTimeZone(e.TimeZone); tz.BiasMinutes != 300 {
		t.Errorf("zone of the entry %+v, want New York", tz)
	}
	info, _ := m.Info(t.Context(), "carol")
	if info.TimeZone != "America/New_York" {
		t.Errorf("info says the zone is %q", info.TimeZone)
	}
	if info, _ := m.Info(t.Context(), "bob"); info.TimeZone != "Europe/Berlin" {
		t.Errorf("bob's zone is %q", info.TimeZone)
	}
}

func TestTheInvitationIsInTheZoneOfTheOrganizer(t *testing.T) {
	srv, m := zoned(t, "Europe/Berlin", "")
	_, err := m.CreateEvent(t.Context(), "bob", "", client.EventRequest{Subject: "Kickoff", Start: "2026-10-11T11:00:00", Duration: "1h",
		Attendees: []client.AttendeeRequest{{Email: "carol@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := srv.Messages("carol", "5")
	if len(got) != 1 {
		t.Fatalf("carol's Inbox = %+v", got)
	}
	body := strings.ReplaceAll(strings.ReplaceAll(got[0].Body, "=\r\n", ""), "=3D", "=") // undo the quoted-printable coding
	for _, want := range []string{"BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time", "DTSTART;TZID=W. Europe Standard Time:20261011T110000", "DTEND;TZID=W. Europe Standard Time:20261011T120000"} {
		if !strings.Contains(body, want) {
			t.Errorf("the invitation lacks %q:\n%s", want, body)
		}
	}
}

func TestTheEventListIsReadInTheZone(t *testing.T) {
	srv, m := zoned(t, "Europe/Berlin", "")
	start := time.Date(2030, 6, 15, 8, 0, 0, 0, time.UTC) // 10:00 in Berlin
	srv.SeedEvent("bob", easmockserver.Event{Subject: "later", Start: start, End: start.Add(time.Hour)})
	// the window is given as local dates: the 15th of June in Berlin starts at 22:00 UTC of the 14th
	l, err := m.Events(t.Context(), "bob", "", client.EventParams{From: "2030-06-15", To: "2030-06-15T23:00:00"})
	if err != nil || l.Count != 1 {
		t.Fatalf("events = %+v, %v", l, err)
	}
	if l.From.Time != "2030-06-14T22:00:00Z" {
		t.Errorf("from = %s, want 2030-06-14T22:00:00Z", l.From.Time)
	}
}

func TestTheZoneIsChecked(t *testing.T) {
	if _, err := client.NewManager([]client.Account{{Name: "a", Server: "x", User: "u", Password: "p"}}, client.Options{TimeZone: "Nowhere/City"}); err == nil {
		t.Error("an unknown zone was accepted")
	}
	if _, err := client.NewManager([]client.Account{{Name: "a", Server: "x", User: "u", Password: "p", TimeZone: "Mars/Base"}}, client.Options{}); err == nil {
		t.Error("an unknown zone of an account was accepted")
	}
	// no zone given: the zone of the machine
	t.Setenv("TZ", "Asia/Tokyo")
	m, err := client.NewManager([]client.Account{{Name: "a", Server: "x", User: "u", Password: "p"}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.TimeZone("a"); got != "Asia/Tokyo" {
		t.Errorf("the default zone is %q, want the one of the machine", got)
	}
	if got := m.TimeZone(""); got != "Asia/Tokyo" {
		t.Errorf("with one account no name is needed: %q", got)
	}
}
