package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/probe"
)

// Event is a calendar entry. Times are UTC as the server sent them; an all-day event starts and ends at midnight.
type Event struct {
	ID              string      `json:"id"`
	Folder          string      `json:"folder"`
	UID             string      `json:"uid,omitempty"`
	Subject         string      `json:"subject"`
	Location        string      `json:"location,omitempty"`
	Start           *Time       `json:"start"`
	End             *Time       `json:"end"`
	AllDay          bool        `json:"all_day"`
	Busy            string      `json:"busy"`        // free, tentative, busy, out_of_office, working_elsewhere
	Sensitivity     string      `json:"sensitivity"` // normal, personal, private, confidential
	Meeting         string      `json:"meeting"`     // none, meeting, received, canceled, canceled_received
	ReminderMinutes int         `json:"reminder_minutes"`
	Organizer       *Person     `json:"organizer,omitempty"`
	Attendees       []Attendee  `json:"attendees,omitempty"`
	Recurring       bool        `json:"recurring"`
	Repeat          *Repeat     `json:"repeat,omitempty"`
	Exceptions      []Exception `json:"exceptions,omitempty"`
	Body            string      `json:"body,omitempty"`
}

type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type Attendee struct {
	Name   string `json:"name,omitempty"`
	Email  string `json:"email"`
	Status string `json:"status"` // unknown, tentative, accepted, declined, not_responded
	Type   string `json:"type"`   // required, optional, resource
}

// Repeat is the repeat rule of a repeating event. The rule is not expanded into the single dates: the start of the event
// is its first instance.
type Repeat struct {
	Type        string   `json:"type"` // daily, weekly, monthly, monthly_nth, yearly, yearly_nth
	Interval    int      `json:"interval"`
	DaysOfWeek  []string `json:"days_of_week,omitempty"`
	DayOfMonth  int      `json:"day_of_month,omitempty"`
	WeekOfMonth int      `json:"week_of_month,omitempty"` // 1 to 5, 5 is the last
	MonthOfYear int      `json:"month_of_year,omitempty"`
	Occurrences int      `json:"occurrences,omitempty"`
	Until       *Time    `json:"until,omitempty"`
}

// Exception is one instance of a repeating event that differs from the rule.
type Exception struct {
	OriginalStart *Time  `json:"original_start"`
	Deleted       bool   `json:"deleted"`
	Subject       string `json:"subject,omitempty"`
	Location      string `json:"location,omitempty"`
	Start         *Time  `json:"start,omitempty"`
	End           *Time  `json:"end,omitempty"`
}

// EventsResult is the answer of "calendar list".
type EventsResult struct {
	Account string `json:"account"`
	Folder  Folder `json:"folder"`
	// From and To are the window of the list; To is empty when the list has no end. Since is the date window that was asked
	// of the server, which is wider than From when From is in the past.
	From   *Time   `json:"from"`
	To     *Time   `json:"to,omitempty"`
	Since  string  `json:"since"`
	Count  int     `json:"count"`
	Events []Event `json:"events"`
}

// EventResult is the answer of "calendar get".
type EventResult struct {
	Account string `json:"account"`
	Event   Event  `json:"event"`
}

// EventParams shape a calendar list.
type EventParams struct {
	// From is the start of the window: a date (2026-10-10, UTC), a time (RFC 3339) or "now", which is the default. A
	// running event and the upcoming ones are in; with an earlier From so are the past ones.
	From string `json:"from"`
	// To is the end of the window; empty means no end.
	To string `json:"to"`
	// Since is the date window asked of the server (see ParseSince). The default covers From.
	Since string `json:"since"`
	// Limit is the number of events (default 50; -1 all).
	Limit int `json:"limit"`
}

var busyNames = []string{"free", "tentative", "busy", "out_of_office", "working_elsewhere"}
var sensitivityNames = []string{"normal", "personal", "private", "confidential"}

