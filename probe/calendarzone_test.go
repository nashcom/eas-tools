package probe_test

import (
	"testing"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

// The entry carries the zone of the user with the rules of daylight saving time, so that Notes does not say "created in a different
// time zone", and a UTC entry stays all zero.
var utcBlob = (probe.Zone{}).EASBlob(2026)

func TestCreateEventCarriesTheTimeZone(t *testing.T) {
	srv, s, _ := calendarBox(t, easmockserver.Options{})
	berlin := zone(t, "Europe/Berlin")
	start := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	id, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "zoned", Start: start, End: start.Add(time.Hour), Busy: 2, Zone: &berlin})
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := srv.EventOf("bob", id)
	if !ok {
		t.Fatal("the entry is not on the server")
	}
	tz, err := eas.DecodeTimeZone(ev.TimeZone)
	if err != nil {
		t.Fatal(err)
	}
	if tz.BiasMinutes != -60 || tz.DaylightBias != -60 || tz.DaylightDate.Month != 3 || tz.StandardDate.Month != 10 || tz.StandardName != "W. Europe Standard Time" {
		t.Errorf("time zone of the entry = %+v", tz)
	}
	if !ev.Start.Equal(start) { // the times on the wire are UTC whatever the zone is
		t.Errorf("start on the server %v, want %v", ev.Start, start)
	}

	id2, err := s.CreateEvent(t.Context(), "8", probe.EventDraft{Subject: "utc", Start: start, End: start.Add(time.Hour), Busy: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ev2, _ := srv.EventOf("bob", id2); len(ev2.TimeZone) == 0 || ev2.TimeZone != utcBlob {
		t.Errorf("an entry without a zone has %q, want the all zero blob", ev2.TimeZone)
	}
}
