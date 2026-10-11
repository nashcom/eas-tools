package load

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// bounds are the upper limits (seconds) of the latency histogram buckets.
var bounds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type opStat struct {
	ok, errs int64
	sum      float64
	max      float64
	counts   [10]int64 // len(bounds)+1, the last one is +Inf
	status   map[int]int64
}

// Stats collects the numbers of all clients of a worker. All methods are
// safe for concurrent use.
type Stats struct {
	mu       sync.Mutex
	started  time.Time
	ops      map[string]*opStat
	sent     int64
	recv     int64
	jobs     map[string]int64 // "job|result"
	clients  map[string]int64
	items    map[string]int64
	failures map[string]int64
	dir      map[string]int64 // own-user lookups: found | missing

	mailSent                map[string]int64 // by recipient kind: self | others | list
	mailLost                int64
	mailLate                int64 // lost mails that showed up after the wait time
	mailCleanupFailed       int64
	sentDeleted, sentFailed int64        // copies in Sent Items deleted at the end of the mail job, and not deleted
	leftBehind              []string     // subjects of the mails that could not be deleted
	missedMails             []MissedMail // the first missedMailsKept lost mails
	mailArrival             mailHist
	mailPush                map[string]int64 // woke | missed
}

// mailBounds are the upper limits (seconds) of the mail round trip histogram.
var mailBounds = []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120}

type mailHist struct {
	counts [11]int64
	sum    float64
	n      int64
}

// NewStats returns empty statistics.
func NewStats() *Stats {
	return &Stats{
		started:  time.Now(),
		ops:      map[string]*opStat{},
		jobs:     map[string]int64{},
		clients:  map[string]int64{},
		items:    map[string]int64{},
		failures: map[string]int64{},
		dir:      map[string]int64{},
		mailSent: map[string]int64{},
		mailPush: map[string]int64{},
	}
}

// Observe records one HTTP round trip (it is the probe's OnExchange hook).
func (s *Stats) Observe(x probe.Exchange) {
	op := x.Cmd
	if op == "" {
		op = x.Method
	}
	d := x.Duration.Seconds()

	s.mu.Lock()
	defer s.mu.Unlock()

	o := s.ops[op]
	if o == nil {
		o = &opStat{status: map[int]int64{}}
		s.ops[op] = o
	}
	if x.Err != "" || x.Status >= 400 || x.Status == 0 {
		o.errs++
	} else {
		o.ok++
	}
	o.status[x.Status]++
	o.sum += d
	if d > o.max {
		o.max = d
	}
	i := 0
	for i < len(bounds) && d > bounds[i] {
		i++
	}
	o.counts[i]++
	s.sent += x.ReqBytes
	s.recv += x.RespBytes
}

// LoginRefusals returns how many steps were refused a login (401/403).
func (s *Stats) LoginRefusals() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failures["auth"] + s.failures["forbidden"]
}

// Job counts a job that ended.
func (s *Stats) Job(job, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.jobs[job+"|"+result]++
}

// ClientState moves one client from one reported state to another.
func (s *Stats) ClientState(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if from != "" {
		s.clients[from]--
	}
	s.clients[to]++
}

// Items counts mail items seen by Sync.
func (s *Stats) Items(added, changed, deleted int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items["added"] += int64(added)
	s.items["changed"] += int64(changed)
	s.items["deleted"] += int64(deleted)
}

// DirectorySelf counts an own-user directory lookup.
func (s *Stats) DirectorySelf(found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if found {
		s.dir["found"]++
	} else {
		s.dir["missing"]++
	}
}

// MailSent counts a sent mail by recipient kind.
func (s *Stats) MailSent(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mailSent[kind]++
}

// MailArrival records the time a sent mail took to come back.
func (s *Stats) MailArrival(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v := d.Seconds()
	i := 0
	for i < len(mailBounds) && v > mailBounds[i] {
		i++
	}
	s.mailArrival.counts[i]++
	s.mailArrival.sum += v
	s.mailArrival.n++
}

// MailPush counts the push check of a mail round trip: a pending Ping that was
// woken by the mail, or a mail that arrived without waking it.
func (s *Stats) MailPush(woke bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if woke {
		s.mailPush["woke"]++
	} else {
		s.mailPush["missed"]++
	}
}

// MailTotals returns sent (all kinds), arrived and lost mails.
func (s *Stats) MailTotals() (sent, arrived, lost int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, n := range s.mailSent {
		sent += n
	}
	return sent, s.mailArrival.n, s.mailLost
}