func nameOf(names []string, n int) string {
	if n >= 0 && n < len(names) {
		return names[n]
	}
	return fmt.Sprintf("unknown_%d", n)
}

func meetingName(n int) string {
	switch n {
	case 0:
		return "none"
	case 1:
		return "meeting"
	case 3:
		return "received"
	case 5:
		return "canceled"
	case 7:
		return "canceled_received"
	}
	return fmt.Sprintf("unknown_%d", n)
}

func attendeeStatus(n int) string {
	switch n {
	case 0:
		return "unknown"
	case 2:
		return "tentative"
	case 3:
		return "accepted"
	case 4:
		return "declined"
	case 5:
		return "not_responded"
	}
	return fmt.Sprintf("unknown_%d", n)
}

func attendeeType(n int) string {
	switch n {
	case 1:
		return "required"
	case 2:
		return "optional"
	case 3:
		return "resource"
	}
	return fmt.Sprintf("unknown_%d", n)
}

func repeatType(n int) string {
	switch n {
	case 0:
		return "daily"
	case 1:
		return "weekly"
	case 2:
		return "monthly"
	case 3:
		return "monthly_nth"
	case 5:
		return "yearly"
	case 6:
		return "yearly_nth"
	}
	return fmt.Sprintf("unknown_%d", n)
}

func daysOfWeek(mask int) []string {
	var out []string
	for i, d := range []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"} {
		if mask&(1<<i) != 0 {
			out = append(out, d)
		}
	}
	return out
}

func eventJSON(e probe.Event) Event {
	out := Event{
		ID: e.ID, Folder: e.Folder, UID: e.UID, Subject: e.Subject, Location: e.Location, Start: stamp(e.Start), End: stamp(e.End),
		AllDay: e.AllDay, Busy: nameOf(busyNames, e.Busy), Sensitivity: nameOf(sensitivityNames, e.Sensitivity),
		Meeting: meetingName(e.Meeting), ReminderMinutes: e.Reminder, Body: e.Body, Recurring: e.Repeat != nil,
	}
	if e.OrganizerEmail != "" || e.OrganizerName != "" {
		out.Organizer = &Person{Name: e.OrganizerName, Email: e.OrganizerEmail}
	}
	for _, a := range e.Attendees {
		out.Attendees = append(out.Attendees, Attendee{Name: a.Name, Email: a.Email, Status: attendeeStatus(a.Status), Type: attendeeType(a.Type)})
	}
	if r := e.Repeat; r != nil {
		out.Repeat = &Repeat{Type: repeatType(r.Type), Interval: r.Interval, DaysOfWeek: daysOfWeek(r.DayOfWeek), DayOfMonth: r.DayOfMonth,
			WeekOfMonth: r.WeekOfMonth, MonthOfYear: r.MonthOfYear, Occurrences: r.Occurrences, Until: stamp(r.Until)}
	}
	for _, x := range e.Exceptions {
		out.Exceptions = append(out.Exceptions, Exception{OriginalStart: stamp(x.OriginalStart), Deleted: x.Deleted, Subject: x.Subject,
			Location: x.Location, Start: stamp(x.Start), End: stamp(x.End)})
	}
	return out
}

// parseWhen reads a point in time: "now", a date (UTC midnight) or an RFC 3339 time.
func parseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "now" {
		return now, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use now, 2026-10-10 or 2026-10-10T09:00:00Z", s)
}

// sinceFor is the smallest date window of the server that reaches back to from.
func sinceFor(from, now time.Time) string {
	age := now.Sub(from)
	switch {
	case age <= 14*24*time.Hour:
		return "2w"
	case age <= 30*24*time.Hour:
		return "1m"
	case age <= 90*24*time.Hour:
		return "3m"
	}
	return "6m"
}

