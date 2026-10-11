package probe

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ICalendar writes the event as an iCalendar file (RFC 5545, a .ics) of one VCALENDAR. EAS sends the fields of an event and no
// iCalendar text, so this is built from the fields: it is what the server tells, in the form every calendar program reads.
//
// Times are UTC, as the server sends them; there is no VTIMEZONE. An all-day event is written with dates. A repeat rule becomes
// an RRULE (a rule that is not understood is left out, with a COMMENT), deleted instances become EXDATE and changed
// instances become events of their own with the same UID and a RECURRENCE-ID. now is the DTSTAMP.
func (e Event) ICalendar(now time.Time) []byte { return e.ICalendarMethod(now, "", 0) }

// ICalendarMethod is ICalendar for a mail: a method (REQUEST, an invitation, or CANCEL) makes it a message of iTIP (RFC 5546), which
// a calendar program shows as an invitation or a cancellation. The attendees are asked to answer and the personal reminder is left
// out. sequence is the number of the version of the meeting (0 for the first, higher after a change or a cancellation). An empty
// method is the plain file.
func (e Event) ICalendarMethod(now time.Time, method string, sequence int) []byte {
	var w icalWriter
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:-//Nash!Com//eas-client " + Version + "//EN")
	w.line("CALSCALE:GREGORIAN")
	if method != "" {
		w.line("METHOD:" + method)
	}
	if e.Zone != nil && !e.Zone.IsUTC() && !e.AllDay {
		w.zone = e.Zone
		w.b.Write(e.Zone.VTimezone(e.Start.In(e.Zone.location()).Year()))
	}

	uid := e.UID
	if uid == "" {
		uid = e.Folder + "-" + strings.ReplaceAll(e.ID, ":", ".") + "@eas-client.invalid"
	}

	w.line("BEGIN:VEVENT")
	w.text("UID", uid)
	w.line("DTSTAMP:" + icalTime(now))
	start, end := e.Start, e.End
	if e.AllDay && !end.After(start) {
		end = start.AddDate(0, 0, 1) // an all-day event lasts a day at least
	}
	w.when("DTSTART", start, e.AllDay)
	w.when("DTEND", end, e.AllDay)
	w.text("SUMMARY", e.Subject)
	if e.Location != "" {
		w.text("LOCATION", e.Location)
	}
	if e.Body != "" {
		w.text("DESCRIPTION", e.Body)
	}
	if e.Busy == 0 {
		w.line("TRANSP:TRANSPARENT")
	} else {
		w.line("TRANSP:OPAQUE")
	}
	switch {
	case method == "CANCEL" || (method == "" && (e.Meeting == 5 || e.Meeting == 7)):
		w.line("STATUS:CANCELLED")
	case method == "REQUEST":
		w.line("STATUS:CONFIRMED")
	case e.Busy == 1:
		w.line("STATUS:TENTATIVE")
	}
	if method != "" {
		w.line(fmt.Sprintf("SEQUENCE:%d", sequence))
	}
	switch e.Sensitivity {
	case 1, 2:
		w.line("CLASS:PRIVATE")
	case 3:
		w.line("CLASS:CONFIDENTIAL")
	default:
		w.line("CLASS:PUBLIC")
	}
	if e.OrganizerEmail != "" {
		w.line("ORGANIZER" + icalParam("CN", e.OrganizerName) + ":mailto:" + e.OrganizerEmail)
	}
	for _, a := range e.Attendees {
		if a.Email == "" {
			continue
		}
		params := icalParam("CN", a.Name)
		role := "REQ-PARTICIPANT"
		switch a.Type {
		case 2:
			role = "OPT-PARTICIPANT"
		case 3:
			params += ";CUTYPE=RESOURCE"
			role = "NON-PARTICIPANT"
		}
		status := "NEEDS-ACTION"
		switch a.Status {
		case 2:
			status = "TENTATIVE"
		case 3:
			status = "ACCEPTED"
		case 4:
			status = "DECLINED"
		}
		rsvp := ""
		if method != "" {
			status = "NEEDS-ACTION"
		}
		if method == "REQUEST" {
			rsvp = ";RSVP=TRUE"
		}
		w.line("ATTENDEE" + params + ";ROLE=" + role + ";PARTSTAT=" + status + rsvp + ":mailto:" + a.Email)
	}
	if r := e.Repeat; r != nil {
		if rule, ok := icalRule(r, e.AllDay); ok {
			w.line("RRULE:" + rule)
		} else {
			w.line(fmt.Sprintf("COMMENT:The repeat rule (type %d) is not converted", r.Type))
		}
		for _, x := range e.Exceptions {
			if x.Deleted {
				w.whenWithName("EXDATE", x.OriginalStart, e.AllDay)
			}
		}
	}
	if e.Reminder > 0 && method == "" {
		w.line("BEGIN:VALARM")
		w.line("ACTION:DISPLAY")
		w.line("DESCRIPTION:Reminder")
		w.line(fmt.Sprintf("TRIGGER:-PT%dM", e.Reminder))
		w.line("END:VALARM")
	}
	w.line("END:VEVENT")

	// a changed instance of a repeating event is an event of its own
	if e.Repeat != nil {
		for _, x := range e.Exceptions {
			if x.Deleted {
				continue
			}
			s, en := x.Start, x.End
			if s.IsZero() {
				s = x.OriginalStart
			}
			if en.IsZero() {
				en = s.Add(e.End.Sub(e.Start))
			}
			subject, location := e.Subject, e.Location
			if x.Subject != "" {
				subject = x.Subject
			}
			if x.Location != "" {
				location = x.Location
			}
			w.line("BEGIN:VEVENT")
			w.text("UID", uid)
			w.line("DTSTAMP:" + icalTime(now))
			w.whenWithName("RECURRENCE-ID", x.OriginalStart, e.AllDay)
			w.when("DTSTART", s, e.AllDay)
			w.when("DTEND", en, e.AllDay)
			w.text("SUMMARY", subject)
			if location != "" {
				w.text("LOCATION", location)
			}
			w.line("END:VEVENT")
		}
	}
	w.line("END:VCALENDAR")
	return []byte(w.b.String())
}