// Failure counts a failed step by error class.
func (s *Stats) Failure(class string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failures[class]++
}

// quantile estimates a latency quantile from the histogram (linear inside
// the bucket); the last bucket reports its lower bound. The estimate never
// exceeds the exact maximum, which the wide buckets could otherwise overshoot.
func (o *opStat) quantile(q float64) float64 {
	v := o.estimate(q)
	if v > o.max {
		v = o.max
	}
	return v
}

func (o *opStat) estimate(q float64) float64 {
	var total int64
	for _, c := range o.counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var cum float64
	for i, c := range o.counts {
		next := cum + float64(c)
		if next >= target && c > 0 {
			if i >= len(bounds) {
				return bounds[len(bounds)-1]
			}
			lo := 0.0
			if i > 0 {
				lo = bounds[i-1]
			}
			return lo + (bounds[i]-lo)*(target-cum)/float64(c)
		}
		cum = next
	}
	return bounds[len(bounds)-1]
}

// Summary is a short human readable status.
func (s *Stats) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "up %s; clients:", time.Since(s.started).Round(time.Second))
	for _, st := range sortedKeys(s.clients) {
		if s.clients[st] > 0 {
			fmt.Fprintf(&b, " %s=%d", st, s.clients[st])
		}
	}
	var jobs []string
	for k, n := range s.jobs {
		jobs = append(jobs, fmt.Sprintf("%s=%d", strings.ReplaceAll(k, "|", "/"), n))
	}
	sort.Strings(jobs)
	if len(jobs) > 0 {
		fmt.Fprintf(&b, "; jobs: %s", strings.Join(jobs, " "))
	}
	if len(s.failures) > 0 {
		var fl []string
		for _, k := range sortedKeys(s.failures) {
			fl = append(fl, fmt.Sprintf("%s=%d", k, s.failures[k]))
		}
		fmt.Fprintf(&b, "; failed steps: %s", strings.Join(fl, " "))
	}
	if len(s.dir) > 0 {
		fmt.Fprintf(&b, "; own user in directory: found=%d missing=%d", s.dir["found"], s.dir["missing"])
	}
	if len(s.mailSent) > 0 {
		var sent int64
		for _, n := range s.mailSent {
			sent += n
		}
		fmt.Fprintf(&b, "; mail: sent=%d arrived=%d late=%d lost=%d", sent, s.mailArrival.n, s.mailLate, s.mailLost)
		if s.sentDeleted+s.sentFailed > 0 {
			fmt.Fprintf(&b, " sent_copies_deleted=%d", s.sentDeleted)
			if s.sentFailed > 0 {
				fmt.Fprintf(&b, " sent_copies_failed=%d", s.sentFailed)
			}
		}
		if s.mailCleanupFailed > 0 {
			fmt.Fprintf(&b, " cleanup_failed=%d", s.mailCleanupFailed)
		}
	}
	if n := s.failures["auth"] + s.failures["forbidden"]; n > 0 {
		fmt.Fprintf(&b, "\n  !! %d step(s) were refused a login (401/403): check passwords, lockouts and block lists", n)
	}
	for _, op := range sortedKeys(s.ops) {
		o := s.ops[op]
		fmt.Fprintf(&b, "\n  %-10s n=%-6d err=%-4d p50=%.2fs p95=%.2fs", op, o.ok+o.errs, o.errs, o.quantile(0.5), o.quantile(0.95))
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WritePrometheus renders the statistics in the Prometheus text format.
func (s *Stats) WritePrometheus(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintln(w, "# HELP eas_load_requests_total HTTP requests by ActiveSync command and result.")
	fmt.Fprintln(w, "# TYPE eas_load_requests_total counter")
	for _, op := range sortedKeys(s.ops) {
		o := s.ops[op]
		fmt.Fprintf(w, "eas_load_requests_total{op=%q,result=\"ok\"} %d\n", op, o.ok)
		fmt.Fprintf(w, "eas_load_requests_total{op=%q,result=\"error\"} %d\n", op, o.errs)
	}

	fmt.Fprintln(w, "# HELP eas_load_http_status_total HTTP requests by command and status code (0 = no response).")
	fmt.Fprintln(w, "# TYPE eas_load_http_status_total counter")
	for _, op := range sortedKeys(s.ops) {
		codes := make([]int, 0, len(s.ops[op].status))
		for c := range s.ops[op].status {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			fmt.Fprintf(w, "eas_load_http_status_total{op=%q,code=\"%d\"} %d\n", op, c, s.ops[op].status[c])
		}
	}

	fmt.Fprintln(w, "# HELP eas_load_request_duration_seconds Request duration by ActiveSync command.")
	fmt.Fprintln(w, "# TYPE eas_load_request_duration_seconds histogram")
	for _, op := range sortedKeys(s.ops) {
		o := s.ops[op]
		var cum int64
		for i, le := range bounds {
			cum += o.counts[i]
			fmt.Fprintf(w, "eas_load_request_duration_seconds_bucket{op=%q,le=\"%g\"} %d\n", op, le, cum)
		}
		cum += o.counts[len(bounds)]
		fmt.Fprintf(w, "eas_load_request_duration_seconds_bucket{op=%q,le=\"+Inf\"} %d\n", op, cum)
		fmt.Fprintf(w, "eas_load_request_duration_seconds_sum{op=%q} %g\n", op, o.sum)
		fmt.Fprintf(w, "eas_load_request_duration_seconds_count{op=%q} %d\n", op, cum)
	}

	fmt.Fprintln(w, "# HELP eas_load_bytes_total HTTP payload bytes.")
	fmt.Fprintln(w, "# TYPE eas_load_bytes_total counter")
	fmt.Fprintf(w, "eas_load_bytes_total{direction=\"sent\"} %d\n", s.sent)
	fmt.Fprintf(w, "eas_load_bytes_total{direction=\"received\"} %d\n", s.recv)

	fmt.Fprintln(w, "# HELP eas_load_clients Simulated devices by reported state.")
	fmt.Fprintln(w, "# TYPE eas_load_clients gauge")
	for _, st := range sortedKeys(s.clients) {
		fmt.Fprintf(w, "eas_load_clients{state=%q} %d\n", st, s.clients[st])
	}

	fmt.Fprintln(w, "# HELP eas_load_jobs_total Jobs that ended, by job and result.")
	fmt.Fprintln(w, "# TYPE eas_load_jobs_total counter")
	for _, k := range sortedKeys(s.jobs) {
		job, result, _ := strings.Cut(k, "|")
		fmt.Fprintf(w, "eas_load_jobs_total{job=%q,result=%q} %d\n", job, result, s.jobs[k])
	}

	fmt.Fprintln(w, "# HELP eas_load_items_total Mail items seen by Sync.")
	fmt.Fprintln(w, "# TYPE eas_load_items_total counter")
	for _, k := range sortedKeys(s.items) {
		fmt.Fprintf(w, "eas_load_items_total{kind=%q} %d\n", k, s.items[k])
	}

	fmt.Fprintln(w, "# HELP eas_load_directory_self_total Directory lookups of the own user, by result.")
	fmt.Fprintln(w, "# TYPE eas_load_directory_self_total counter")
	for _, k := range sortedKeys(s.dir) {
		fmt.Fprintf(w, "eas_load_directory_self_total{result=%q} %d\n", k, s.dir[k])
	}

	fmt.Fprintln(w, "# HELP eas_load_mail_sent_total Mails sent with SendMail, by recipient kind.")
	fmt.Fprintln(w, "# TYPE eas_load_mail_sent_total counter")
	for _, k := range sortedKeys(s.mailSent) {
		fmt.Fprintf(w, "eas_load_mail_sent_total{to=%q} %d\n", k, s.mailSent[k])
	}
	fmt.Fprintln(w, "# HELP eas_load_mail_push_total Mail round trips by push result: the pending Ping was woken, or the mail arrived without waking it.")
	fmt.Fprintln(w, "# TYPE eas_load_mail_push_total counter")
	for _, k := range sortedKeys(s.mailPush) {
		fmt.Fprintf(w, "eas_load_mail_push_total{result=%q} %d\n", k, s.mailPush[k])
	}
	fmt.Fprintln(w, "# HELP eas_load_mail_lost_total Mails to self that did not arrive, not even in the extra wait after the wait time.")
	fmt.Fprintln(w, "# TYPE eas_load_mail_lost_total counter")
	fmt.Fprintf(w, "eas_load_mail_lost_total %d\n", s.mailLost)
	fmt.Fprintln(w, "# HELP eas_load_mail_late_total Mails to self that did not arrive within the wait time but showed up in the extra wait (not counted in lost).")
	fmt.Fprintln(w, "# TYPE eas_load_mail_late_total counter")
	fmt.Fprintf(w, "eas_load_mail_late_total %d\n", s.mailLate)
	fmt.Fprintln(w, "# HELP eas_load_mail_cleanup_failed_total Mails that came back but could not be deleted again (they stay in the mailbox).")
	fmt.Fprintln(w, "# TYPE eas_load_mail_cleanup_failed_total counter")
	fmt.Fprintf(w, "eas_load_mail_cleanup_failed_total %d\n", s.mailCleanupFailed)
	fmt.Fprintln(w, "# HELP eas_load_mail_sent_copies_total Copies in Sent Items that the server kept; deleted at the end of a mail job, by result.")
	fmt.Fprintln(w, "# TYPE eas_load_mail_sent_copies_total counter")
	fmt.Fprintf(w, "eas_load_mail_sent_copies_total{result=\"deleted\"} %d\n", s.sentDeleted)
	fmt.Fprintf(w, "eas_load_mail_sent_copies_total{result=\"failed\"} %d\n", s.sentFailed)
	fmt.Fprintln(w, "# HELP eas_load_mail_arrival_seconds Time from SendMail until the mail shows up in the Inbox (to self).")
	fmt.Fprintln(w, "# TYPE eas_load_mail_arrival_seconds histogram")
	var cum int64
	for i, le := range mailBounds {
		cum += s.mailArrival.counts[i]
		fmt.Fprintf(w, "eas_load_mail_arrival_seconds_bucket{le=\"%g\"} %d\n", le, cum)
	}
	cum += s.mailArrival.counts[len(mailBounds)]
	fmt.Fprintf(w, "eas_load_mail_arrival_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "eas_load_mail_arrival_seconds_sum %g\n", s.mailArrival.sum)
	fmt.Fprintf(w, "eas_load_mail_arrival_seconds_count %d\n", cum)

	fmt.Fprintln(w, "# HELP eas_load_failures_total Failed steps by error class.")
	fmt.Fprintln(w, "# TYPE eas_load_failures_total counter")
	for _, k := range sortedKeys(s.failures) {
		fmt.Fprintf(w, "eas_load_failures_total{class=%q} %d\n", k, s.failures[k])
	}

	fmt.Fprintln(w, "# HELP eas_load_uptime_seconds Seconds since the worker started.")
	fmt.Fprintln(w, "# TYPE eas_load_uptime_seconds gauge")
	fmt.Fprintf(w, "eas_load_uptime_seconds %d\n", int64(time.Since(s.started).Seconds()))
}

// MissedMail describes a mail that did not come back within the wait time (late or lost), with what its device saw meanwhile.
type MissedMail struct {
	Subject     string   `json:"subject"`
	Device      int      `json:"device"`
	Late        bool     `json:"late"` // it showed up after the wait time
	LateSeconds float64  `json:"late_seconds,omitempty"`
	Polls       int      `json:"syncs"`
	SeenMails   int      `json:"mails_seen"`
	SyncKey     string   `json:"last_sync_key,omitempty"`
	Timeline    []string `json:"timeline,omitempty"`
	// ReceivedAfter is when the server says it received the mail, in seconds after SendMail started (nil: not told).
	ReceivedAfter *float64 `json:"server_received_after_seconds,omitempty"`
}

// missedMailsKept bounds the list of lost mails, so a broken server cannot make the result grow without limit.
const missedMailsKept = 200

// MailMissed counts a mail that did not arrive within the wait time and keeps what is known about it: a late mail
// showed up in the extra wait, a lost one did not show up at all.
func (s *Stats) MailMissed(l MissedMail) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if l.Late {
		s.mailLate++
	} else {
		s.mailLost++
	}
	if len(s.missedMails) < missedMailsKept {
		s.missedMails = append(s.missedMails, l)
	}
}

// maxMissedTimelines is how many lost mails get their timeline in the log; the result file keeps them all.
const maxMissedTimelines = 10

// MissedMailsLogged returns how many lost mails have been recorded so far.
func (s *Stats) MissedMailsLogged() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.missedMails)
}

// MailCleanupFailed counts a mail that came back but could not be deleted again, so it stays in the mailbox, and keeps
// its subject (up to missedMailsKept) so that it can be found and removed by hand.
func (s *Stats) MailCleanupFailed(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mailCleanupFailed++
	if len(s.leftBehind) < missedMailsKept {
		s.leftBehind = append(s.leftBehind, subject)
	}
}

// SentCopies counts the copies in Sent Items that were deleted at the end of a mail job, and those that could not be.
func (s *Stats) SentCopies(deleted, failed int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sentDeleted += int64(deleted)
	s.sentFailed += int64(failed)
}
