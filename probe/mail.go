package probe

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
)

// Mail is a message to send with SendMail.
type Mail struct {
	To      []string
	Cc      []string // copies; they receive the mail like To (EAS reads the recipients from the headers)
	Subject string
	// Size is the approximate body size in bytes (a plain text filler).
	Size int
	// Attachments is how many attachments the message gets, each of AttachSize bytes of binary data (0 = a plain text
	// message). The data is pseudo-random, so it does not shrink when it is compressed on the way.
	Attachments int
	AttachSize  int
	// AttachBinary makes the attachments binary data (.bin); by default they are text files (.txt).
	AttachBinary bool
	// SkipSaveInSent stops the server from keeping a copy in Sent Items,
	// which keeps test mailboxes clean.
	SkipSaveInSent bool
	// Body, when set, is the text of the message instead of the filler (Size is then ignored).
	Body string
}

// BuildMIME makes the RFC 5322 message for a Mail from the given sender.
// The tag is also put into a header, so test mail can be found again.
func BuildMIME(from string, m Mail, tag string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(m.To, ", ") + "\r\n")
	if len(m.Cc) > 0 {
		b.WriteString("Cc: " + strings.Join(m.Cc, ", ") + "\r\n")
	}
	b.WriteString("Subject: " + m.Subject + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: <" + tag + "@easload.invalid>\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("X-Easload-Tag: " + tag + "\r\n")

	attachments := m.Attachments
	if m.AttachSize <= 0 {
		attachments = 0
	}
	boundary := "easload-" + tag
	if attachments > 0 {
		b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
		b.WriteString("--" + boundary + "\r\n")
	}
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 7bit\r\n")
	b.WriteString("\r\n")

	const line = "easload test mail - this text only fills the message to the requested size.\r\n"
	if m.Body != "" {
		b.WriteString(strings.ReplaceAll(m.Body, "\n", "\r\n"))
	} else {
		for n := 0; n < m.Size; n += len(line) {
			b.WriteString(line)
		}
	}

	if attachments > 0 {
		h := fnv.New64a()
		h.Write([]byte(tag))
		rnd := rand.New(rand.NewSource(int64(h.Sum64())))
		data := make([]byte, m.AttachSize)
		for i := 1; i <= attachments; i++ {
			b.WriteString("\r\n--" + boundary + "\r\n")
			if m.AttachBinary {
				rnd.Read(data)
				name := fmt.Sprintf("attachment-%d.bin", i)
				b.WriteString("Content-Type: application/octet-stream; name=\"" + name + "\"\r\n")
				b.WriteString("Content-Transfer-Encoding: base64\r\n")
				b.WriteString("Content-Disposition: attachment; filename=\"" + name + "\"\r\n\r\n")
				enc := base64.StdEncoding.EncodeToString(data)
				for len(enc) > 76 {
					b.WriteString(enc[:76] + "\r\n")
					enc = enc[76:]
				}
				b.WriteString(enc + "\r\n")
				continue
			}
			name := fmt.Sprintf("attachment-%d.txt", i)
			b.WriteString("Content-Type: text/plain; charset=us-ascii; name=\"" + name + "\"\r\n")
			b.WriteString("Content-Transfer-Encoding: 7bit\r\n")
			b.WriteString("Content-Disposition: attachment; filename=\"" + name + "\"\r\n\r\n")
			b.WriteString(textAttachment(rnd, tag, i, attachments, m.AttachSize))
			b.WriteString("\r\n")
		}
		b.WriteString("\r\n--" + boundary + "--\r\n")
	}
	return []byte(b.String())
}

// textAttachment makes the text of an attachment of exactly size bytes: a few readable lines that say what the file
// is, then lines of random base64 characters, which read as text but hardly shrink when compressed. No line starts
// with "--", so the text can never look like a MIME boundary.
func textAttachment(rnd *rand.Rand, tag string, n, of, size int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	const width = 74

	var b strings.Builder
	fmt.Fprintf(&b, "easload test attachment\r\ntag:  %s\r\nfile: %d of %d\r\nsize: %d bytes\r\n\r\n", tag, n, of, size)

	r := make([]byte, width)
	for b.Len() < size {
		rnd.Read(r)
		for i := range r {
			r[i] = alphabet[r[i]&63]
		}
		b.Write(r)
		b.WriteString("\r\n")
	}
	return b.String()[:size]
}

