package client

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// EventRequest is an event to create, as the REST API receives it and as the command line builds it.
type EventRequest struct {
	Subject  string `json:"subject"`
	Location string `json:"location,omitempty"`
	Body     string `json:"body,omitempty"`
	// Start and End are a time (RFC 3339, UTC if no zone is given) or a date. For an all-day event they are dates, and End is the
	// last day (default: the start day).
	Start string `json:"start"`
	End   string `json:"end,omitempty"`
	// Duration is the length instead of End, for example 90m or 2h (default: one hour; ignored for an all-day event).
	Duration        string `json:"duration,omitempty"`
	AllDay          bool   `json:"all_day,omitempty"`
	Busy            string `json:"busy,omitempty"`        // free, tentative, busy (default), out_of_office, working_elsewhere
	Sensitivity     string `json:"sensitivity,omitempty"` // normal (default), personal, private, confidential
	ReminderMinutes int    `json:"reminder_minutes,omitempty"`
	// Attendees make the event a meeting. The client mails the invitation to them: HCL Traveler creates a meeting through ActiveSync
	// and invites nobody. That sends mail, so it needs the switch for sending.
	Attendees []AttendeeRequest `json:"attendees,omitempty"`
	// NoInviteMail leaves the invitation to the server, for a server that sends it itself.
	NoInviteMail bool `json:"no_invite_mail,omitempty"`
}

// AttendeeRequest is a person to invite.
type AttendeeRequest struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type,omitempty"` // required (default), optional, resource
}

func indexOf(names []string, s, what string) (int, error) {
	if s == "" {
		return -1, nil
	}
	for i, n := range names {
		if n == s {
			return i, nil
		}
	}
	return 0, newError(http.StatusBadRequest, "bad_request", fmt.Sprintf("%s %q: use %s", what, s, strings.Join(names, ", ")))
}

// draftOf checks an EventRequest and turns it into what the session creates.
func draftOf(r EventRequest, zone probe.Zone) (probe.EventDraft, error) {
	bad := func(format string, a ...any) (probe.EventDraft, error) {
		return probe.EventDraft{}, newError(http.StatusBadRequest, "bad_request", fmt.Sprintf(format, a...))
	}
	d := probe.EventDraft{Subject: r.Subject, Location: r.Location, Body: r.Body, AllDay: r.AllDay, Busy: 2, Reminder: r.ReminderMinutes}
	if !zone.IsUTC() {
		z := zone
		d.Zone = &z
	}
	if strings.TrimSpace(r.Subject) == "" {
		return bad("a subject is required")
	}
	if r.Start == "" {
		return bad("a start is required")
	}
	if r.ReminderMinutes < 0 {
		return bad("reminder_minutes cannot be negative")
	}
	start, err := parseWhen(r.Start, time.Now(), zone.Location())
	if err != nil {
		return bad("start: %v", err)
	}
	switch {
	case r.AllDay:
		day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
		d.Start = day(start)
		last := d.Start
		if r.End != "" {
			e, err := parseWhen(r.End, time.Now(), zone.Location())
			if err != nil {
				return bad("end: %v", err)
			}
			if last = day(e); last.Before(d.Start) {
				return bad("the last day is before the first")
			}
		}
		d.End = last.AddDate(0, 0, 1)
	case r.End != "" && r.Duration != "":
		return bad("give end or duration, not both")
	case r.End != "":
		end, err := parseWhen(r.End, time.Now(), zone.Location())
		if err != nil {
			return bad("end: %v", err)
		}
		d.Start, d.End = start, end
	default:
		length := time.Hour
		if r.Duration != "" {
			if length, err = time.ParseDuration(r.Duration); err != nil || length <= 0 {
				return bad("duration %q: use for example 30m, 90m or 2h", r.Duration)
			}
		}
		d.Start, d.End = start, start.Add(length)
	}
	if !d.End.After(d.Start) {
		return bad("the end has to be after the start")
	}
	if i, err := indexOf(busyNames, r.Busy, "busy"); err != nil {
		return probe.EventDraft{}, err
	} else if i >= 0 {
		d.Busy = i
	}
	if i, err := indexOf(sensitivityNames, r.Sensitivity, "sensitivity"); err != nil {
		return probe.EventDraft{}, err
	} else if i >= 0 {
		d.Sensitivity = i
	}
	for _, a := range r.Attendees {
		typ := 1
		switch a.Type {
		case "", "required":
		case "optional":
			typ = 2
		case "resource":
			typ = 3
		default:
			return bad("attendee type %q: use required, optional or resource", a.Type)
		}
		email := strings.TrimSpace(a.Email)
		if p, err := mail.ParseAddress(email); err != nil || !strings.Contains(p.Address, "@") {
			return bad("attendee %q is not an e-mail address", a.Email)
		}
		d.Attendees = append(d.Attendees, probe.Attendee{Name: a.Name, Email: email, Type: typ})
	}
	return d, nil
}

