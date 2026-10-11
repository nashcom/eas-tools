package easmockserver

import (
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// FolderCalendar is the calendar folder of the mailbox mode.
const FolderCalendar = "8"

// Attendee is a person invited to an Event.
type Attendee struct {
	Name, Email string
	Status      int // 0 response unknown, 2 tentative, 3 accepted, 4 declined, 5 not responded
	Type        int // 1 required, 2 optional, 3 resource
}

// Repeat is the repeat rule of an Event (the recurrence of EAS).
type Repeat struct {
	Type        int // 0 daily, 1 weekly, 2 monthly by date, 3 monthly by weekday, 5 yearly by date, 6 yearly by weekday
	Interval    int
	DayOfWeek   int // bit mask: 1 Sunday, 2 Monday, 4 Tuesday ...
	DayOfMonth  int
	WeekOfMonth int
	Until       time.Time // zero: forever
}

// Event is a calendar entry for SeedEvent (Options.Mailbox only).
type Event struct {
	Folder        string // default: the calendar
	UID           string
	Subject       string
	Location      string
	Body          string
	Start, End    time.Time
	AllDay        bool
	Busy          int // 0 free, 1 tentative, 2 busy, 3 out of office, 4 working elsewhere
	Sensitivity   int
	Meeting       int // 0 no meeting, 1 meeting, 3 received, 5 canceled
	Reminder      int
	Organizer     string // address
	OrganizerName string
	Attendees     []Attendee
	Repeat        *Repeat
	TimeZone      string // the TimeZone of an entry that was added: the base64 text the client sent
}

// SeedEvent puts an event into a calendar of a user and returns its server id.
func (f *Server) SeedEvent(user string, e Event) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if e.Folder == "" {
		e.Folder = FolderCalendar
	}
	// the date window of a Sync filters a calendar by the end of the event; a series is in as long as it runs
	at := e.End
	if e.Repeat != nil {
		at = e.Repeat.Until
		if at.IsZero() {
			at = time.Now().AddDate(10, 0, 0)
		}
	}
	return f.boxPut(user, &boxMsg{folder: e.Folder, subject: e.Subject, body: e.Body, at: at, ev: &e})
}