func icalTime(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// icalRule writes the repeat rule as the value of an RRULE; false means the rule is not one that is understood.
func icalRule(r *Repeat, allDay bool) (string, bool) {
	interval := max(r.Interval, 1)
	var parts []string
	days := icalDays(r.DayOfWeek, r.WeekOfMonth)
	switch r.Type {
	case 0:
		parts = []string{"FREQ=DAILY", fmt.Sprintf("INTERVAL=%d", interval)}
	case 1:
		parts = []string{"FREQ=WEEKLY", fmt.Sprintf("INTERVAL=%d", interval)}
		if d := icalDays(r.DayOfWeek, 0); d != "" {
			parts = append(parts, "BYDAY="+d)
		}
	case 2:
		parts = []string{"FREQ=MONTHLY", fmt.Sprintf("INTERVAL=%d", interval)}
		if r.DayOfMonth > 0 {
			parts = append(parts, fmt.Sprintf("BYMONTHDAY=%d", r.DayOfMonth))
		}
	case 3:
		if days == "" {
			return "", false
		}
		parts = []string{"FREQ=MONTHLY", fmt.Sprintf("INTERVAL=%d", interval), "BYDAY=" + days}
	case 5:
		parts = []string{"FREQ=YEARLY", fmt.Sprintf("INTERVAL=%d", interval)}
		if r.MonthOfYear > 0 {
			parts = append(parts, fmt.Sprintf("BYMONTH=%d", r.MonthOfYear))
		}
		if r.DayOfMonth > 0 {
			parts = append(parts, fmt.Sprintf("BYMONTHDAY=%d", r.DayOfMonth))
		}
	case 6:
		if days == "" {
			return "", false
		}
		parts = []string{"FREQ=YEARLY", fmt.Sprintf("INTERVAL=%d", interval)}
		if r.MonthOfYear > 0 {
			parts = append(parts, fmt.Sprintf("BYMONTH=%d", r.MonthOfYear))
		}
		parts = append(parts, "BYDAY="+days)
	default:
		return "", false
	}
	switch {
	case !r.Until.IsZero():
		if allDay {
			parts = append(parts, "UNTIL="+r.Until.UTC().Format("20060102"))
		} else {
			parts = append(parts, "UNTIL="+icalTime(r.Until))
		}
	case r.Occurrences > 0:
		parts = append(parts, fmt.Sprintf("COUNT=%d", r.Occurrences))
	}
	return strings.Join(parts, ";"), true
}

// icalDays writes the days of a bit mask (1 Sunday ... 64 Saturday) as MO,TU; with a week of the month (1 to 5, 5 is the last)
// each day gets it as a number: 2TU, -1FR.
func icalDays(mask, week int) string {
	prefix := ""
	switch {
	case week == 5:
		prefix = "-1"
	case week > 0:
		prefix = fmt.Sprint(week)
	}
	var out []string
	for i, d := range []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"} {
		if mask&(1<<i) != 0 {
			out = append(out, prefix+d)
		}
	}
	return strings.Join(out, ",")
}

// icalParam is ";NAME=value" for a parameter, quoted when it needs to be; nothing for an empty value.
func icalParam(name, value string) string {
	if value == "" {
		return ""
	}
	value = strings.NewReplacer(`"`, "", "\r", " ", "\n", " ").Replace(value)
	if strings.ContainsAny(value, ";:,") {
		value = `"` + value + `"`
	}
	return ";" + name + "=" + value
}

// icalWriter builds the lines of a calendar, folded at 75 bytes and ended with CRLF.
type icalWriter struct {
	b strings.Builder
	// zone, when set, makes the times local times of this zone (with a TZID) and not UTC
	zone *Zone
}

func (w *icalWriter) line(s string) {
	limit := 75 // the first line; a continuation line starts with a blank, so it carries 74 bytes
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) { // a fold must not split a character
			cut--
		}
		w.b.WriteString(s[:cut] + "\r\n ")
		s = s[cut:]
		limit = 74
	}
	w.b.WriteString(s + "\r\n")
}

// text writes a property with a TEXT value: backslash, semicolon, comma and line breaks are escaped.
func (w *icalWriter) text(name, value string) {
	value = strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(value)
	w.line(name + ":" + value)
}

func (w *icalWriter) when(name string, t time.Time, allDay bool) { w.whenWithName(name, t, allDay) }

func (w *icalWriter) whenWithName(name string, t time.Time, allDay bool) {
	if allDay {
		w.line(name + ";VALUE=DATE:" + t.UTC().Format("20060102"))
		return
	}
	if w.zone != nil {
		param := w.zone.TZID()
		if strings.ContainsAny(param, ";:,") {
			param = `"` + param + `"`
		}
		w.line(name + ";TZID=" + param + ":" + t.In(w.zone.location()).Format("20060102T150405"))
		return
	}
	w.line(name + ":" + icalTime(t))
}