// SelfAddress returns the e-mail address of the account, "" if unknown.
func (s *Session) SelfAddress() string { return s.selfAddress() }

// SendMail sends a message from the account with the EAS SendMail command.
// tag is a unique text for the message (it goes into the Message-ID and a
// header); it is not added to the subject.
func (s *Session) SendMail(ctx context.Context, st *Step, m Mail, tag string) error {
	s.rec.setStep(StepSendMail)
	if err := s.ensureClient(); err != nil {
		return err
	}
	from := s.selfAddress()
	if from == "" {
		return errors.New("the account has no e-mail address to send from")
	}
	if len(m.To) == 0 {
		return errors.New("no recipient")
	}

	mime := BuildMIME(from, m, tag)
	if err := s.client.SendMail(ctx, eas.SendMailOptions{MIME: mime, SkipSaveInSent: m.SkipSaveInSent}); err != nil {
		return err
	}
	if s.watch != nil {
		s.watch.note("SendMail accepted (%d bytes)", len(mime))
	}
	put(st, "to", strings.Join(m.To, ","))
	put(st, "bytes", fmt.Sprintf("%d", len(mime)))
	return nil
}

// DeleteMail deletes a mail of the Inbox by its server id (it goes to
// Deleted Items; the server decides what happens after that).
func (s *Session) DeleteMail(ctx context.Context, st *Step, serverID string) error {
	s.rec.setStep(StepSync)
	if err := s.ensureClient(); err != nil {
		return err
	}
	inbox := s.Inbox()
	if inbox == "" {
		return errors.New("Inbox unknown: run FolderSync first")
	}

	if err := s.deleteViaSync(ctx, inbox, serverID); err != nil {
		return err
	}
	put(st, "deleted", serverID)
	return nil
}

// ErrMailLost means the mail was sent but did not show up in the Inbox within
// the wait time.
var ErrMailLost = errors.New("the mail did not arrive in the Inbox within the wait time")

// ErrMailLate means the mail did not show up within the wait time, but did in the extra wait that follows it (see
// MailTripOptions.ExtraWait).
var ErrMailLate = errors.New("the mail arrived in the Inbox, but only after the wait time")

// MailTripOptions control MailRoundTrip.
type MailTripOptions struct {
	// Wait is how long to wait for the mail to come back (default 60 s).
	Wait time.Duration
	// ExtraWait keeps looking for the mail this long after the wait time is over (0 = do not): a mail found then is
	// late (ErrMailLate), one that is still missing is lost (ErrMailLost). A late mail is deleted like any other.
	ExtraWait time.Duration
	// Arrival is "push" (hold a Ping, the way a phone learns of new mail;
	// default) or "poll" (Sync every second).
	Arrival string
	// Keep leaves the mail in the Inbox and a copy in Sent Items; by default
	// the mail is deleted again and no copy is kept.
	Keep bool
	// SaveSent asks the server to keep a copy in Sent Items (default: no copy).
	SaveSent bool
	// SizeKB is the approximate body size (default 2).
	SizeKB float64
	// Attachments and AttachKB add that many attachments of that size (KB) to the mail (default none).
	Attachments int
	AttachKB    float64
	// AttachBinary sends binary attachments (.bin) instead of text files (.txt).
	AttachBinary bool
	// Subject and Tag identify the mail; both are generated when empty.
	Subject string
	Tag     string
}

// MailTrip is the outcome of MailRoundTrip.
type MailTrip struct {
	Sent    bool          // the SendMail command was accepted
	Arrived bool          // the mail showed up in the Inbox
	Elapsed time.Duration // from the start of SendMail until it showed up
	Deleted bool          // the mail was deleted again
	// CleanupError is why the mail could not be deleted again (it stays in the mailbox).
	CleanupError string
	Subject      string

	// What the device saw while it waited, to tell a slow mail from one that this device never got: the Inbox Syncs
	// done, the mails they returned (all senders), the sync key of the last one. When the wait ran out the device
	// kept looking for as long again: Late is true when the mail then showed up (LateAfter after SendMail).
	Polls     int
	SeenMails int
	LastKey   string
	Late      bool
	LateAfter time.Duration
	// Timeline lists what happened until the mail was declared lost: SendMail, the first Syncs, every Sync with changes
	// (with the subjects), and a note about every 15th quiet one.
	Timeline []string
	// ReceivedAfter is how long after the start of SendMail the server says it received the mail (its DateReceived), when it
	// said; it depends on the clocks of both sides, so a difference of a few seconds does not mean much.
	ReceivedAfter time.Duration
	HasReceived   bool

	// Push (arrival=push only): was a Ping that was already pending when the
	// mail was sent woken by it? PushWake is true when the server ended the Ping
	// with "changes" (status 2) and the Sync after it found this exact mail;
	// WakeLatency is the time from SendMail to the end of the Ping. A mail that
	// arrived without waking the Ping is a missed push (PushWake false).
	PushChecked bool
	PushWake    bool
	WakeLatency time.Duration
	PushError   string // the held Ping failed or was refused; the arrival was found by polling
	PushNote    string // why the push counts as missed, e.g. the server ended the Ping at once
}

