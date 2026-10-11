package load

import (
	"context"
	"errors"
	"fmt"
	"github.com/nashcom/eas-tools/internal/logging"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// maxPages bounds the pages one Sync drain may take.
const maxPages = 50

// maxConsecutiveErrors ends an eas-sync job as failed.
const maxConsecutiveErrors = 10

// device is one of the devices of a client's account: its own ActiveSync
// session and device id. A job runs on all devices of a client at the same
// time, like the phone and the tablet of one user.
type device struct {
	c      *client
	number int // 1-based
	sess   *probe.Session

	// only touched by the goroutine that runs a job on this device (jobs of
	// a client never overlap)
	setupDone bool

	// recipients found in the directory for to=others
	rcpts    []string
	rcptUses int
}

// runJob runs the named job on every device of the client and returns its
// result (ok | failed | stopped) and a message. An unknown job or unknown
// parameters fail the job, never the worker.
func (c *client) runJob(ctx context.Context, j *job, params string) (string, string) {
	var run func(ctx context.Context, d *device) (string, string)

	// sim=... is for the mock server: it goes out as a request header of every
	// request of this job and is not a parameter of the job itself.
	params, sim, err := splitSim(params)
	if err != nil {
		return "failed", err.Error()
	}
	if sim != "" {
		for _, d := range c.devs {
			d.sess.SetHeader(probe.SimHeader, sim)
		}
		defer func() {
			for _, d := range c.devs {
				d.sess.SetHeader(probe.SimHeader, "")
			}
		}()
	}

	switch j.name {
	case JobProbe:
		if err := parseProbeParams(params); err != nil {
			return "failed", err.Error()
		}
		run = func(ctx context.Context, d *device) (string, string) { return d.jobProbe(ctx, j) }
	case JobSync:
		p, err := parseSyncParams(params)
		if err != nil {
			return "failed", err.Error()
		}
		run = func(ctx context.Context, d *device) (string, string) { return d.jobSync(ctx, j, p) }
	case JobMail:
		p, err := parseMailParams(params)
		if err != nil {
			return "failed", err.Error()
		}
		run = func(ctx context.Context, d *device) (string, string) { return d.jobMail(ctx, j, p) }
	case JobGal:
		p, err := parseGalParams(params)
		if err != nil {
			return "failed", err.Error()
		}
		run = func(ctx context.Context, d *device) (string, string) { return d.jobGal(ctx, j, p) }
	default:
		return "failed", fmt.Sprintf("unknown job %q", j.name)
	}

	return c.fanOut(ctx, run)
}

// fanOut runs the job on all devices at once. One failed device fails the
// job and stops the others: all devices share the account, so a refused
// login or a broken mailbox would hit each of them, and repeating it on every
// device is what gets accounts blocked.
func (c *client) fanOut(ctx context.Context, run func(context.Context, *device) (string, string)) (string, string) {
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		number          int
		result, message string
	}
	ch := make(chan outcome, len(c.devs))
	for _, d := range c.devs {
		go func() {
			r, m := run(jctx, d)
			ch <- outcome{d.number, r, m}
		}()
	}

	var failed, stopped, first *outcome
	for range c.devs {
		o := <-ch
		switch o.result {
		case "failed":
			if failed == nil {
				failed = &o
				cancel()
			}
		case "stopped":
			if stopped == nil {
				stopped = &o
			}
		default:
			if first == nil {
				first = &o
			}
		}
	}

	multi := len(c.devs) > 1
	switch {
	case failed != nil:
		if multi {
			return "failed", fmt.Sprintf("device %d: %s", failed.number, failed.message)
		}
		return "failed", failed.message
	case stopped != nil:
		return "stopped", stopped.message
	case multi:
		return "ok", fmt.Sprintf("%d devices ok; %s", len(c.devs), first.message)
	}
	return "ok", first.message
}

// step runs one session step with its own timeout and counts a failure by
// error class.
func (d *device) step(ctx context.Context, name string, timeout time.Duration, fn func(context.Context) error) error {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := fn(sctx)
	if err != nil && ctx.Err() == nil {
		class := probe.Classify(sctx, err)
		d.c.cfg.Stats.Failure(class)
		if class == "auth" || class == "forbidden" {
			d.c.log.Warn("login refused: check password, lockout and block lists; not retrying",
				"device", d.number, "step", name, "class", class)
		}
		return &stepError{name: name, err: err}
	}
	return err
}

// stepError names the step that failed and describes an EAS status with the
// table of its own command (the library's shared table can be wrong).
type stepError struct {
	name string
	err  error
}

