// Package mailmonitor measures the round trip of a mail to an address outside: the account of an ActiveSync target sends
// a mail to a remote address, something there (for example an agent that replies) answers it, and the time until the
// answer shows up in the Inbox of the account is the round-trip time.
//
// One goroutine (Runner.Run) serves all targets: it sends the mails that are due, looks at the Inbox of the accounts
// that wait for an answer, counts the mails that were not answered in time as lost and removes everything it created.
package mailmonitor

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/probe"
)

// Target is one remote address that is probed.
type Target struct {
	Name     string
	To       string        // the internet address the mail is sent to
	Account  string        // the name of the ActiveSync account that sends and receives
	Interval time.Duration // time between two mails
	Timeout  time.Duration // an answer later than this counts as lost
}

// Settings are what the Runner works from; they can be replaced while it runs.
type Settings struct {
	Targets map[string]Target
	// Poll is the time between two looks at the Inbox of an account that waits for an answer.
	Poll time.Duration
	// AuthBackoff is the pause after a refused login, so that a wrong password is not tried again and again.
	AuthBackoff time.Duration
	// Fingerprints say what each account is made of (a hash of its settings). An account whose fingerprint changed gets a new
	// session, so that a new password or server is used without a restart.
	Fingerprints map[string]string
}

// Mail is a mail of the Inbox as far as the Runner needs it.
type Mail struct {
	ID      string
	Subject string
}

// Mailbox is the account of one ActiveSync session, ready to send and to read the Inbox.
type Mailbox interface {
	// Send sends a mail from the account. The copy in Sent Items is not wanted.
	Send(ctx context.Context, to, subject, body, tag string) error
	// Poll returns the mails that arrived in the Inbox since the last call (the first call: all of them).
	Poll(ctx context.Context) ([]Mail, error)
	// Delete deletes a mail of the Inbox.
	Delete(ctx context.Context, id string) error
	// DeleteSent deletes the copies in Sent Items that have one of these subjects.
	DeleteSent(ctx context.Context, subjects map[string]bool) (deleted, failed int, err error)
	Close()
}

// Opener brings the session of an account up to the point where it can send and receive.
type Opener func(ctx context.Context, account string) (Mailbox, error)

const (
	callTimeout = 60 * time.Second
	openTimeout = 3 * time.Minute
	retryAfter  = 60 * time.Second // after a failure that is not a refused login
	cleanEvery  = 30 * time.Second // time between two cleanups of Sent Items
)

// The subject is "[nshmail-probe] probe-id=NAME-MS probe-posted=TIME": the tag and the two parameters that the agent at
// the other end looks for (it also knows the older tag [nshmailmon]). The answer keeps them, whatever it puts before or
// after ("Re: ..."); the probe finds its mail again by probe-id, which holds the target and the time of sending.
var (
	subjectTag = regexp.MustCompile(`\[nshmail-probe\]|\[nshmailmon\]`)
	replyTag   = regexp.MustCompile(`probe-id=([A-Za-z0-9._-]{1,64})-(\d{10,})`)
)

// Subject is the subject of the mail that a target sends at the given time (Unix milliseconds). The time is part of it,
// so that an answer still counts after a restart and a leftover can be recognized as one of ours.
func Subject(name string, ms int64) string {
	return fmt.Sprintf("[nshmail-probe] probe-id=%s-%d probe-posted=%s", name, ms, time.UnixMilli(ms).UTC().Format("20060102150405"))
}

// Runner runs the mail monitor targets.
type Runner struct {
	open Opener
	log  *slog.Logger
	now  func() time.Time

	set atomic.Pointer[Settings]

	mu    sync.Mutex // guards stats and up, which the metrics read
	stats map[string]*Stats
	up    map[string]bool

	cleanupFailed atomic.Int64

	// only the goroutine of Run touches these
	accts map[string]*account
	last  map[string]time.Time // when the last mail of a target was sent

	// what Debug shows: copies that the goroutine of Run publishes after every step
	pub     map[string]AccountDebug
	pubNext map[string]string
}