// pingSettle is how long the held Ping gets to reach the server before the
// mail is sent, so that the mail really arrives while the Ping is pending.
const pingSettle = 400 * time.Millisecond

// During a mail test the Inbox is read with this window, and at most mailMaxPages
// pages are read to find the test mail: other mail in the Inbox is only skipped,
// never touched, and an Inbox with thousands of mails is still no problem.
const (
	mailSyncWindow = 50
	mailMaxPages   = 200
)

// MailRoundTrip sends a mail to the account itself and waits until it shows up
// in the Inbox: a round trip through the server's mail routing and back to the
// device. FolderSync has to have run. A mail that does not come back within the
// wait time returns ErrMailLost together with the (non-nil) trip, or ErrMailLate when it showed up in the ExtraWait.
func (s *Session) MailRoundTrip(ctx context.Context, st *Step, o MailTripOptions) (*MailTrip, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	self := s.selfAddress()
	if self == "" {
		return nil, errors.New("the account has no e-mail address to send to (use the internet address as login)")
	}
	if s.Inbox() == "" {
		return nil, errors.New("Inbox unknown: run FolderSync first")
	}
	if o.Wait <= 0 {
		o.Wait = 60 * time.Second
	}
	if o.Arrival == "" {
		o.Arrival = "push"
	}
	if o.SizeKB <= 0 {
		o.SizeKB = 2
	}
	if o.Tag == "" {
		o.Tag = fmt.Sprintf("%s-%08x", s.cfg.DeviceID[:6], time.Now().UnixNano()&0xffffffff)
	}
	if o.Subject == "" {
		o.Subject = "[easprobe " + o.Tag + "]"
	}
	trip := &MailTrip{Subject: o.Subject}
	put(st, "subject", o.Subject) // how to find the mail by hand if the cleanup fails

	s.windowOverride = mailSyncWindow
	watch := &mailWatch{}
	s.watch = watch
	defer func() { s.windowOverride, s.watch = 0, nil }()

	// Start from a drained Inbox, so that only the new mail is seen.
	if _, err := s.drainInbox(ctx); err != nil {
		return trip, err
	}

	*watch = mailWatch{start: time.Now()} // count only the Syncs after the mail was sent
	msg := Mail{To: []string{self}, Subject: o.Subject, Size: int(o.SizeKB * 1024), SkipSaveInSent: !o.SaveSent,
		Attachments: o.Attachments, AttachSize: int(o.AttachKB * 1024), AttachBinary: o.AttachBinary}

	var ref MailRef
	var found bool
	var t0 time.Time
	var err error
	if o.Arrival == "push" {
		ref, found, t0, err = s.mailPush(ctx, o, msg, trip)
	} else {
		t0 = time.Now()
		if err = s.SendMail(ctx, nil, msg, o.Tag); err == nil {
			trip.Sent = true
			ref, found, err = s.awaitMail(ctx, o.Subject, o)
		}
	}
	if err != nil {
		return trip, err
	}
	if !found {
		trip.Polls, trip.SeenMails, trip.LastKey = watch.polls, watch.seen, watch.key
		put(st, "sent", "true")
		put(st, "arrived", "false")
		put(st, "waited_seconds", fmt.Sprintf("%.0f", o.Wait.Seconds()))
		put(st, "polls", fmt.Sprintf("%d", trip.Polls))
		put(st, "mails_seen", fmt.Sprintf("%d", trip.SeenMails))

		if o.ExtraWait <= 0 {
			return trip, ErrMailLost
		}

		// Keep looking for the extra wait: a mail that shows up now is late, one that does not is lost. A late mail
		// is deleted like any other, so it does not stay behind in the Inbox.
		watch.note("wait of %.0fs is over: looking for another %.0fs", o.Wait.Seconds(), o.ExtraWait.Seconds())
		later := o
		later.Arrival, later.Wait = "poll", o.ExtraWait
		var lateRef MailRef
		lateRef, trip.Late, err = s.awaitMail(ctx, o.Subject, later)
		trip.Polls, trip.SeenMails, trip.LastKey = watch.polls, watch.seen, watch.key
		if trip.Late {
			watch.note("the mail showed up (late)")
			if !lateRef.Received.IsZero() {
				trip.ReceivedAfter, trip.HasReceived = lateRef.Received.Sub(t0), true
				watch.note("the server says it received the mail %+.1fs after SendMail started", trip.ReceivedAfter.Seconds())
			}
		} else {
			watch.note("the mail did not show up in the extra wait either")
		}
		trip.Timeline = append([]string(nil), watch.events...)
		if watch.cut > 0 {
			trip.Timeline = append(trip.Timeline, fmt.Sprintf("... %d more entries left out", watch.cut))
		}
		if err != nil {
			return trip, err
		}
		if trip.Late {
			trip.LateAfter = time.Since(t0)
			put(st, "late_seconds", fmt.Sprintf("%.0f", trip.LateAfter.Seconds()))
			if !o.Keep {
				if err := s.DeleteMail(ctx, nil, lateRef.ServerID); err == nil {
					trip.Deleted = true
				} else {
					trip.CleanupError = ErrorText(err)
				}
			}
		}
		put(st, "late", fmt.Sprintf("%t", trip.Late))
		if trip.Late {
			return trip, ErrMailLate
		}
		return trip, ErrMailLost
	}
	trip.Arrived = true
	if !ref.Received.IsZero() {
		trip.ReceivedAfter, trip.HasReceived = ref.Received.Sub(t0), true
	}
	trip.Elapsed = time.Since(t0)
	put(st, "arrived", "true")
	put(st, "roundtrip_seconds", fmt.Sprintf("%.2f", trip.Elapsed.Seconds()))
	put(st, "arrival", o.Arrival)
	if trip.PushChecked {
		switch {
		case trip.PushWake:
			put(st, "push", "woke")
			put(st, "wake_seconds", fmt.Sprintf("%.2f", trip.WakeLatency.Seconds()))
		case trip.PushError != "":
			put(st, "push", "error")
			put(st, "push_error", trip.PushError)
		default:
			put(st, "push", "missed")
			if trip.PushNote != "" {
				put(st, "push_note", trip.PushNote)
			}
		}
	}

	if !o.Keep {
		// A failed cleanup is reported but is not a failure of the mail.
		if err := s.DeleteMail(ctx, nil, ref.ServerID); err == nil {
			trip.Deleted = true
		} else {
			trip.CleanupError = ErrorText(err)
			put(st, "cleanup_error", trip.CleanupError)
		}
		put(st, "deleted", fmt.Sprintf("%t", trip.Deleted))
	}
	return trip, nil
}

