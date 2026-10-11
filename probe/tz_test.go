package probe_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/probe"
)

func zone(t *testing.T, name string) probe.Zone {
	t.Helper()
	z, err := probe.LoadZone(name)
	if err != nil {
		t.Fatalf("zone %q: %v", name, err)
	}
	return z
}

func decoded(t *testing.T, z probe.Zone, year int) eas.EASTimeZone {
	t.Helper()
	tz, err := eas.DecodeTimeZone(z.EASBlob(year))
	if err != nil {
		t.Fatal(err)
	}
	return tz
}

func TestLoadZone(t *testing.T) {
	for _, name := range []string{"", "UTC", "utc"} {
		if z := zone(t, name); !z.IsUTC() {
			t.Errorf("%q is not UTC: %+v", name, z)
		}
	}
	if z := zone(t, "Europe/Berlin"); z.IsUTC() || z.Name != "Europe/Berlin" {
		t.Errorf("Berlin = %+v", z)
	}
	if _, err := probe.LoadZone("Nowhere/City"); err == nil {
		t.Error("an unknown zone was accepted")
	}
}

func TestWindowsNamesOfZones(t *testing.T) {
	for name, want := range map[string]string{
		"Europe/Berlin": "W. Europe Standard Time", "Europe/Vienna": "W. Europe Standard Time", "Europe/Paris": "Romance Standard Time",
		"Europe/London": "GMT Standard Time", "America/New_York": "Eastern Standard Time", "America/Los_Angeles": "Pacific Standard Time",
		"Asia/Tokyo": "Tokyo Standard Time", "Australia/Sydney": "AUS Eastern Standard Time", "UTC": "UTC",
	} {
		if got := zone(t, name).TZID(); got != want {
			t.Errorf("%s: TZID %q, want %q", name, got, want)
		}
	}
	// a zone without a Windows name keeps its own name
	if got := zone(t, "Pacific/Chatham").TZID(); got != "Pacific/Chatham" {
		t.Errorf("an unmapped zone: %q", got)
	}
}

func TestEASTimeZoneOfBerlin(t *testing.T) {
	tz := decoded(t, zone(t, "Europe/Berlin"), 2026)
	if tz.BiasMinutes != -60 || tz.StandardBias != 0 || tz.DaylightBias != -60 {
		t.Errorf("biases = %d, %d, %d; want -60, 0, -60", tz.BiasMinutes, tz.StandardBias, tz.DaylightBias)
	}
	// daylight time starts the last Sunday of March at 02:00 (standard time), standard time the last Sunday of October at 03:00 (daylight time)
	if d := tz.DaylightDate; d.Month != 3 || d.DayOfWeek != 0 || d.Day != 5 || d.Hour != 2 || d.Minute != 0 || d.Year != 0 {
		t.Errorf("daylight date = %+v", d)
	}
	if s := tz.StandardDate; s.Month != 10 || s.DayOfWeek != 0 || s.Day != 5 || s.Hour != 3 || s.Minute != 0 || s.Year != 0 {
		t.Errorf("standard date = %+v", s)
	}
}

func TestEASTimeZoneOfNewYorkAndSydney(t *testing.T) {
	ny := decoded(t, zone(t, "America/New_York"), 2026)
	if ny.BiasMinutes != 300 || ny.DaylightBias != -60 {
		t.Errorf("New York bias %d, %d", ny.BiasMinutes, ny.DaylightBias)
	}
	if d := ny.DaylightDate; d.Month != 3 || d.Day != 2 || d.DayOfWeek != 0 || d.Hour != 2 { // second Sunday of March
		t.Errorf("New York daylight = %+v", d)
	}
	if s := ny.StandardDate; s.Month != 11 || s.Day != 1 || s.DayOfWeek != 0 || s.Hour != 2 { // first Sunday of November
		t.Errorf("New York standard = %+v", s)
	}

	sy := decoded(t, zone(t, "Australia/Sydney"), 2026) // the southern summer: daylight time starts in October
	if sy.BiasMinutes != -600 {
		t.Errorf("Sydney bias %d", sy.BiasMinutes)
	}
	if d := sy.DaylightDate; d.Month != 10 || d.Day != 1 || d.DayOfWeek != 0 || d.Hour != 2 {
		t.Errorf("Sydney daylight = %+v", d)
	}
	if s := sy.StandardDate; s.Month != 4 || s.Day != 1 || s.DayOfWeek != 0 || s.Hour != 3 {
		t.Errorf("Sydney standard = %+v", s)
	}
}