type account struct {
	mb         Mailbox
	fp         string // fingerprint of the settings the session was opened with
	retryAt    time.Time
	initial    bool // the first look at the Inbox is still to do
	lastPoll   time.Time
	lastPollOK time.Time
	lastClean  time.Time
	pollUntil  time.Time // keep looking until then, to see an answer that comes late
	pending    map[string]*pending
	done       map[string]time.Time // tags that were counted, so that a second answer does not count again
	clean      map[string]bool      // subjects whose copy in Sent Items is to be deleted
	lastErr    string               // why the last attempt with the account failed; empty while it works
}

type pending struct {
	target  string
	at      time.Time
	subject string
}

// New returns a Runner that opens the accounts with open.
func New(open Opener, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{open: open, log: log, now: time.Now,
		stats: map[string]*Stats{}, up: map[string]bool{},
		accts: map[string]*account{}, last: map[string]time.Time{}}
}

// SetSettings replaces the settings. A target that stays keeps its numbers; one that is gone loses them.
func (r *Runner) SetSettings(s Settings) {
	if s.Poll <= 0 {
		s.Poll = 2 * time.Second
	}
	if s.AuthBackoff <= 0 {
		s.AuthBackoff = 10 * time.Minute
	}
	prev := r.set.Load()
	r.set.Store(&s)
	r.logChanges(prev, &s)

	r.mu.Lock()
	for name := range r.stats {
		if _, ok := s.Targets[name]; !ok {
			delete(r.stats, name)
			delete(r.up, name)
		}
	}
	for name := range s.Targets {
		if r.stats[name] == nil {
			r.stats[name] = &Stats{}
		}
	}
	r.mu.Unlock()
}

// Run looks after the targets until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		r.Step(ctx)
		select {
		case <-ctx.Done():
			for _, a := range r.accts {
				if a.mb != nil {
					a.mb.Close()
				}
			}
			return
		case <-t.C:
		}
	}
}

// Step does what is due now: send, look, expire, clean up. Run calls it every second.
func (r *Runner) Step(ctx context.Context) {
	s := r.set.Load()
	if s == nil {
		return
	}
	byAccount := map[string][]string{}
	for name, t := range s.Targets {
		byAccount[t.Account] = append(byAccount[t.Account], name)
	}
	names := map[string]bool{}
	for n := range byAccount {
		names[n] = true
	}
	for n := range r.accts {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		if ctx.Err() != nil {
			return
		}
		targets := byAccount[name]
		sort.Strings(targets)
		a := r.accts[name]
		if len(targets) == 0 { // the last target of the account was removed
			if a != nil && a.mb != nil {
				a.mb.Close()
			}
			delete(r.accts, name)
			r.mu.Lock()
			delete(r.pub, name)
			r.mu.Unlock()
			continue
		}
		if a == nil {
			a = &account{pending: map[string]*pending{}, done: map[string]time.Time{}, clean: map[string]bool{}}
			r.accts[name] = a
		}
		if fp := s.Fingerprints[name]; fp != a.fp {
			if a.mb != nil {
				r.log.Info("mail monitor: the settings of the account changed, opening a new session", "account", name)
				a.mb.Close()
				a.mb, a.retryAt = nil, time.Time{}
			}
			a.fp = fp
		}
		r.stepAccount(ctx, s, name, a, targets)
		r.publish(name, a, targets)
	}
	for n := range r.last {
		if _, ok := s.Targets[n]; !ok {
			delete(r.last, n)
		}
	}
}

func (r *Runner) stepAccount(ctx context.Context, s *Settings, name string, a *account, targets []string) {
	if a.mb == nil {
		if r.now().Before(a.retryAt) {
			return
		}
		octx, cancel := context.WithTimeout(ctx, openTimeout)
		mb, err := r.open(octx, name)
		cancel()
		if err != nil {
			r.fail(ctx, s, name, a, targets, "opening the account", err)
			return
		}
		a.mb, a.initial = mb, true
		a.lastErr = ""
		r.log.Info("mail monitor: account is ready", "account", name)
	}
	r.setUp(targets, true)

	for _, tn := range targets {
		t := s.Targets[tn]
		// due is counted from the last mail, so a changed interval applies at once
		if last, known := r.last[tn]; known && r.now().Before(last.Add(t.Interval)) {
			continue
		}
		r.last[tn] = r.now()
		r.send(ctx, s, name, a, targets, t)
		if a.mb == nil {
			return
		}
	}

	if a.initial || len(a.pending) > 0 || r.now().Before(a.pollUntil) {
		if r.now().Sub(a.lastPoll) >= s.Poll {
			r.poll(ctx, s, name, a, targets)
			if a.mb == nil {
				return
			}
		}
	}

	r.expire(s, a)
	r.cleanup(ctx, a)

	count := map[string]int{}
	for _, p := range a.pending {
		count[p.target]++
	}
	for _, tn := range targets {
		r.statsOf(tn).SetPending(count[tn])
	}
}