// CreateEvent adds an event to a calendar. Without a folder the calendar of the mailbox is used. It needs the switch for writing,
// and with attendees also the one for sending, because the server invites the attendees.
func (m *Manager) CreateEvent(ctx context.Context, account, folder string, r EventRequest) (*ChangeResult, error) {
	if err := m.needWrite("creating an event"); err != nil {
		return nil, err
	}
	if len(r.Attendees) > 0 && !m.opts.AllowSend {
		return nil, newError(http.StatusForbidden, "send_disabled", "an event with attendees invites them and sends mail, and sending is not allowed: start with -send (send in the config)")
	}
	if len(r.Attendees) > 0 {
		emails := make([]string, 0, len(r.Attendees))
		for _, at := range r.Attendees {
			emails = append(emails, at.Email)
		}
		if err := m.checkRecipients(emails); err != nil { // the invitation is mail to them, from the client or from the server
			return nil, err
		}
	}
	ent, err := m.resolve(account)
	if err != nil {
		return nil, err
	}
	d, err := draftOf(r, ent.zone)
	if err != nil {
		return nil, err
	}
	var out *ChangeResult
	err = m.with(ctx, account, func(a Account, s *probe.Session) error {
		if folder == "" {
			folder = "calendar"
		}
		f, err := folderOf(s, folder)
		if err != nil {
			return err
		}
		d.UID = probe.NewUID() // the entry and its invitation have the same one
		id, err := s.CreateEvent(ctx, f.ID, d)
		if err != nil {
			return err
		}
		out = &ChangeResult{Account: a.Name, Action: "create", Folder: f.ID, ID: id, OK: true}
		for _, at := range d.Attendees {
			out.Attendees = append(out.Attendees, at.Email)
		}
		if len(d.Attendees) > 0 && !r.NoInviteMail {
			ev := probe.Event{ID: id, Folder: f.ID, UID: d.UID, Subject: d.Subject, Location: d.Location, Body: d.Body, Start: d.Start, End: d.End,
				AllDay: d.AllDay, Busy: d.Busy, Meeting: 1, OrganizerEmail: s.SelfAddress(), Attendees: d.Attendees, Zone: d.Zone}
			if err := s.SendInvitation(ctx, "REQUEST", ev, 0); err != nil {
				return newError(http.StatusBadGateway, "invitation_failed", fmt.Sprintf("the event was created (id %s), but the invitation could not be sent: %v", id, err))
			}
			out.InvitationMail = true
		}
		return nil
	})
	return out, err
}

// DeleteEvent deletes an event. The server may tell the attendees of a meeting that it is canceled, so a meeting needs the switch for
// sending as well.
func (m *Manager) DeleteEvent(ctx context.Context, account, folder, id string) (*ChangeResult, error) {
	return m.DeleteEventWith(ctx, account, folder, id, true)
}

// DeleteEventWith is DeleteEvent that says whether the client mails the cancellation of a meeting that the account organizes to its
// attendees (HCL Traveler does not do it).
func (m *Manager) DeleteEventWith(ctx context.Context, account, folder, id string, cancelMail bool) (*ChangeResult, error) {
	if err := m.needWrite("deleting an event"); err != nil {
		return nil, err
	}
	var out *ChangeResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, err := folderForMail(s, folder, id)
		if err != nil {
			return err
		}
		e, err := findEvent(ctx, s, f.ID, id)
		if err != nil {
			return err
		}
		if len(e.Attendees) > 0 && !m.opts.AllowSend {
			return newError(http.StatusForbidden, "send_disabled", "the event has attendees, and deleting it may send them a cancellation, and sending is not allowed: start with -send (send in the config)")
		}
		if len(e.Attendees) > 0 { // the cancellation is mail to them, and nothing is deleted when it may not go
			emails := make([]string, 0, len(e.Attendees))
			for _, at := range e.Attendees {
				emails = append(emails, at.Email)
			}
			if err := m.checkRecipients(emails); err != nil {
				return err
			}
		}
		if err := s.DeleteEvent(ctx, f.ID, id); err != nil {
			return err
		}
		out = &ChangeResult{Account: a.Name, Action: "delete", Folder: f.ID, ID: id, OK: true}
		if cancelMail && len(e.Attendees) > 0 && strings.EqualFold(e.OrganizerEmail, s.SelfAddress()) {
			if err := s.SendInvitation(ctx, "CANCEL", e, 1); err != nil {
				return newError(http.StatusBadGateway, "invitation_failed", fmt.Sprintf("the event was deleted, but the cancellation could not be sent: %v", err))
			}
			out.InvitationMail = true
		}
		return nil
	})
	return out, err
}
