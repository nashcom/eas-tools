package probe

import (
	"context"
	"errors"

	"github.com/hstern/go-activesync/eas"
)

// SentID returns the Sent Items folder id, or "" while it is unknown.
func (s *Session) SentID() string {
	return s.state.FolderIDByType(eas.FolderTypeSentItems)
}

// bootstrapFolder runs a Sync of the folder so that it has a sync key and has been fetched once: the Inbox with the
// session's own Sync, any other folder directly with the library.
func (s *Session) bootstrapFolder(ctx context.Context, folder string) error {
	if folder == s.Inbox() {
		_, err := s.Sync(ctx, nil)
		return err
	}
	_, err := s.syncFolder(ctx, folder)
	return err
}

// syncFolder runs one Sync of a mail folder with the options of the Inbox Sync.
func (s *Session) syncFolder(ctx context.Context, folder string) (*eas.EmailSyncResult, error) {
	s.rec.setStep(StepSync)
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	return s.client.SyncEmail(ctx, folder, eas.EmailSyncOptions{
		WindowSize:         s.window(),
		BodyType:           eas.BodyTypePlain,
		BodyTruncationSize: 256,
	})
}

// DeleteSentCopies deletes the mails in Sent Items whose subject is in the set, and nothing else: it is for the copies
// of test mail that the server keeps although the SendMail asked for none. It returns how many mails were deleted and
// how many could not be.
func (s *Session) DeleteSentCopies(ctx context.Context, subjects map[string]bool) (deleted, failed int, err error) {
	if len(subjects) == 0 {
		return 0, 0, nil
	}
	sent := s.SentID()
	if sent == "" {
		return 0, 0, errors.New("Sent Items folder unknown: run FolderSync first")
	}

	s.windowOverride = mailSyncWindow
	defer func() { s.windowOverride = 0 }()

	var ids []string
	for page := 0; page < mailMaxPages; page++ {
		res, err := s.syncFolder(ctx, sent)
		if err != nil {
			return 0, 0, err
		}
		for _, m := range append(append([]eas.EmailItem(nil), res.Added...), res.Changed...) {
			if subjects[m.Subject] {
				ids = append(ids, m.ServerID)
			}
		}
		if !res.MoreAvailable {
			break
		}
	}

	for _, id := range ids {
		if err := s.deleteViaSync(ctx, sent, id); err != nil {
			failed++
			continue
		}
		deleted++
	}
	return deleted, failed, nil
}

// BootstrapSent fetches the Sent Items folder once, so that it has a sync key and a later DeleteSentCopies only sees what
// was sent after it. A long-living session that sends mail calls it before the first mail.
func (s *Session) BootstrapSent(ctx context.Context) error {
	sent := s.SentID()
	if sent == "" {
		return nil // a server without Sent Items has no copies to clean up
	}
	s.windowOverride = mailSyncWindow
	for page := 0; page < mailMaxPages; page++ {
		res, err := s.syncFolder(ctx, sent)
		if err != nil {
			return err
		}
		if !res.MoreAvailable {
			break
		}
	}
	return nil
}

// UseMailWindow makes the Syncs of this session read the Inbox (and Sent Items) in large pages, as a mail test does. It is
// for a session that is only used for mail, such as the one of the mail monitor.
func (s *Session) UseMailWindow() { s.windowOverride = mailSyncWindow }