// drainInbox syncs the Inbox until the server has nothing more.
func (s *Session) drainInbox(ctx context.Context) (int, error) {
	total := 0
	for page := 0; page < mailMaxPages; page++ {
		res, err := s.Sync(ctx, nil)
		if err != nil {
			return total, err
		}
		total += res.Added + res.Changed
		if !res.MoreAvailable {
			return total, nil
		}
	}
	return total, nil
}

// findMail syncs until the mail with the subject is found or the server has
// nothing more.
func (s *Session) findMail(ctx context.Context, subject string) (MailRef, bool, error) {
	for page := 0; page < mailMaxPages; page++ {
		t := time.Now()
		res, err := s.Sync(ctx, nil)
		if err != nil {
			return MailRef{}, false, err
		}
		if s.watch != nil {
			s.watch.sync(res, time.Since(t), subject)
		}
		for _, m := range res.Mails {
			if m.Subject == subject {
				return m, true, nil
			}
		}
		if !res.MoreAvailable {
			return MailRef{}, false, nil
		}
	}
	return MailRef{}, false, nil
}

// awaitMail waits for the mail to show up. Push mode holds a Ping and syncs
// when it ends; poll mode syncs every second.
func (s *Session) awaitMail(ctx context.Context, subject string, o MailTripOptions) (MailRef, bool, error) {
	deadline := time.Now().Add(o.Wait)
	for {
		if o.Arrival == "push" {
			hb := int(math.Ceil(time.Until(deadline).Seconds()))
			if hb < 1 {
				hb = 1
			}
			if hb > 60 {
				hb = 60
			}
			pr, err := s.Ping(ctx, hb, nil)
			if err != nil {
				return MailRef{}, false, err
			}
			ref, ok, err := s.findMail(ctx, subject)
			if ok || err != nil {
				return ref, ok, err
			}
			// A server that answers a Ping long before the heartbeat would
			// make a tight loop: wait a moment.
			if !pr.Changes && pr.Held < time.Duration(pr.HeartbeatUsed)*time.Second/2 {
				if !sleepCtx(ctx, 300*time.Millisecond) {
					return MailRef{}, false, ctx.Err()
				}
			}
		} else {
			ref, ok, err := s.findMail(ctx, subject)
			if ok || err != nil {
				return ref, ok, err
			}
			if time.Now().After(deadline) {
				return MailRef{}, false, nil
			}
			// The Sync after this pause is the last look, even if the pause ends after the deadline.
			if !sleepCtx(ctx, time.Second) {
				return MailRef{}, false, ctx.Err()
			}
			continue
		}
		if time.Now().After(deadline) {
			return MailRef{}, false, nil
		}
	}
}