func (e *stepError) Error() string { return e.name + ": " + probe.ErrorText(e.err) }
func (e *stepError) Unwrap() error { return e.err }

// setup brings the device to the point where it can sync: protocol
// version, registration, device information, folder list.
func (d *device) setup(ctx context.Context, j *job) error {
	t := d.c.cfg.StepTimeout

	j.setStatus(d.number, j.name+": options")
	if err := d.step(ctx, probe.StepOptions, t, func(ctx context.Context) error { return d.sess.Options(ctx, nil) }); err != nil {
		return err
	}
	j.setStatus(d.number, j.name+": provision")
	if err := d.step(ctx, probe.StepProvision, t, func(ctx context.Context) error { return d.sess.Provision(ctx, nil) }); err != nil {
		return err
	}
	if d.sess.HasDeviceInfo() {
		// Like the one-shot probe: reported (counted), but not fatal.
		_ = d.step(ctx, probe.StepSettings, t, func(ctx context.Context) error { return d.sess.DeviceInfo(ctx, nil) })
	}
	j.setStatus(d.number, j.name+": folders")
	return d.step(ctx, probe.StepFolderSync, t, func(ctx context.Context) error { return d.sess.FolderSync(ctx, nil) })
}

// outcome maps a job error to a result: a cancelled job was stopped.
func outcome(ctx context.Context, err error, ok string) (string, string) {
	switch {
	case err == nil:
		return "ok", ok
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return "stopped", "stopped"
	}
	return "failed", err.Error()
}

// jobProbe is the simple one-shot action: the whole flow once.
func (d *device) jobProbe(ctx context.Context, j *job) (string, string) {
	if err := d.setup(ctx, j); err != nil {
		return outcome(ctx, err, "")
	}
	j.setStatus(d.number, j.name+": sync")
	var res *probe.SyncResult
	err := d.step(ctx, probe.StepSync, d.c.cfg.StepTimeout, func(ctx context.Context) error {
		var err error
		res, err = d.sess.Sync(ctx, nil)
		return err
	})
	if err != nil {
		return outcome(ctx, err, "")
	}
	d.c.cfg.Stats.Items(res.Added, res.Changed, res.Deleted)
	return outcome(ctx, nil, fmt.Sprintf("probe ok, %d items", res.Added))
}

// jobSync keeps the mailbox in sync like a phone: sync, then wait (think
// time) or hold a Ping (push) and sync when the server reports changes.
func (d *device) jobSync(ctx context.Context, j *job, p syncParams) (string, string) {
	c := d.c
	if !d.setupDone {
		if err := d.setup(ctx, j); err != nil {
			return outcome(ctx, err, "")
		}
		d.setupDone = true
	}

	var deadline time.Time
	if p.Duration > 0 {
		deadline = time.Now().Add(p.Duration)
	}

	var syncs, pings, items, errs, consecutive int
	status := func() {
		j.setStatus(d.number, fmt.Sprintf("%s: %d syncs, %d pings, %d items, %d errors", j.name, syncs, pings, items, errs))
	}
	status()

	needSync := true
	for {
		if !c.gate(ctx) {
			return "stopped", fmt.Sprintf("stopped after %d syncs", syncs)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return "ok", fmt.Sprintf("completed: %d syncs, %d items, %d errors", syncs, items, errs)
		}

		var err error
		if needSync {
			var n int
			n, err = d.drain(ctx)
			if err == nil {
				syncs++
				items += n
			}
		}

		if err == nil {
			consecutive = 0
			needSync = true

			if p.Ping > 0 {
				var pr *probe.PingResult
				err = d.step(ctx, probe.StepPing, time.Duration(p.Ping)*time.Second+c.cfg.StepTimeout, func(ctx context.Context) error {
					var err error
					pr, err = d.sess.Ping(ctx, p.Ping, nil)
					return err
				})
				if err == nil {
					pings++
					needSync = pr.Changes
					// A server that answers a Ping without changes long before
					// the heartbeat would make a tight loop: wait instead.
					if !pr.Changes && pr.Held < time.Duration(pr.HeartbeatUsed)*time.Second/2 {
						status()
						if !sleep(ctx, p.think()) {
							return "stopped", fmt.Sprintf("stopped after %d syncs", syncs)
						}
					}
					status()
					continue
				}
			}
		}

		if err != nil {
			if ctx.Err() != nil {
				return "stopped", fmt.Sprintf("stopped after %d syncs", syncs)
			}
			errs++
			consecutive++
			logging.Trace(c.log, "eas-sync error", "device", d.number, "error", err)
			// A refused login is not retried: repeating a wrong password
			// is how accounts and client addresses get blocked.
			if class := probe.Classify(ctx, err); class == "auth" || class == "forbidden" {
				return "failed", fmt.Sprintf("login refused (%s): %v", class, err)
			}
			if consecutive >= maxConsecutiveErrors {
				return "failed", fmt.Sprintf("%d errors in a row, last: %v", consecutive, err)
			}
			needSync = true
		}

		status()
		if !sleep(ctx, p.think()) {
			return "stopped", fmt.Sprintf("stopped after %d syncs", syncs)
		}
	}
}

