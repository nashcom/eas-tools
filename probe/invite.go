package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
)

// NewUID makes the UID of a new calendar entry: the name that the entry has in every calendar, and in the invitations.
func NewUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d@eas-client.invalid", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:]) + "@eas-client.invalid"
}

// SendInvitation mails a meeting to its attendees as an iTIP message (RFC 5546): a text and a text/calendar part with the method
// REQUEST (an invitation) or CANCEL (a cancellation). A calendar program, Notes among them, shows it as an invitation with the
// buttons to answer it. The server is not trusted to do this: HCL Traveler creates a meeting that it is given through ActiveSync and
// invites nobody. sequence is the number of the version of the meeting.
func (s *Session) SendInvitation(ctx context.Context, method string, e Event, sequence int) error {
	if method != "REQUEST" && method != "CANCEL" {
		return fmt.Errorf("invitation method %q: use REQUEST or CANCEL", method)
	}
	var to []string
	for _, a := range e.Attendees {
		if a.Email != "" {
			to = append(to, a.Email)
		}
	}
	if len(to) == 0 {
		return errors.New("the meeting has no attendees to send to")
	}
	if err := s.ensureClient(); err != nil {
		return err
	}
	from := s.selfAddress()
	if from == "" {
		return errors.New("the account has no e-mail address to send from")
	}
	s.rec.setStep(StepSendMail)
	if e.OrganizerEmail == "" {
		e.OrganizerEmail = from
	}

	subject, lead := e.Subject, "You are invited to a meeting." // Notes puts "Invitation:" in front of it itself
	if method == "CANCEL" {
		subject, lead = "Canceled: "+e.Subject, "The meeting has been canceled."
	}
	var text strings.Builder
	text.WriteString(lead + "\r\n\r\n")
	text.WriteString("Subject: " + e.Subject + "\r\n")
	if e.AllDay {
		text.WriteString("When: " + e.Start.UTC().Format("2006-01-02") + "\r\n")
	} else {
		text.WriteString("When: " + e.Start.UTC().Format("2006-01-02 15:04") + " - " + e.End.UTC().Format("15:04") + " UTC\r\n")
	}
	if e.Location != "" {
		text.WriteString("Where: " + e.Location + "\r\n")
	}
	text.WriteString("Organizer: " + e.OrganizerEmail + "\r\n")
	if e.Body != "" {
		text.WriteString("\r\n" + e.Body + "\r\n")
	}

	var b [12]byte
	_, _ = rand.Read(b[:])
	boundary := "eas-client-" + hex.EncodeToString(b[:])
	var msg bytes.Buffer
	msg.WriteString("From: " + from + "\r\n")
	msg.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n")
	msg.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	msg.WriteString("Message-ID: <" + hex.EncodeToString(b[:]) + "@eas-client.invalid>\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	part := func(contentType string, body []byte) {
		msg.WriteString("--" + boundary + "\r\n")
		msg.WriteString("Content-Type: " + contentType + "\r\n")
		msg.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&msg)
		_, _ = w.Write(body)
		_ = w.Close()
		msg.WriteString("\r\n")
	}
	part("text/plain; charset=UTF-8", []byte(text.String()))
	part("text/calendar; charset=UTF-8; method="+method, e.ICalendarMethod(time.Now(), method, sequence))
	msg.WriteString("--" + boundary + "--\r\n")

	return s.client.SendMail(ctx, eas.SendMailOptions{MIME: msg.Bytes()})
}