// sleepCtx waits for d or until ctx ends; it reports whether ctx is alive.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// mailPush sends the mail while a Ping is pending and sees whether the Ping is
// woken: the real test of push delivery.
//
// The Ping is started first and given a moment to reach the server. A Ping that
// ends in that moment is dealt with before anything is sent: status 5 (heartbeat
// outside the server's range) is restarted with the limit the server gave,
// status 7 (hierarchy out of date) after a FolderSync, and a wake-up by somebody
// else's change (other devices share the mailbox) simply with a new Ping. Only
// then the mail is sent, so that it arrives while a Ping is really pending.
//
// When the Ping ends with "changes" the Inbox is synced and must contain this
// exact mail; a Ping woken by somebody else's change is renewed. If the Ping
// does not signal, or fails, the mail is looked for by polling for the rest of
// the wait time and the trip records a missed push.
func (s *Session) mailPush(ctx context.Context, o MailTripOptions, msg Mail, trip *MailTrip) (ref MailRef, found bool, t0 time.Time, err error) {
	trip.PushChecked = true

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type pingOut struct {
		res *eas.PingResult
		err error
	}
	hbFor := func(rest time.Duration) int {
		hb := int(math.Ceil(rest.Seconds()))
		if hb < 1 {
			hb = 1
		}
		if hb > 60 {
			hb = 60
		}
		return hb
	}
	startPing := func(hb int) chan pingOut {
		ch := make(chan pingOut, 1)
		folders := []eas.PingFolder{{ID: s.Inbox(), Class: "Email"}}
		s.rec.setStep(StepPing)
		go func() {
			r, e := s.client.Ping(pctx, hb, folders)
			if r != nil && r.Status != 0 {
				e = nil // a status answer: see Session.Ping
			}
			ch <- pingOut{r, e}
		}()
		return ch
	}

	hb := hbFor(o.Wait)
	done := startPing(hb)

	// Settle: the Ping has to be pending at the server before the mail is sent.
	settled := false
	var early *pingOut // a Ping that ended in the settle time without being usable
	for tries := 0; tries < 4 && !settled && early == nil; tries++ {
		select {
		case out := <-done:
			switch {
			case out.err != nil:
				early = &out
			case out.res.Status == 5 && out.res.HeartbeatInterval > 0 && hb != out.res.HeartbeatInterval:
				hb = out.res.HeartbeatInterval
				done = startPing(hb)
			case out.res.Status == 7:
				if ferr := s.FolderSync(ctx, nil); ferr != nil {
					return MailRef{}, false, time.Now(), ferr
				}
				done = startPing(hb)
			case out.res.Status == 2:
				done = startPing(hb) // somebody else's change
			default:
				early = &out
			}
		case <-time.After(pingSettle):
			settled = true
		case <-ctx.Done():
			return MailRef{}, false, time.Now(), ctx.Err()
		}
	}
	if early != nil {
		switch {
		case early.err != nil:
			trip.PushError = ErrorText(early.err)
		case early.res.Status == 1:
			// not an error: the server does not hold the Ping, so there is no push
			trip.PushNote = "the server ended the Ping at once without changes (it does not hold Pings)"
		default:
			trip.PushError = fmt.Sprintf("the Ping ended at once with status %d (%s)", early.res.Status, StatusName("Ping", early.res.Status))
		}
	}

	t0 = time.Now()
	if err = s.SendMail(ctx, nil, msg, o.Tag); err != nil {
		return MailRef{}, false, t0, err
	}
	trip.Sent = true
	deadline := t0.Add(o.Wait)

	if early == nil {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()

		for {
			woke := false
			select {
			case out := <-done:
				switch {
				case out.err != nil:
					trip.PushError = ErrorText(out.err)
				case out.res.Status == 2:
					woke = true
					trip.WakeLatency = time.Since(t0)
				case out.res.Status == 5 || out.res.Status == 7:
					trip.PushError = fmt.Sprintf("the pending Ping was refused with status %d (%s)", out.res.Status, StatusName("Ping", out.res.Status))
				}
			case <-timer.C:
			case <-ctx.Done():
				return MailRef{}, false, t0, ctx.Err()
			}

			// The Sync after the Ping has to find this exact mail.
			ref, found, err = s.findMail(ctx, o.Subject)
			if err != nil {
				return MailRef{}, false, t0, err
			}
			if found {
				trip.PushWake = woke
				return ref, true, t0, nil
			}

			// Woken by somebody else's change: keep a Ping pending for our mail.
			if rest := time.Until(deadline); woke && rest > 0 {
				trip.WakeLatency = 0
				done = startPing(hbFor(rest))
				continue
			}
			break
		}
	}

	// The push did not tell us: wait for the mail by polling.
	trip.PushWake = false
	rest := time.Until(deadline)
	if rest <= 0 {
		return MailRef{}, false, t0, nil
	}
	poll := o
	poll.Arrival, poll.Wait = "poll", rest
	ref, found, err = s.awaitMail(ctx, o.Subject, poll)
	return ref, found, t0, err
}