// drain runs Sync until the server has nothing more, and returns the number
// of items it brought.
func (d *device) drain(ctx context.Context) (int, error) {
	total := 0
	for page := 0; page < maxPages; page++ {
		var res *probe.SyncResult
		err := d.step(ctx, probe.StepSync, d.c.cfg.StepTimeout, func(ctx context.Context) error {
			var err error
			res, err = d.sess.Sync(ctx, nil)
			return err
		})
		if err != nil {
			return total, err
		}
		d.c.cfg.Stats.Items(res.Added, res.Changed, res.Deleted)
		total += res.Added + res.Changed

		if !res.MoreAvailable {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// galQueries is what one round of directory lookups searches for: the own
// address (self), the growing prefixes of it like a phone does while the user
// types (typeahead), or a fixed text. final marks the query that looks for the
// complete own address.
type galQuery struct {
	text  string
	final bool
}

func (d *device) galQueries(p galParams) []galQuery {
	user := d.sess.Username()
	switch {
	case p.Query != "":
		return []galQuery{{text: p.Query}}
	case p.Mode == "typeahead":
		base, _, _ := strings.Cut(strings.ToLower(user), "@")
		var qs []galQuery
		for n := 3; n < len(base) && n <= 10; n++ {
			qs = append(qs, galQuery{text: base[:n]})
		}
		return append(qs, galQuery{text: user, final: true})
	}
	return []galQuery{{text: user, final: true}}
}

// jobGal is the directory job: look for the own user in the directory (the
// Global Address List), over and over, like a user addressing a mail.
func (d *device) jobGal(ctx context.Context, j *job, p galParams) (string, string) {
	c := d.c
	if !d.setupDone {
		if err := d.setup(ctx, j); err != nil {
			return outcome(ctx, err, "")
		}
		d.setupDone = true
	}
	if !d.sess.SupportsDirectory() {
		return "failed", "the server does not advertise the Search command"
	}

	var deadline time.Time
	if p.Duration > 0 {
		deadline = time.Now().Add(p.Duration)
	}

	var lookups, found, missing, errs, consecutive int
	status := func() {
		j.setStatus(d.number, fmt.Sprintf("%s: %d lookups, own user found %d, missing %d, %d errors",
			j.name, lookups, found, missing, errs))
	}
	status()

	for {
		for _, q := range d.galQueries(p) {
			if !c.gate(ctx) {
				return "stopped", fmt.Sprintf("stopped after %d lookups", lookups)
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				return "ok", fmt.Sprintf("completed: %d lookups, own user found %d, missing %d, %d errors", lookups, found, missing, errs)
			}

			var res *probe.DirectoryResult
			err := d.step(ctx, probe.StepDirectory, c.cfg.StepTimeout, func(ctx context.Context) error {
				var err error
				res, err = d.sess.Directory(ctx, nil, q.text, q.final)
				return err
			})
			if err != nil {
				if ctx.Err() != nil {
					return "stopped", fmt.Sprintf("stopped after %d lookups", lookups)
				}
				errs++
				consecutive++
				if class := probe.Classify(ctx, err); class == "auth" || class == "forbidden" {
					return "failed", fmt.Sprintf("login refused (%s): %v", class, err)
				}
				if consecutive >= maxConsecutiveErrors {
					return "failed", fmt.Sprintf("%d errors in a row, last: %v", consecutive, err)
				}
			} else {
				consecutive = 0
				lookups++
				if res.SelfChecked {
					c.cfg.Stats.DirectorySelf(res.SelfFound)
					if res.SelfFound {
						found++
					} else {
						missing++
					}
				}
			}
			status()

			// the pause between two keystrokes
			if !sleep(ctx, 150*time.Millisecond+time.Duration(rand.Int64N(int64(250*time.Millisecond)))) {
				return "stopped", fmt.Sprintf("stopped after %d lookups", lookups)
			}
		}

		if !sleep(ctx, p.think()) {
			return "stopped", fmt.Sprintf("stopped after %d lookups", lookups)
		}
	}
}

// mailRecipients picks the recipients of the next mail. For to=others they come
// from the directory: the address book is searched for the accounts that look
// like this one (the name without its trailing digits), and one to three of
// the other users are picked.
func (d *device) mailRecipients(ctx context.Context, p mailParams, self string) ([]string, error) {
	switch p.To {
	case "self":
		return []string{self}, nil
	case "list":
		return p.List, nil
	}

	if len(d.rcpts) == 0 || d.rcptUses >= 50 {
		local, _, _ := strings.Cut(self, "@")
		query := strings.TrimRight(local, "0123456789")
		if len(query) < 2 {
			query = local
		}
		var res *probe.DirectoryResult
		err := d.step(ctx, probe.StepDirectory, d.c.cfg.StepTimeout, func(ctx context.Context) error {
			var err error
			res, err = d.sess.Directory(ctx, nil, query, false)
			return err
		})
		if err != nil {
			return nil, err
		}
		d.rcpts, d.rcptUses = d.rcpts[:0], 0
		for _, a := range res.Addresses {
			if !strings.EqualFold(a, self) {
				d.rcpts = append(d.rcpts, a)
			}
		}
		if len(d.rcpts) == 0 {
			return nil, fmt.Errorf("no other users found in the directory for %q (to=others)", query)
		}
	}
	d.rcptUses++

	n := 1 + rand.IntN(3)
	if n > len(d.rcpts) {
		n = len(d.rcpts)
	}
	out := make([]string, 0, n)
	for _, i := range rand.Perm(len(d.rcpts))[:n] {
		out = append(out, d.rcpts[i])
	}
	return out, nil
}

// jobMail sends mail through ActiveSync, over and over. To the account itself
// it also measures how long the mail takes to come back: a round trip through
// the server's mail routing. With cleanup the mail is deleted again and no copy
// is kept in Sent Items.
func (d *device) runMail(ctx context.Context, j *job, p mailParams, sentSubjects map[string]bool) (string, string) {
	c := d.c
	if !d.setupDone {
		if err := d.setup(ctx, j); err != nil {
			return outcome(ctx, err, "")
		}
		d.setupDone = true
	}
	self := d.sess.SelfAddress()
	if self == "" {
		return "failed", "the account has no e-mail address to send from (use the internet address as login)"
	}

	// Start from a drained Inbox, so that only new mail is seen later.
	if _, err := d.drain(ctx); err != nil {
		return outcome(ctx, err, "")
	}

	var deadline time.Time
	if p.Duration > 0 {
		deadline = time.Now().Add(p.Duration)
	}

	var sent, arrived, late, lost, errs, consecutive int
	status := func() {
		j.setStatus(d.number, fmt.Sprintf("%s: %d sent, %d arrived, %d late, %d lost, %d errors", j.name, sent, arrived, late, lost, errs))
	}
	status()

	for {
		if !c.gate(ctx) {
			return "stopped", fmt.Sprintf("stopped after %d mails", sent)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return "ok", fmt.Sprintf("completed: %d sent, %d arrived, %d late, %d lost, %d errors", sent, arrived, late, lost, errs)
		}
		if p.Count > 0 && sent >= p.Count {
			return "ok", fmt.Sprintf("completed: %d sent, %d arrived, %d late, %d lost, %d errors", sent, arrived, late, lost, errs)
		}

		var err error
		var rcpts []string
		if rcpts, err = d.mailRecipients(ctx, p, self); err == nil {
			n := sent + 1
			tag := fmt.Sprintf("%s-d%d-%d-%08x", c.key, d.number, n, rand.Uint32())
			subject := fmt.Sprintf("[easload %s d%d #%d]", c.key, d.number, n)

			if p.To == "self" {
				// the shared round trip: send, wait for the mail to come back, delete it
				var trip *probe.MailTrip
				err = d.step(ctx, probe.StepMail, 2*c.cfg.StepTimeout+p.Wait, func(ctx context.Context) error {
					var e error
					trip, e = d.sess.MailRoundTrip(ctx, nil, probe.MailTripOptions{
						Wait: p.Wait, ExtraWait: p.Wait, Arrival: p.Arrival, Keep: !p.Cleanup, SaveSent: p.SaveSent,
						SizeKB: float64(p.sizeBytes()) / 1024, Subject: subject, Tag: tag,
						Attachments: p.Files, AttachKB: float64(p.attachBytes()) / 1024, AttachBinary: p.AttachBinary,
					})
					return e
				})
				if trip != nil && p.Cleanup && (trip.Arrived || trip.Late) && !trip.Deleted {
					c.cfg.Stats.MailCleanupFailed(subject)
					c.log.Warn("mail not deleted: it stays in the mailbox", "device", d.number, "subject", subject, "error", trip.CleanupError)
				}
				if trip != nil && trip.Sent {
					sentSubjects[subject] = true
					sent++
					c.cfg.Stats.MailSent("self")
				}
				switch {
				case err == nil:
					arrived++
					c.cfg.Stats.MailArrival(trip.Elapsed)
					if trip.PushChecked {
						c.cfg.Stats.MailPush(trip.PushWake)
					}
				case errors.Is(err, probe.ErrMailLost), errors.Is(err, probe.ErrMailLate):
					if errors.Is(err, probe.ErrMailLate) {
						late++
					} else {
						lost++
					}
					info := MissedMail{Subject: subject, Device: d.number}
					if trip != nil {
						info.Late, info.LateSeconds = trip.Late, trip.LateAfter.Seconds()
						info.Polls, info.SeenMails, info.SyncKey = trip.Polls, trip.SeenMails, trip.LastKey
						info.Timeline = trip.Timeline
						if trip.HasReceived {
							v := trip.ReceivedAfter.Seconds()
							info.ReceivedAfter = &v
						}
					}
					c.cfg.Stats.MailMissed(info)
					kind := "mail lost"
					if info.Late {
						kind = "mail late"
					}
					c.log.Warn(kind, "device", d.number, "subject", subject,
						"late_seconds", int(info.LateSeconds), "server_received_after", receivedText(info.ReceivedAfter), "syncs", info.Polls, "mails_seen", info.SeenMails, "sync_key", info.SyncKey)
					// what the device was told while it waited, for the first lost mails of the run
					if c.cfg.Stats.MissedMailsLogged() <= maxMissedTimelines {
						for _, line := range info.Timeline {
							c.log.Warn(kind+": timeline", "device", d.number, "subject", subject, "event", line)
						}
					}
					err = nil
				}
			} else {
				msg := probe.Mail{To: rcpts, Subject: subject, Size: p.sizeBytes(), SkipSaveInSent: !p.SaveSent,
					Attachments: p.Files, AttachSize: p.attachBytes(), AttachBinary: p.AttachBinary}
				err = d.step(ctx, probe.StepSendMail, c.cfg.StepTimeout, func(ctx context.Context) error {
					return d.sess.SendMail(ctx, nil, msg, tag)
				})
				if err == nil {
					sent++
					sentSubjects[subject] = true
					c.cfg.Stats.MailSent(p.To)
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return "stopped", fmt.Sprintf("stopped after %d mails", sent)
			}
			errs++
			consecutive++
			logging.Trace(c.log, "eas-mail error", "device", d.number, "error", err)
			if class := probe.Classify(ctx, err); class == "auth" || class == "forbidden" {
				return "failed", fmt.Sprintf("login refused (%s): %v", class, err)
			}
			if consecutive >= maxConsecutiveErrors {
				return "failed", fmt.Sprintf("%d errors in a row, last: %v", consecutive, err)
			}
		} else {
			consecutive = 0
		}

		status()
		if !sleep(ctx, p.think()) {
			return "stopped", fmt.Sprintf("stopped after %d mails", sent)
		}
	}
}

// jobMail runs the mail job and, when the job did not ask for a copy in Sent Items (save_sent), removes the copies that
// the server keeps anyway: only the mails this device sent in this run, found by their exact subject.
func (d *device) jobMail(ctx context.Context, j *job, p mailParams) (string, string) {
	sent := map[string]bool{}
	result, msg := d.runMail(ctx, j, p, sent)
	if p.SaveSent || 0 == len(sent) {
		return result, msg
	}

	// the job may have ended because its context was cancelled: the cleanup gets its own time
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	deleted, failed, err := d.sess.DeleteSentCopies(cctx, sent)
	d.c.cfg.Stats.SentCopies(deleted, failed)
	switch {
	case err != nil:
		d.c.log.Warn("deleting the copies in Sent Items failed", "device", d.number, "error", probe.ErrorText(err))
		msg += fmt.Sprintf("; Sent Items cleanup failed: %s", probe.ErrorText(err))
	default:
		d.c.log.Debug("deleted the copies in Sent Items", "device", d.number, "deleted", deleted, "failed", failed, "sent", len(sent))
		msg += fmt.Sprintf("; %d copies deleted from Sent Items", deleted)
		if failed > 0 {
			msg += fmt.Sprintf(" (%d failed)", failed)
		}
	}
	return result, msg
}

// receivedText describes when the server says it received a mail, for the log.
func receivedText(v *float64) string {
	if nil == v {
		return "unknown"
	}
	return fmt.Sprintf("%+.1fs", *v)
}