func TestEASTimeZoneWithoutDaylightSaving(t *testing.T) {
	for _, name := range []string{"UTC", "Asia/Kolkata", "Asia/Tokyo"} {
		tz := decoded(t, zone(t, name), 2026)
		if tz.StandardDate.Month != 0 || tz.DaylightDate.Month != 0 || tz.DaylightBias != 0 {
			t.Errorf("%s: dates %+v / %+v, daylight bias %d; want no rule", name, tz.StandardDate, tz.DaylightDate, tz.DaylightBias)
		}
	}
	if tz := decoded(t, zone(t, "Asia/Kolkata"), 2026); tz.BiasMinutes != -330 {
		t.Errorf("Kolkata bias %d", tz.BiasMinutes)
	}
	if blob := zone(t, "UTC").EASBlob(2026); strings.Trim(blob, "A=") != "" {
		t.Errorf("the blob of UTC has to be all zero: %q", blob)
	}
}

func TestVTimezoneOfBerlin(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(string(zone(t, "Europe/Berlin").VTimezone(2026))), "\r\n")
	for _, want := range []string{
		"BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time",
		"BEGIN:STANDARD", "DTSTART:19701025T030000", "TZOFFSETFROM:+0200", "TZOFFSETTO:+0100", "RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10", "END:STANDARD",
		"BEGIN:DAYLIGHT", "DTSTART:19700329T020000", "TZOFFSETFROM:+0100", "TZOFFSETTO:+0200", "RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3", "END:DAYLIGHT",
		"END:VTIMEZONE",
	} {
		if !has(lines, want) {
			t.Errorf("the time zone lacks %q in:\n%s", want, strings.Join(lines, "\n"))
		}
	}

	utc := strings.TrimSpace(string(zone(t, "UTC").VTimezone(2026)))
	if utc != "" {
		t.Errorf("UTC needs no time zone block: %q", utc)
	}
	india := strings.Join(strings.Split(strings.TrimSpace(string(zone(t, "Asia/Kolkata").VTimezone(2026))), "\r\n"), "\n")
	if !strings.Contains(india, "TZOFFSETTO:+0530") || strings.Contains(india, "DAYLIGHT") || !strings.Contains(india, "TZID:India Standard Time") {
		t.Errorf("a zone without daylight saving:\n%s", india)
	}
}

func TestICalendarInAZoneHasLocalTimesAndTheTimeZone(t *testing.T) {
	berlin := zone(t, "Europe/Berlin")
	start := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC) // 11:00 in Berlin
	e := probe.Event{ID: "8:1", Folder: "8", UID: "z@x", Subject: "zone", Start: start, End: start.Add(time.Hour), Zone: &berlin}
	lines := unfold(t, e.ICalendar(icalNow))
	mustHave(t, lines, "BEGIN:VTIMEZONE", "TZID:W. Europe Standard Time", "DTSTART;TZID=W. Europe Standard Time:20261011T110000", "DTEND;TZID=W. Europe Standard Time:20261011T120000")
	posTZ, posEvent := -1, -1
	for i, l := range lines {
		switch l {
		case "BEGIN:VTIMEZONE":
			posTZ = i
		case "BEGIN:VEVENT":
			posEvent = i
		}
	}
	if posTZ < 0 || posTZ > posEvent {
		t.Errorf("the time zone (line %d) has to come before the event (line %d)", posTZ, posEvent)
	}

	// an all-day event has dates, no zone; without a zone the times stay UTC
	allDay := e
	allDay.AllDay, allDay.Start, allDay.End = true, time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	mustHave(t, unfold(t, allDay.ICalendar(icalNow)), "DTSTART;VALUE=DATE:20261224")
	plain := e
	plain.Zone = nil
	mustHave(t, unfold(t, plain.ICalendar(icalNow)), "DTSTART:20261011T090000Z")
}