// mailWatch records what the Inbox Syncs of a mail round trip returned: the counts, and a short timeline that is kept
// for a lost mail, so that one can see what the device was told while it waited.
type mailWatch struct {
	polls int
	seen  int
	key   string

	start  time.Time
	events []string
	cut    int // events that did not fit
}

// maxWatchEvents bounds the timeline of one mail.
const maxWatchEvents = 40

// note adds a timeline entry, with the time since the mail test started sending.
func (w *mailWatch) note(format string, args ...any) {
	if len(w.events) >= maxWatchEvents {
		w.cut++
		return
	}
	var at float64
	if !w.start.IsZero() {
		at = time.Since(w.start).Seconds()
	}
	w.events = append(w.events, fmt.Sprintf("+%.1fs %s", at, fmt.Sprintf(format, args...)))
}

// sync records one Inbox Sync: the first few, every Sync that brought changes, and every 15th of the quiet ones.
func (w *mailWatch) sync(res *SyncResult, took time.Duration, subject string) {
	w.polls++
	w.seen += len(res.Mails)
	w.key = res.SyncKey

	n := res.Added + res.Changed + res.Deleted
	if n == 0 && w.polls > 3 && w.polls%15 != 0 && !res.MoreAvailable {
		return
	}
	var subjects []string
	for i, m := range res.Mails {
		if i == 5 {
			subjects = append(subjects, fmt.Sprintf("... %d more", len(res.Mails)-5))
			break
		}
		name := m.Subject
		if !m.Received.IsZero() && !w.start.IsZero() {
			name += fmt.Sprintf(" (server received it at %+.1fs)", m.Received.Sub(w.start).Seconds())
		}
		if name == subject {
			name += " <- this mail"
		}
		subjects = append(subjects, name)
	}
	w.note("sync #%d took %.2fs: added=%d changed=%d deleted=%d more=%t key=%s %s", w.polls, took.Seconds(),
		res.Added, res.Changed, res.Deleted, res.MoreAvailable, res.SyncKey, strings.Join(subjects, "; "))
}