func (r *Runner) send(ctx context.Context, s *Settings, account string, a *account, targets []string, t Target) {
	t0 := r.now()
	ms := t0.UnixMilli()
	subject := Subject(t.Name, ms)
	tag := fmt.Sprintf("%s-%d", t.Name, ms)
	body := "This is an automatic mail of an ActiveSync round-trip probe (easprobe).\n" +
		"Please answer it with a reply that keeps the subject.\n\n" + tag + "\n"

	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	err := a.mb.Send(cctx, t.To, subject, body, tag)
	cancel()
	st := r.statsOf(t.Name)
	if err != nil {
		st.SendError()
		r.log.Warn("mail monitor: sending failed", "target", t.Name, "account", account, "error", probe.ErrorText(err))
		if class := probe.Classify(ctx, err); class == "auth" || class == "forbidden" {
			r.fail(ctx, s, account, a, targets, "sending", err)
		}
		return
	}
	st.Sent(t0)
	a.pending[tag] = &pending{target: t.Name, at: t0, subject: subject}
	a.pollUntil = t0.Add(2 * t.Timeout)
	r.log.Debug("mail monitor: sent", "target", t.Name, "to", t.To)
}

func (r *Runner) poll(ctx context.Context, s *Settings, account string, a *account, targets []string) {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	mails, err := a.mb.Poll(cctx)
	cancel()
	if err != nil {
		r.fail(ctx, s, account, a, targets, "looking at the Inbox", err)
		return
	}
	a.lastPoll = r.now()
	a.lastPollOK = a.lastPoll
	a.initial = false
	for _, m := range mails {
		r.handle(ctx, s, a, m)
	}
}

// handle deals with one mail of the Inbox. Only mail with the tag of a probe is touched.
func (r *Runner) handle(ctx context.Context, s *Settings, a *account, m Mail) {
	f := replyTag.FindStringSubmatch(m.Subject)
	if f == nil || !subjectTag.MatchString(m.Subject) {
		return
	}
	name, key := f[1], f[1]+"-"+f[2]
	now := r.now()

	if _, again := a.done[key]; !again {
		a.done[key] = now
		var ms int64
		fmt.Sscan(f[2], &ms)
		a.clean[Subject(name, ms)] = true

		st := r.statsOfKnown(name)
		switch p := a.pending[key]; {
		case st == nil:
			r.log.Debug("mail monitor: answer of a target that is not configured any more, deleted", "target", name)
		case p != nil:
			rtt := now.Sub(p.at)
			st.Received(now, rtt, key)
			delete(a.pending, key)
			r.log.Info("mail monitor: answered", "target", name, "round_trip", rtt.Round(10*time.Millisecond))
		default:
			age := now.Sub(time.UnixMilli(ms))
			switch {
			case age > LostWindow:
				r.log.Debug("mail monitor: old answer deleted", "target", name, "age", age.Round(time.Second))
			case age <= s.Targets[name].Timeout: // sent before a restart, answered in time
				st.Received(now, age, key)
				r.log.Info("mail monitor: answered", "target", name, "round_trip", age.Round(10*time.Millisecond))
			default:
				st.Late(now, age, key)
				r.log.Warn("mail monitor: answer came after the wait time", "target", name, "after", age.Round(time.Second))
			}
		}
	}

	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	err := a.mb.Delete(cctx, m.ID)
	cancel()
	if err != nil {
		r.cleanupFailed.Add(1)
		r.log.Warn("mail monitor: cannot delete the answer", "error", probe.ErrorText(err))
	}
}