func easTime(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// eventData is the ApplicationData of a calendar entry.
func eventData(m *boxMsg) *wbxml.Element {
	e := m.ev
	c := func(name, v string) *wbxml.Element { return txt(wbxml.PageCalendar, name, v) }
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	out := wbxml.E(wbxml.PageAirSync, "ApplicationData",
		c("StartTime", easTime(e.Start)),
		c("EndTime", easTime(e.End)),
		c("Subject", e.Subject),
		c("Location", e.Location),
		c("UID", e.UID),
		c("AllDayEvent", b(e.AllDay)),
		c("BusyStatus", strconv.Itoa(e.Busy)),
		c("Sensitivity", strconv.Itoa(e.Sensitivity)),
		c("MeetingStatus", strconv.Itoa(e.Meeting)),
		c("Reminder", strconv.Itoa(e.Reminder)),
		c("OrganizerName", e.OrganizerName),
		c("OrganizerEmail", e.Organizer),
	)
	if len(e.Attendees) > 0 {
		at := wbxml.E(wbxml.PageCalendar, "Attendees")
		for _, a := range e.Attendees {
			at.Children = append(at.Children, wbxml.E(wbxml.PageCalendar, "Attendee",
				c("Email", a.Email), c("Name", a.Name),
				c("AttendeeStatus", strconv.Itoa(a.Status)), c("AttendeeType", strconv.Itoa(a.Type))))
		}
		out.Children = append(out.Children, at)
	}
	if r := e.Repeat; r != nil {
		rec := wbxml.E(wbxml.PageCalendar, "Recurrence",
			c("Recurrence_Type", strconv.Itoa(r.Type)), c("Recurrence_Interval", strconv.Itoa(max(r.Interval, 1))))
		if r.DayOfWeek > 0 {
			rec.Children = append(rec.Children, c("Recurrence_DayOfWeek", strconv.Itoa(r.DayOfWeek)))
		}
		if r.DayOfMonth > 0 {
			rec.Children = append(rec.Children, c("Recurrence_DayOfMonth", strconv.Itoa(r.DayOfMonth)))
		}
		if r.WeekOfMonth > 0 {
			rec.Children = append(rec.Children, c("Recurrence_WeekOfMonth", strconv.Itoa(r.WeekOfMonth)))
		}
		if !r.Until.IsZero() {
			rec.Children = append(rec.Children, c("Recurrence_Until", easTime(r.Until)))
		}
		out.Children = append(out.Children, rec)
	}
	if e.Body != "" {
		out.Children = append(out.Children, wbxml.E(wbxml.PageAirSyncBase, "Body",
			txt(wbxml.PageAirSyncBase, "Type", "1"),
			txt(wbxml.PageAirSyncBase, "EstimatedDataSize", strconv.Itoa(len(e.Body))),
			txt(wbxml.PageAirSyncBase, "Data", e.Body)))
	}
	return out
}

// responsesOf wraps the answers to Add commands in the Responses element of a Sync reply.
func responsesOf(answers []wbxml.Node) []wbxml.Node {
	if len(answers) == 0 {
		return nil
	}
	r := wbxml.E(wbxml.PageAirSync, "Responses")
	r.Children = append(r.Children, answers...)
	return []wbxml.Node{r}
}

// boxAddEvent stores the calendar entry of an Add command and answers it; call with f.mu held.
func (f *Server) boxAddEvent(user, deviceID, folder string, el *wbxml.Element) wbxml.Node {
	text := func(e *wbxml.Element, name string) string {
		if e == nil {
			return ""
		}
		if v := e.Find(name); v != nil {
			return v.TextContent()
		}
		return ""
	}
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	app := el.Find("ApplicationData")
	start, _ := time.Parse("20060102T150405Z", text(app, "StartTime"))
	end, _ := time.Parse("20060102T150405Z", text(app, "EndTime"))
	ev := &Event{
		Folder: folder, Subject: text(app, "Subject"), Location: text(app, "Location"), Start: start, End: end,
		AllDay: text(app, "AllDayEvent") == "1", Busy: num(text(app, "BusyStatus")), Sensitivity: num(text(app, "Sensitivity")),
		Meeting: num(text(app, "MeetingStatus")), Reminder: num(text(app, "Reminder")),
		Organizer: text(app, "OrganizerEmail"), OrganizerName: text(app, "OrganizerName"),
		UID: text(app, "UID"), TimeZone: text(app, "TimeZone"),
	}
	if b := app.Find("Body"); b != nil {
		ev.Body = text(b, "Data")
	}
	if app != nil {
		for _, a := range app.FindAll("Attendee") {
			ev.Attendees = append(ev.Attendees, Attendee{Name: text(a, "Name"), Email: text(a, "Email"), Type: num(text(a, "AttendeeType"))})
		}
	}
	id := f.boxPut(user, &boxMsg{folder: folder, subject: ev.Subject, body: ev.Body, at: ev.End, ev: ev})

	if f.opts.SendInvitations {
		for _, a := range ev.Attendees {
			for u := range f.users {
				if a.Email != "" && strings.EqualFold(addressOf(u), a.Email) {
					f.boxPut(u, &boxMsg{folder: FolderInbox, subject: "Invitation: " + ev.Subject, from: ev.Organizer, to: a.Email, at: time.Now()})
				}
			}
		}
	}
	answerID := id
	if f.opts.EchoClientID {
		answerID = text(el, "ClientId")
	}
	return wbxml.E(wbxml.PageAirSync, "Add",
		txt(wbxml.PageAirSync, "ClientId", text(el, "ClientId")),
		txt(wbxml.PageAirSync, "ServerId", answerID),
		txt(wbxml.PageAirSync, "Status", "1"))
}

// EventOf returns the calendar entry of a user with the id, as the server holds it.
func (f *Server) EventOf(user, id string) (Event, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, m := f.boxFind(user, id); m != nil && m.ev != nil {
		return *m.ev, true
	}
	return Event{}, false
}