// inWindow tells whether an event belongs to the list from from to to (zero: no end). A repeating event is not expanded: it
// is in when its rule is still running and has started by to.
func inWindow(e probe.Event, from, to time.Time) bool {
	if !to.IsZero() && e.Start.After(to) {
		return false
	}
	if r := e.Repeat; r != nil {
		return r.Until.IsZero() || !r.Until.Before(from)
	}
	return !e.End.Before(from)
}

// Events lists the events of a calendar in a window, by start time. Without a folder the calendar of the mailbox is used.
func (m *Manager) Events(ctx context.Context, account, folder string, p EventParams) (*EventsResult, error) {
	now := time.Now()
	ent, err := m.resolve(account)
	if err != nil {
		return nil, err
	}
	loc := ent.zone.Location()
	from, err := parseWhen(p.From, now, loc)
	if err != nil {
		return nil, newError(http.StatusBadRequest, "bad_request", "from: "+err.Error())
	}
	var to time.Time
	if p.To != "" {
		if to, err = parseWhen(p.To, now, loc); err != nil {
			return nil, newError(http.StatusBadRequest, "bad_request", "to: "+err.Error())
		}
		if to.Before(from) {
			return nil, newError(http.StatusBadRequest, "bad_request", "to is before from")
		}
	}
	since := p.Since
	if since == "" {
		since = sinceFor(from, now)
	}
	filter, err := probe.ParseSince(since)
	if err != nil {
		return nil, newError(http.StatusBadRequest, "bad_request", err.Error())
	}
	limit := p.Limit
	if limit == 0 {
		limit = 50
	}

	var out *EventsResult
	err = m.with(ctx, account, func(a Account, s *probe.Session) error {
		if folder == "" {
			folder = "calendar"
		}
		f, err := folderOf(s, folder)
		if err != nil {
			return err
		}
		events, err := s.ListEvents(ctx, f.ID, filter)
		if err != nil {
			return err
		}
		out = &EventsResult{Account: a.Name, Folder: folderJSON(f), From: stamp(from), To: stamp(to), Since: probe.SinceName(filter), Events: []Event{}}
		for _, e := range events {
			if !inWindow(e, from, to) {
				continue
			}
			if limit > 0 && len(out.Events) >= limit {
				break
			}
			out.Events = append(out.Events, eventJSON(e))
		}
		out.Count = len(out.Events)
		return nil
	})
	return out, err
}

// Event returns one event with everything the server sends. The event is looked for in the widest date window.
func (m *Manager) Event(ctx context.Context, account, folder, id string) (*EventResult, error) {
	var out *EventResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		e, err := findEvent(ctx, s, folder, id)
		if err != nil {
			return err
		}
		out = &EventResult{Account: a.Name, Event: eventJSON(e)}
		return nil
	})
	return out, err
}

// findEvent reads the calendar in the widest date window and returns the event with the id.
func findEvent(ctx context.Context, s *probe.Session, folder, id string) (probe.Event, error) {
	f, err := folderForMail(s, folder, id)
	if err != nil {
		return probe.Event{}, err
	}
	events, err := s.ListEvents(ctx, f.ID, eas.FilterSixMonth)
	if err != nil {
		return probe.Event{}, err
	}
	for _, e := range events {
		if e.ID == id {
			return e, nil
		}
	}
	return probe.Event{}, newError(http.StatusNotFound, "event_not_found", fmt.Sprintf("the calendar has no event %s within the last 6 months or later", id))
}

// ICS returns the event as an iCalendar file (a .ics), built from the fields the server sends: the server has no iCalendar
// text to give. Times are UTC. See probe.Event.ICalendar for what is converted.
func (m *Manager) ICS(ctx context.Context, account, folder, id string) (*Raw, error) {
	var out *Raw
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		e, err := findEvent(ctx, s, folder, id)
		if err != nil {
			return err
		}
		out = &Raw{Data: e.ICalendar(time.Now()), ContentType: "text/calendar; charset=utf-8", Filename: fileName(e.Folder, e.ID) + ".ics"}
		return nil
	})
	return out, err
}
