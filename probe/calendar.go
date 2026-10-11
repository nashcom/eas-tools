package probe

import (
	"context"
	"sort"
	"time"

	"github.com/hstern/go-activesync/eas"
)

// Attendee is a person invited to an event.
type Attendee struct {
	Name   string
	Email  string
	Status int // 0 response unknown, 2 tentative, 3 accepted, 4 declined, 5 not responded
	Type   int // 1 required, 2 optional, 3 resource
}

// Repeat is the repeat rule of an event, as EAS sends it. The rule is not expanded into the single dates.
type Repeat struct {
	Type        int // 0 daily, 1 weekly, 2 monthly by date, 3 monthly by weekday, 5 yearly by date, 6 yearly by weekday
	Interval    int
	DayOfWeek   int // bit mask: 1 Sunday, 2 Monday, 4 Tuesday, 8 Wednesday, 16 Thursday, 32 Friday, 64 Saturday
	DayOfMonth  int
	WeekOfMonth int // 1 to 5, 5 is the last
	MonthOfYear int
	Occurrences int       // the number of instances; 0 when the rule has an end date or none
	Until       time.Time // the last day; zero when the rule has none
}

// EventException is one instance of a repeating event that differs from the rule.
type EventException struct {
	OriginalStart time.Time
	Deleted       bool
	Subject       string
	Location      string
	Start, End    time.Time
}

// Event is a calendar entry. Times are UTC as the server sent them.
type Event struct {
	ID             string
	Folder         string
	UID            string
	Subject        string
	Location       string
	Body           string
	Start, End     time.Time
	AllDay         bool
	Busy           int // 0 free, 1 tentative, 2 busy, 3 out of office, 4 working elsewhere
	Sensitivity    int // 0 normal, 1 personal, 2 private, 3 confidential
	Meeting        int // 0 no meeting, 1 meeting, 3 meeting received, 5 canceled, 7 canceled and received
	Reminder       int // minutes before the start; 0 none
	OrganizerName  string
	OrganizerEmail string
	Attendees      []Attendee
	Repeat         *Repeat
	Exceptions     []EventException
	// Zone is the time zone that the times of the file are written in; nil is UTC.
	Zone *Zone
}

func eventOf(folder string, e eas.EventItem) Event {
	out := Event{
		ID: e.ServerID, Folder: folder, UID: e.UID, Subject: e.Subject, Location: e.Location, Body: e.Body,
		Start: e.StartTime, End: e.EndTime, AllDay: e.AllDayEvent, Busy: e.BusyStatus, Sensitivity: e.Sensitivity,
		Meeting: e.MeetingStatus, Reminder: e.Reminder, OrganizerName: e.OrganizerName, OrganizerEmail: e.OrganizerEmail,
	}
	for _, a := range e.Attendees {
		out.Attendees = append(out.Attendees, Attendee{Name: a.Name, Email: a.Email, Status: a.AttendeeStatus, Type: a.AttendeeType})
	}
	if r := e.Recurrence; r != nil {
		out.Repeat = &Repeat{Type: int(r.Type), Interval: r.Interval, DayOfWeek: int(r.DayOfWeek), DayOfMonth: r.DayOfMonth,
			WeekOfMonth: r.WeekOfMonth, MonthOfYear: r.MonthOfYear, Occurrences: r.Occurrences, Until: r.Until}
	}
	for _, x := range e.Exceptions {
		out.Exceptions = append(out.Exceptions, EventException{OriginalStart: x.ExceptionStartTime, Deleted: x.Deleted,
			Subject: x.Subject, Location: x.Location, Start: x.StartTime, End: x.EndTime})
	}
	return out
}

// ListEvents reads a calendar folder from the start and returns its events, by start time. The date window of the server
// (since, 0 is the default of two weeks) keeps the events that ended within it or have not ended yet, so the upcoming
// events are always in. A repeating event comes once, with its rule.
func (s *Session) ListEvents(ctx context.Context, folder string, since eas.FilterType) ([]Event, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	if err := s.state.SetSyncKey(ctx, folder, "0"); err != nil {
		return nil, err
	}
	var out []Event
	for page := 0; page < mailMaxPages; page++ {
		res, err := s.client.SyncCalendar(ctx, folder, eas.CalendarSyncOptions{WindowSize: mailSyncWindow, DateFilter: since})
		if err != nil {
			return nil, err
		}
		for _, e := range res.Added {
			out = append(out, eventOf(folder, e))
		}
		if !res.MoreAvailable {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}
