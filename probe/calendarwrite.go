package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/hstern/go-activesync/wbxml"
)

// EventDraft is a calendar entry to create. With attendees it is a meeting: the account is its organizer, and the server
// sends the invitations (or not: that is up to the server).
type EventDraft struct {
	Subject     string
	Location    string
	Body        string
	Start, End  time.Time // an all-day event: midnight UTC of its first day, and midnight of the day after its last
	AllDay      bool
	Busy        int // 0 free, 1 tentative, 2 busy, 3 out of office, 4 working elsewhere
	Sensitivity int // 0 normal, 1 personal, 2 private, 3 confidential
	Reminder    int // minutes before the start; 0 none
	// UID names the entry in every calendar and in the invitations; empty gets a new one (see NewUID).
	UID string
	// Zone is the time zone of the user: the entry carries it with its rules of daylight saving time. nil is UTC. The times of the
	// entry are UTC on the wire whatever the zone is.
	Zone      *Zone
	Attendees []Attendee
}

func (d EventDraft) validate() error {
	switch {
	case strings.TrimSpace(d.Subject) == "":
		return errors.New("an event needs a subject")
	case d.Start.IsZero() || d.End.IsZero():
		return errors.New("an event needs a start and an end")
	case !d.End.After(d.Start):
		return errors.New("the end of an event has to be after its start")
	}
	for _, a := range d.Attendees {
		if p, err := mail.ParseAddress(a.Email); err != nil || !strings.Contains(p.Address, "@") {
			return fmt.Errorf("attendee %q is not an e-mail address", a.Email)
		}
	}
	return nil
}

// easCompact is the time format of ActiveSync: 20261020T090000Z.
func easCompact(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// eventApplicationData is the ApplicationData of a new calendar entry, in the order of the Calendar schema of MS-ASCAL. The
// time zone is UTC: the times are exact, but the entry has no daylight saving rule.
func (s *Session) eventApplicationData(d EventDraft, now time.Time) (*wbxml.Element, error) {
	c := func(name, v string) *wbxml.Element { return wbxml.E(wbxml.PageCalendar, name, wbxml.Text(v)) }
	app := wbxml.E(wbxml.PageAirSync, "ApplicationData",
		c("TimeZone", zoneOf(d).EASBlob(d.Start.In(zoneOf(d).location()).Year())),
		c("AllDayEvent", map[bool]string{true: "1", false: "0"}[d.AllDay]),
		c("BusyStatus", strconv.Itoa(d.Busy)),
		c("DTStamp", easCompact(now)),
		c("EndTime", easCompact(d.End)),
	)
	if d.Location != "" {
		app.Children = append(app.Children, c("Location", d.Location))
	}
	if len(d.Attendees) > 0 {
		self := s.selfAddress()
		if self == "" {
			return nil, errors.New("the account has no e-mail address to organize a meeting from")
		}
		app.Children = append(app.Children, c("MeetingStatus", "1"), c("OrganizerEmail", self))
	}
	if d.Reminder > 0 {
		app.Children = append(app.Children, c("Reminder", strconv.Itoa(d.Reminder)))
	}
	app.Children = append(app.Children, c("Sensitivity", strconv.Itoa(d.Sensitivity)), c("Subject", d.Subject), c("StartTime", easCompact(d.Start)))
	if d.UID != "" {
		app.Children = append(app.Children, c("UID", d.UID))
	}
	if len(d.Attendees) > 0 {
		att := wbxml.E(wbxml.PageCalendar, "Attendees")
		for _, a := range d.Attendees {
			at := wbxml.E(wbxml.PageCalendar, "Attendee", c("Email", a.Email))
			if a.Name != "" {
				at.Children = append(at.Children, c("Name", a.Name))
			}
			typ := a.Type
			if typ == 0 {
				typ = 1 // required
			}
			at.Children = append(at.Children, c("AttendeeType", strconv.Itoa(typ)))
			att.Children = append(att.Children, at)
		}
		app.Children = append(app.Children, att)
	}
	if d.Body != "" {
		app.Children = append(app.Children, wbxml.E(wbxml.PageAirSyncBase, "Body",
			wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text("1")),
			wbxml.E(wbxml.PageAirSyncBase, "Data", wbxml.Text(d.Body)),
		))
	}
	return app, nil
}