// expire counts the mails that were not answered in time as lost. It does so only when the Inbox was looked at after
// the time was over: while the account cannot be read, nothing is known.
func (r *Runner) expire(s *Settings, a *account) {
	for key, p := range a.pending {
		t, ok := s.Targets[p.target]
		if !ok {
			delete(a.pending, key)
			continue
		}
		over := p.at.Add(t.Timeout)
		if r.now().After(over) && a.lastPollOK.After(over) {
			delete(a.pending, key)
			a.clean[p.subject] = true
			r.statsOf(p.target).Lost(r.now(), key, p.at)
			r.log.Warn("mail monitor: no answer", "target", p.target, "waited", t.Timeout)
		}
	}
	for key, at := range a.done {
		if r.now().Sub(at) > LostWindow+time.Hour {
			delete(a.done, key)
		}
	}
}

func (r *Runner) cleanup(ctx context.Context, a *account) {
	if len(a.clean) == 0 || r.now().Sub(a.lastClean) < cleanEvery {
		return
	}
	a.lastClean = r.now()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	deleted, failed, err := a.mb.DeleteSent(cctx, a.clean)
	cancel()
	if err != nil {
		r.log.Warn("mail monitor: cleaning up Sent Items failed, will try again", "error", probe.ErrorText(err))
		return
	}
	if failed > 0 {
		r.cleanupFailed.Add(int64(failed))
	}
	logging.Trace(r.log, "mail monitor: copies in Sent Items deleted", "deleted", deleted, "failed", failed)
	a.clean = map[string]bool{}
}

// fail ends the session of an account after an error. The next try is after a pause, a long one after a refused login.
func (r *Runner) fail(ctx context.Context, s *Settings, account string, a *account, targets []string, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	pause := retryAfter
	if class := probe.Classify(ctx, err); class == "auth" || class == "forbidden" {
		pause = s.AuthBackoff
	}
	if a.mb != nil {
		a.mb.Close()
		a.mb = nil
	}
	a.retryAt = r.now().Add(pause)
	r.setUp(targets, false)
	a.lastErr = what + ": " + probe.ErrorText(err)
	r.log.Warn("mail monitor: account failed", "account", account, "while", what, "error", probe.ErrorText(err), "retry_in", pause)
}

func (r *Runner) setUp(targets []string, up bool) {
	r.mu.Lock()
	for _, t := range targets {
		r.up[t] = up
	}
	r.mu.Unlock()
}

func (r *Runner) statsOf(name string) *Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stats[name] == nil {
		r.stats[name] = &Stats{}
	}
	return r.stats[name]
}

// statsOfKnown returns the numbers of a configured target, nil for a name that is not one.
func (r *Runner) statsOfKnown(name string) *Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats[name]
}

// Snapshot returns the numbers of a target, for tests and the metrics.
func (r *Runner) Snapshot(name string) (Snapshot, bool) {
	st := r.statsOfKnown(name)
	if st == nil {
		return Snapshot{}, false
	}
	return st.Snapshot(r.now()), true
}

// logChanges says what a new configuration changed: an entry that was added, removed or changed. The first settings
// (the start) are not logged here, the start has its own line. Nothing is logged when nothing changed.
func (r *Runner) logChanges(prev, now *Settings) {
	if prev == nil {
		return
	}
	names := map[string]bool{}
	for n := range prev.Targets {
		names[n] = true
	}
	for n := range now.Targets {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, n := range sorted {
		old, was := prev.Targets[n]
		cur, is := now.Targets[n]
		switch {
		case !was:
			r.log.Info("mail monitor: entry added", "name", n, "to", cur.To, "account", cur.Account, "interval", cur.Interval, "timeout", cur.Timeout)
		case !is:
			r.log.Info("mail monitor: entry removed", "name", n)
		default:
			var what []any
			if old.To != cur.To {
				what = append(what, "to", old.To+" -> "+cur.To)
			}
			if old.Account != cur.Account {
				what = append(what, "account", old.Account+" -> "+cur.Account)
			}
			if old.Interval != cur.Interval {
				what = append(what, "interval", old.Interval.String()+" -> "+cur.Interval.String())
			}
			if old.Timeout != cur.Timeout {
				what = append(what, "timeout", old.Timeout.String()+" -> "+cur.Timeout.String())
			}
			if len(what) > 0 {
				r.log.Info("mail monitor: entry changed", append([]any{"name", n}, what...)...)
			}
		}
	}
	if prev.Poll != now.Poll {
		r.log.Info("mail monitor: poll interval changed", "poll", prev.Poll.String()+" -> "+now.Poll.String())
	}
}
