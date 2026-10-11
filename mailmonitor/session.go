package mailmonitor

import (
	"context"

	"github.com/nashcom/eas-tools/probe"
)

// sessionBox is a Mailbox on an ActiveSync session.
type sessionBox struct{ s *probe.Session }

// OpenSession brings a session to the point where it can send and receive: protocol version, registration, device
// information, folder list, and a first look at Sent Items so that its cleanup later only sees new mail.
func OpenSession(ctx context.Context, s *probe.Session) (Mailbox, error) {
	s.UseMailWindow()
	if err := s.Options(ctx, nil); err != nil {
		return nil, err
	}
	if err := s.Provision(ctx, nil); err != nil {
		return nil, err
	}
	if s.HasDeviceInfo() {
		_ = s.DeviceInfo(ctx, nil) // like the other probes: not fatal
	}
	if err := s.FolderSync(ctx, nil); err != nil {
		return nil, err
	}
	if err := s.BootstrapSent(ctx); err != nil {
		return nil, err
	}
	return &sessionBox{s}, nil
}

func (b *sessionBox) Send(ctx context.Context, to, subject, body, tag string) error {
	return b.s.SendMail(ctx, nil, probe.Mail{To: []string{to}, Subject: subject, Body: body, SkipSaveInSent: true}, tag)
}

func (b *sessionBox) Poll(ctx context.Context) ([]Mail, error) {
	var out []Mail
	for page := 0; page < 200; page++ {
		res, err := b.s.Sync(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, m := range res.Mails {
			out = append(out, Mail{ID: m.ServerID, Subject: m.Subject})
		}
		if !res.MoreAvailable {
			break
		}
	}
	return out, nil
}

func (b *sessionBox) Delete(ctx context.Context, id string) error {
	return b.s.DeleteMail(ctx, nil, id)
}

func (b *sessionBox) DeleteSent(ctx context.Context, subjects map[string]bool) (int, int, error) {
	if b.s.SentID() == "" {
		return 0, 0, nil // no Sent Items folder, no copies
	}
	return b.s.DeleteSentCopies(ctx, subjects)
}

func (b *sessionBox) Close() { b.s.Close() }