// calendarCommand sends a Sync command to a calendar folder. The folder needs a current sync key, so a folder that was not read
// yet is read first, and a key that the server refuses (status 3) is renewed once.
func (s *Session) calendarCommand(ctx context.Context, folder, ref string, command *wbxml.Element) (*wbxml.Element, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	if key, _ := s.state.SyncKey(ctx, folder); key == "" || key == "0" {
		if _, err := s.ListEvents(ctx, folder, 0); err != nil {
			return nil, err
		}
	}
	s.rec.setStep(StepSync)
	coll, err := s.syncCommand(ctx, folder, ref, command)
	if statusIs(err, "Sync", 3) {
		s.log.Warn("Sync status 3 while changing the calendar: reading the calendar again for a new sync key")
		if _, lerr := s.ListEvents(ctx, folder, 0); lerr != nil {
			return nil, lerr
		}
		s.rec.setStep(StepSync)
		coll, err = s.syncCommand(ctx, folder, ref, command)
	}
	return coll, err
}

// CreateEvent adds an event to a calendar folder and returns its server id. The Sync request is made here, with the Options that
// the Sync of the mail has, because a Traveler refused the requests of the EAS library without them (see syncContactsOnce).
func (s *Session) CreateEvent(ctx context.Context, folder string, d EventDraft) (string, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	if d.UID == "" {
		d.UID = NewUID()
	}
	app, err := s.eventApplicationData(d, time.Now())
	if err != nil {
		return "", err
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	clientID := hex.EncodeToString(rnd[:])
	add := wbxml.E(wbxml.PageAirSync, "Add", wbxml.E(wbxml.PageAirSync, "ClientId", wbxml.Text(clientID)), app)

	// the entries that are there before: a Traveler answers the Add with the client id as the id of the entry, and the real id is the
	// one that the new entry has when the calendar is read again
	before, err := s.ListEvents(ctx, folder, eas.FilterSixMonth)
	if err != nil {
		return "", err
	}
	coll, err := s.calendarCommand(ctx, folder, "the new event", add)
	if err != nil {
		return "", err
	}
	answered := ""
	if coll != nil {
		if responses := coll.Find("Responses"); responses != nil {
			for _, r := range responses.Children {
				if el, ok := r.(*wbxml.Element); ok && el.Name == "Add" && childText(el, "ClientId") == clientID {
					answered = childText(el, "ServerId")
				}
			}
		}
	}
	if answered == "" {
		return "", errors.New("the server did not say which id the new event has")
	}
	return s.realEventID(ctx, folder, d, answered, before), nil
}

// realEventID finds the id that the new entry has in the calendar: the answer of the server when the calendar knows it, else the one new
// entry, else the one with the UID, else the one with the subject and times.
func (s *Session) realEventID(ctx context.Context, folder string, d EventDraft, answered string, before []Event) string {
	after, err := s.ListEvents(ctx, folder, eas.FilterSixMonth)
	if err != nil {
		s.log.Warn("the new event could not be looked up", "error", err.Error())
		return answered
	}
	known := map[string]bool{}
	for _, e := range before {
		known[e.ID] = true
	}
	var fresh, byUID, byTime []string
	for _, e := range after {
		if e.ID == answered {
			return answered
		}
		if !known[e.ID] {
			fresh = append(fresh, e.ID)
		}
		if d.UID != "" && e.UID == d.UID {
			byUID = append(byUID, e.ID)
		}
		if e.Subject == d.Subject && e.Start.Equal(d.Start) && e.End.Equal(d.End) {
			byTime = append(byTime, e.ID)
		}
	}
	for _, ids := range [][]string{fresh, byUID, byTime} {
		if len(ids) == 1 {
			return ids[0]
		}
	}
	s.log.Warn("the id of the new event is not certain", "answered", answered, "new", len(fresh))
	return answered
}

// DeleteEvent deletes an event of a calendar folder. For a meeting that the account organizes the server may tell the attendees
// that it is canceled.
func (s *Session) DeleteEvent(ctx context.Context, folder, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("the id of the event is required")
	}
	del := wbxml.E(wbxml.PageAirSync, "Delete", wbxml.E(wbxml.PageAirSync, "ServerId", wbxml.Text(id)))
	_, err := s.calendarCommand(ctx, folder, id, del)
	return err
}

func zoneOf(d EventDraft) Zone {
	if d.Zone == nil {
		return Zone{Name: "UTC", Loc: time.UTC}
	}
	return *d.Zone
}
