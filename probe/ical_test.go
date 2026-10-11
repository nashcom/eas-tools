package probe_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

var icalNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// unfold returns the logical lines of an iCalendar text (RFC 5545 folding undone) and checks the physical ones.
func unfold(t *testing.T, raw []byte) []string {
	t.Helper()
	text := string(raw)
	if !strings.HasSuffix(text, "\r\n") || strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\r") {
		t.Fatalf("lines have to end with CRLF and nothing else:\n%q", text)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("a line is %d bytes, the limit is 75: %q", len(line), line)
		}
		if strings.HasPrefix(line, " ") && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func mustHave(t *testing.T, lines []string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !has(lines, w) {
			t.Errorf("the calendar lacks the line %q in:\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

func TestICalendarOfASimpleEvent(t *testing.T) {
	e := probe.Event{
		ID: "8:12", Folder: "8", UID: "uid-1@example.com", Subject: "Planning, with; special \\ chars", Location: "Room 4",
		Body: "line one\r\nline two", Start: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC),
		Busy: 2, Reminder: 15, OrganizerName: "Carol", OrganizerEmail: "carol@example.com",
		Attendees: []probe.Attendee{{Name: "Bob", Email: "bob@example.com", Status: 3, Type: 1}, {Email: "dave@example.com", Status: 5, Type: 2}, {Name: "Room", Email: "room@example.com", Status: 3, Type: 3}},
	}
	lines := unfold(t, e.ICalendar(icalNow))
	if lines[0] != "BEGIN:VCALENDAR" || lines[len(lines)-1] != "END:VCALENDAR" {
		t.Errorf("not a calendar: %q ... %q", lines[0], lines[len(lines)-1])
	}
	mustHave(t, lines,
		"VERSION:2.0", "CALSCALE:GREGORIAN", "BEGIN:VEVENT", "END:VEVENT",
		"UID:uid-1@example.com", "DTSTAMP:20261010T120000Z",
		"DTSTART:20261011T090000Z", "DTEND:20261011T103000Z",
		`SUMMARY:Planning\, with\; special \\ chars`, "LOCATION:Room 4", `DESCRIPTION:line one\nline two`,
		"TRANSP:OPAQUE", "CLASS:PUBLIC",
		"ORGANIZER;CN=Carol:mailto:carol@example.com",
		"ATTENDEE;CN=Bob;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:bob@example.com",
		"ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION:mailto:dave@example.com",
		"ATTENDEE;CN=Room;CUTYPE=RESOURCE;ROLE=NON-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:room@example.com",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "END:VALARM",
	)
	if len(unfold(t, e.ICalendar(icalNow))) != len(lines) {
		t.Error("two calls give different text")
	}
}

func TestICalendarFoldsLongLinesAndKeepsCharacters(t *testing.T) {
	long := strings.Repeat("äöü ", 60) // 2-byte characters: a fold must not split one
	e := probe.Event{ID: "1", Folder: "8", Subject: long, Start: icalNow, End: icalNow.Add(time.Hour)}
	lines := unfold(t, e.ICalendar(icalNow))
	mustHave(t, lines, "SUMMARY:"+strings.TrimSpace(long)+" ") // unfold checks the limit of 75 bytes
	if !strings.Contains(string(e.ICalendar(icalNow)), "ä") {
		t.Error("characters were changed")
	}
}

func TestICalendarWithoutUIDGetsOne(t *testing.T) {
	e := probe.Event{ID: "8:12", Folder: "8", Subject: "x", Start: icalNow, End: icalNow.Add(time.Hour)}
	mustHave(t, unfold(t, e.ICalendar(icalNow)), "UID:8-8.12@eas-client.invalid")
}

func TestICalendarAllDayAndStatus(t *testing.T) {
	e := probe.Event{ID: "1", Folder: "8", UID: "u", Subject: "Holiday", AllDay: true, Busy: 0, Sensitivity: 2, Meeting: 5,
		Start: time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)}
	lines := unfold(t, e.ICalendar(icalNow))
	mustHave(t, lines, "DTSTART;VALUE=DATE:20261224", "DTEND;VALUE=DATE:20261225", "TRANSP:TRANSPARENT", "CLASS:PRIVATE", "STATUS:CANCELLED")

	same := probe.Event{ID: "1", Folder: "8", UID: "u", AllDay: true, Start: e.Start, End: e.Start}
	mustHave(t, unfold(t, same.ICalendar(icalNow)), "DTSTART;VALUE=DATE:20261224", "DTEND;VALUE=DATE:20261225") // an all-day event lasts a day

	tentative := probe.Event{ID: "1", Folder: "8", UID: "u", Busy: 1, Start: icalNow, End: icalNow.Add(time.Hour)}
	mustHave(t, unfold(t, tentative.ICalendar(icalNow)), "STATUS:TENTATIVE", "TRANSP:OPAQUE")
}

func TestICalendarRepeatRules(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	until := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	for name, c := range map[string]struct {
		repeat probe.Repeat
		want   string
	}{
		"daily":           {probe.Repeat{Type: 0, Interval: 3}, "RRULE:FREQ=DAILY;INTERVAL=3"},
		"weekly":          {probe.Repeat{Type: 1, Interval: 2, DayOfWeek: 2 | 16}, "RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,TH"},
		"weekly until":    {probe.Repeat{Type: 1, Interval: 1, DayOfWeek: 64 | 1, Until: until}, "RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=SU,SA;UNTIL=20261231T235959Z"},
		"count":           {probe.Repeat{Type: 0, Interval: 1, Occurrences: 10}, "RRULE:FREQ=DAILY;INTERVAL=1;COUNT=10"},
		"monthly date":    {probe.Repeat{Type: 2, Interval: 1, DayOfMonth: 15}, "RRULE:FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=15"},
		"monthly nth":     {probe.Repeat{Type: 3, Interval: 1, DayOfWeek: 4, WeekOfMonth: 2}, "RRULE:FREQ=MONTHLY;INTERVAL=1;BYDAY=2TU"},
		"monthly last":    {probe.Repeat{Type: 3, Interval: 1, DayOfWeek: 32, WeekOfMonth: 5}, "RRULE:FREQ=MONTHLY;INTERVAL=1;BYDAY=-1FR"},
		"yearly date":     {probe.Repeat{Type: 5, Interval: 1, MonthOfYear: 3, DayOfMonth: 7}, "RRULE:FREQ=YEARLY;INTERVAL=1;BYMONTH=3;BYMONTHDAY=7"},
		"yearly nth":      {probe.Repeat{Type: 6, Interval: 1, MonthOfYear: 11, DayOfWeek: 16, WeekOfMonth: 4}, "RRULE:FREQ=YEARLY;INTERVAL=1;BYMONTH=11;BYDAY=4TH"},
		"interval of 0":   {probe.Repeat{Type: 0, Interval: 0}, "RRULE:FREQ=DAILY;INTERVAL=1"},
		"unknown, a note": {probe.Repeat{Type: 9, Interval: 1}, "COMMENT:The repeat rule (type 9) is not converted"},
	} {
		r := c.repeat
		e := probe.Event{ID: "1", Folder: "8", UID: "u", Subject: "rep", Start: start, End: start.Add(time.Hour), Repeat: &r}
		lines := unfold(t, e.ICalendar(icalNow))
		if !has(lines, c.want) {
			t.Errorf("%s: want %q in\n%s", name, c.want, strings.Join(lines, "\n"))
		}
		if name == "unknown, a note" {
			for _, l := range lines {
				if strings.HasPrefix(l, "RRULE") {
					t.Errorf("%s: a rule that is not understood was written: %q", name, l)
				}
			}
		}
	}
}

func TestICalendarExceptionsOfARepeatingEvent(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	e := probe.Event{ID: "1", Folder: "8", UID: "series", Subject: "Standup", Location: "Room 1", Start: start, End: start.Add(30 * time.Minute),
		Repeat: &probe.Repeat{Type: 1, Interval: 1, DayOfWeek: 2},
		Exceptions: []probe.EventException{
			{OriginalStart: start.AddDate(0, 0, 7), Deleted: true},
			{OriginalStart: start.AddDate(0, 0, 14), Subject: "Standup (moved)", Start: start.AddDate(0, 0, 14).Add(2 * time.Hour), End: start.AddDate(0, 0, 14).Add(150 * time.Minute)},
		}}
	lines := unfold(t, e.ICalendar(icalNow))
	mustHave(t, lines, "EXDATE:20261019T090000Z", "RECURRENCE-ID:20261026T090000Z", "SUMMARY:Standup (moved)", "DTSTART:20261026T110000Z", "DTEND:20261026T113000Z")
	n := 0
	for _, l := range lines {
		if l == "BEGIN:VEVENT" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d events, want the series and one changed instance", n)
	}
	uids := 0
	for _, l := range lines {
		if l == "UID:series" {
			uids++
		}
	}
	if uids != 2 {
		t.Errorf("the changed instance has to carry the UID of the series: %d lines", uids)
	}
	mustHave(t, lines, "LOCATION:Room 1") // what an instance does not change comes from the series
}
