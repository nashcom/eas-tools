package probe

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
)

// ErrNoSender: the mail to reply to has no sender; ErrNoRecipient: the reply or forward would go to nobody.
var (
	ErrNoSender    = errors.New("the mail has no sender to reply to")
	ErrNoRecipient = errors.New("the reply or forward has no recipient")
)

// ReplyOptions shape ReplyMail.
type ReplyOptions struct {
	// All also sends the reply to the other recipients of the original (To and Cc), not only to its sender.
	All bool
	// Body is the new text. The server adds the original to it (SmartReply).
	Body string
	// SkipSaveInSent keeps no copy in Sent Items.
	SkipSaveInSent bool
}

// ForwardOptions shape ForwardMail.
type ForwardOptions struct {
	To, Cc         []string
	Body           string
	SkipSaveInSent bool
}

// SmartResult tells what a reply or forward was sent as.
type SmartResult struct {
	To      []string
	Cc      []string
	Subject string
}

// ReplyMail replies to a mail with the EAS SmartReply command: the client sends only the new text and the server merges it
// with the original, as it does for a phone. The reply goes to the sender of the original, and with All also to its other
// recipients, the account itself left out.
func (s *Session) ReplyMail(ctx context.Context, folder, id string, o ReplyOptions) (*SmartResult, error) {
	orig, err := s.FetchMail(ctx, folder, id)
	if err != nil {
		return nil, err
	}
	self := strings.ToLower(s.selfAddress())

	from := addressList(orig.From)
	if len(from) == 0 {
		return nil, ErrNoSender
	}
	to := withoutAddress(from, self)
	if len(to) == 0 { // a reply to a mail of the account itself goes to the people it was sent to
		to = withoutAddress(addressList(orig.To), self)
	}
	var cc []string
	if o.All {
		seen := map[string]bool{}
		for _, a := range to {
			seen[bareAddress(a)] = true
		}
		for _, a := range withoutAddress(addressList(orig.To), self) {
			if !seen[bareAddress(a)] {
				to, seen[bareAddress(a)] = append(to, a), true
			}
		}
		for _, a := range withoutAddress(addressList(orig.Cc), self) {
			if !seen[bareAddress(a)] {
				cc, seen[bareAddress(a)] = append(cc, a), true
			}
		}
	}
	if len(to) == 0 {
		return nil, ErrNoRecipient
	}
	subject := orig.Subject
	if !hasPrefixFold(subject, "re:") {
		subject = "Re: " + subject
	}
	return s.smart(ctx, "SmartReply", folder, id, Mail{To: to, Cc: cc, Subject: subject, Body: o.Body, SkipSaveInSent: o.SkipSaveInSent})
}

// ForwardMail forwards a mail with the EAS SmartForward command: the client sends the recipients and the new text, the
// server adds the original (with its attachments).
func (s *Session) ForwardMail(ctx context.Context, folder, id string, o ForwardOptions) (*SmartResult, error) {
	if len(o.To) == 0 {
		return nil, ErrNoRecipient
	}
	orig, err := s.FetchMail(ctx, folder, id)
	if err != nil {
		return nil, err
	}
	subject := orig.Subject
	if !hasPrefixFold(subject, "fw:") && !hasPrefixFold(subject, "fwd:") {
		subject = "Fw: " + subject
	}
	return s.smart(ctx, "SmartForward", folder, id, Mail{To: o.To, Cc: o.Cc, Subject: subject, Body: o.Body, SkipSaveInSent: o.SkipSaveInSent})
}

func (s *Session) smart(ctx context.Context, cmd, folder, id string, m Mail) (*SmartResult, error) {
	s.rec.setStep(StepSendMail)
	from := s.selfAddress()
	if from == "" {
		return nil, errors.New("the account has no e-mail address to send from")
	}
	if strings.TrimSpace(m.Body) == "" {
		m.Body = "\r\n" // an empty text is not "no text": the filler of Mail would be used
	}
	mime := BuildMIME(from, m, fmt.Sprintf("client-%d", time.Now().UnixMilli()))
	opts := eas.ReplyForwardOptions{
		SendMailOptions: eas.SendMailOptions{MIME: mime, SkipSaveInSent: m.SkipSaveInSent},
		FolderID:        folder, ServerID: id,
	}
	var err error
	if cmd == "SmartReply" {
		err = s.client.SmartReply(ctx, opts)
	} else {
		err = s.client.SmartForward(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	return &SmartResult{To: m.To, Cc: m.Cc, Subject: m.Subject}, nil
}

func hasPrefixFold(s, prefix string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// addressList reads a list of addresses as a server writes it in a header field: "Name <a@b>, c@d", and some servers
// separate with semicolons. What does not parse is left out.
func addressList(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	list, err := mail.ParseAddressList(text)
	if err != nil {
		if list, err = mail.ParseAddressList(strings.ReplaceAll(text, ";", ",")); err != nil {
			return nil
		}
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

// bareAddress is the lower case address of "Name <a@b>".
func bareAddress(a string) string {
	if p, err := mail.ParseAddress(a); err == nil {
		return strings.ToLower(p.Address)
	}
	return strings.ToLower(a)
}

// withoutAddress returns the list without the address (an empty address removes nothing).
func withoutAddress(list []string, address string) []string {
	var out []string
	for _, a := range list {
		if address == "" || bareAddress(a) != address {
			out = append(out, a)
		}
	}
	return out
}
