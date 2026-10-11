package mailmonitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/logging"
)

type fakeBox struct {
	sent      []string // subjects
	to        []string
	inbox     []Mail // delivered by the next Poll
	deleted   []string
	sentClean []map[string]bool
	sendErr   error
	pollErr   error
	closed    bool
	nextID    int
}

func (b *fakeBox) Send(ctx context.Context, to, subject, body, tag string) error {
	if b.sendErr != nil {
		return b.sendErr
	}
	b.sent = append(b.sent, subject)
	b.to = append(b.to, to)
	return nil
}

func (b *fakeBox) Poll(ctx context.Context) ([]Mail, error) {
	if b.pollErr != nil {
		return nil, b.pollErr
	}
	out := b.inbox
	b.inbox = nil
	return out, nil
}

func (b *fakeBox) Delete(ctx context.Context, id string) error {
	b.deleted = append(b.deleted, id)
	return nil
}

func (b *fakeBox) DeleteSent(ctx context.Context, subjects map[string]bool) (int, int, error) {
	cp := map[string]bool{}
	for k := range subjects {
		cp[k] = true
	}
	b.sentClean = append(b.sentClean, cp)
	return len(subjects), 0, nil
}

func (b *fakeBox) Close() { b.closed = true }

// reply puts an answer to the last mail into the Inbox.
func (b *fakeBox) reply(subjectOf string) string {
	b.nextID++
	id := fmt.Sprintf("m%d", b.nextID)
	b.inbox = append(b.inbox, Mail{ID: id, Subject: "Re: " + subjectOf})
	return id
}

type env struct {
	t   *testing.T
	r   *Runner
	box *fakeBox
	now time.Time
	log bytes.Buffer
}

func newEnv(t *testing.T, targets ...Target) *env {
	e := &env{t: t, box: &fakeBox{}, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	e.r = New(func(ctx context.Context, account string) (Mailbox, error) { return e.box, nil },
		logging.New(&e.log, logging.NewState(logging.LevelTrace), "test", "1"))
	e.r.now = func() time.Time { return e.now }
	m := map[string]Target{}
	for _, tg := range targets {
		m[tg.Name] = tg
	}
	e.r.SetSettings(Settings{Targets: m, Poll: time.Second})
	return e
}

func (e *env) step(advance time.Duration) {
	e.now = e.now.Add(advance)
	e.r.Step(context.Background())
}

func tgt(name string) Target {
	return Target{Name: name, To: "echo@example.org", Account: "acc", Interval: 10 * time.Minute, Timeout: 5 * time.Minute}
}

func TestAnsweredMailIsMeasuredAndCleanedUp(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	if len(e.box.sent) != 1 || e.box.to[0] != "echo@example.org" {
		t.Fatalf("sent %v to %v", e.box.sent, e.box.to)
	}
	if !strings.HasPrefix(e.box.sent[0], "[nshmail-probe] probe-id=echo-") {
		t.Fatalf("subject %q", e.box.sent[0])
	}

	e.step(7 * time.Second)
	e.box.reply(e.box.sent[0])
	e.step(3 * time.Second) // found at 10 s after the send
	s, _ := e.r.Snapshot("echo")
	if s.Sent != 1 || s.Received != 1 || s.Lost != 0 || s.Pending != 0 {
		t.Fatalf("%+v", s)
	}
	if s.LastRTT != 10*time.Second {
		t.Fatalf("round trip %v", s.LastRTT)
	}
	if len(e.box.deleted) != 1 {
		t.Fatalf("answer not deleted: %v", e.box.deleted)
	}

	e.step(cleanEvery)
	if len(e.box.sentClean) != 1 || !e.box.sentClean[0][e.box.sent[0]] {
		t.Fatalf("copy in Sent Items not cleaned: %v", e.box.sentClean)
	}
}

func TestUnansweredMailIsLostOnlyAfterTheTimeout(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	e.step(4 * time.Minute)
	if s, _ := e.r.Snapshot("echo"); s.Lost != 0 || s.Pending != 1 {
		t.Fatalf("too early: %+v", s)
	}
	e.step(61 * time.Second) // 5:01: over, and looked at after that
	s, _ := e.r.Snapshot("echo")
	if s.Lost != 1 || s.Pending != 0 || s.LostInWindow != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestNothingIsLostWhileTheAccountCannotBeRead(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	e.box.pollErr = errors.New("connection refused")
	e.step(6 * time.Minute)
	if s, _ := e.r.Snapshot("echo"); s.Lost != 0 || s.Pending != 1 {
		t.Fatalf("lost although nothing could be looked at: %+v", s)
	}
	if !e.box.closed {
		t.Fatal("the failed session was not closed")
	}
	e.box.pollErr, e.box.closed = nil, false
	e.step(2 * time.Minute) // past the retry pause: open again, look, and now it is known
	if s, _ := e.r.Snapshot("echo"); s.Lost != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestLateAnswerIsCountedAndDeleted(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	first := e.box.sent[0]
	e.step(5*time.Minute + 5*time.Second)
	e.step(2 * time.Second) // lost
	e.box.reply(first)
	e.step(2 * time.Second)
	s, _ := e.r.Snapshot("echo")
	if s.Lost != 1 || s.Late != 1 || s.Received != 0 {
		t.Fatalf("%+v", s)
	}
	if len(e.box.deleted) != 1 {
		t.Fatalf("late answer not deleted: %v", e.box.deleted)
	}
}

func TestAnswerAfterARestartCountsWhenInTime(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	ms := e.now.Add(-20 * time.Second).UnixMilli()
	e.box.inbox = []Mail{{ID: "x1", Subject: "Re: " + Subject("echo", ms)}}
	e.step(0)
	s, _ := e.r.Snapshot("echo")
	if s.Received != 1 || s.LastRTT != 20*time.Second {
		t.Fatalf("%+v", s)
	}
}

func TestOldLeftoverIsDeletedButNotCounted(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	ms := e.now.Add(-400 * time.Minute).UnixMilli()
	e.box.inbox = []Mail{{ID: "old", Subject: "Re: " + Subject("echo", ms)}}
	e.step(0)
	s, _ := e.r.Snapshot("echo")
	if s.Received != 0 || s.Late != 0 || len(e.box.deleted) != 1 {
		t.Fatalf("%+v deleted %v", s, e.box.deleted)
	}
}

func TestOtherMailIsNeverTouched(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.box.inbox = []Mail{{ID: "a", Subject: "Quarterly report"}, {ID: "b", Subject: "Re: [easprobe 1234] self mail"}}
	e.step(0)
	if len(e.box.deleted) != 0 {
		t.Fatalf("deleted %v", e.box.deleted)
	}
}

func TestSecondAnswerIsNotCountedAgain(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	e.step(3 * time.Second)
	e.box.reply(e.box.sent[0])
	e.step(2 * time.Second)
	e.box.reply(e.box.sent[0])
	e.step(2 * time.Second)
	s, _ := e.r.Snapshot("echo")
	if s.Received != 1 || s.Late != 0 || len(e.box.deleted) != 2 {
		t.Fatalf("%+v deleted %v", s, e.box.deleted)
	}
}

func TestMailsAreSentAtTheInterval(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	e.step(9 * time.Minute)
	if len(e.box.sent) != 1 {
		t.Fatalf("sent %d too early", len(e.box.sent))
	}
	e.step(61 * time.Second)
	if len(e.box.sent) != 2 {
		t.Fatalf("sent %d", len(e.box.sent))
	}
}

func TestTargetsOfOneAccountShareTheSession(t *testing.T) {
	a, b := tgt("one"), tgt("two")
	b.To = "other@example.net"
	e := newEnv(t, a, b)
	opened := 0
	e.r.open = func(ctx context.Context, account string) (Mailbox, error) { opened++; return e.box, nil }
	e.step(0)
	if opened != 1 || len(e.box.sent) != 2 {
		t.Fatalf("opened %d, sent %v", opened, e.box.sent)
	}
	e.step(2 * time.Second)
	e.box.reply(e.box.sent[1])
	e.step(2 * time.Second)
	s1, _ := e.r.Snapshot("one")
	s2, _ := e.r.Snapshot("two")
	if s1.Received+s2.Received != 1 {
		t.Fatalf("%+v %+v", s1, s2)
	}
}

func TestFailedOpenIsRetriedAfterThePause(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	calls := 0
	e.r.open = func(ctx context.Context, account string) (Mailbox, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("no route")
		}
		return e.box, nil
	}
	e.step(0)
	e.step(30 * time.Second)
	if calls != 1 {
		t.Fatalf("tried again after %d calls too early", calls)
	}
	e.step(31 * time.Second)
	if calls != 2 || len(e.box.sent) != 1 {
		t.Fatalf("calls %d sent %v", calls, e.box.sent)
	}
}

func TestRemovedTargetLosesItsNumbers(t *testing.T) {
	e := newEnv(t, tgt("echo"), tgt("keep"))
	e.step(0)
	e.r.SetSettings(Settings{Targets: map[string]Target{"keep": tgt("keep")}, Poll: time.Second})
	if _, ok := e.r.Snapshot("echo"); ok {
		t.Fatal("numbers of a removed target are still there")
	}
	if s, ok := e.r.Snapshot("keep"); !ok || s.Sent != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestWindowsAverageTheSingleValues(t *testing.T) {
	var s Stats
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.Received(now.Add(-50*time.Minute), 40*time.Second, "a")
	s.Received(now.Add(-20*time.Minute), 20*time.Second, "b")
	s.Received(now.Add(-10*time.Minute), 10*time.Second, "c")
	s.Received(now.Add(-2*time.Minute), 4*time.Second, "d")
	s.Lost(now.Add(-395*time.Minute), "old", now.Add(-400*time.Minute)) // older than the window: gone
	s.Lost(now.Add(-195*time.Minute), "lost", now.Add(-200*time.Minute))
	got := s.Snapshot(now)

	want := []struct {
		n   int
		avg time.Duration
		max time.Duration
	}{
		{1, 4 * time.Second, 4 * time.Second},
		{2, 7 * time.Second, 10 * time.Second},
		{3, 34 * time.Second / 3, 20 * time.Second},
		{4, 18500 * time.Millisecond, 40 * time.Second},
	}
	for i, w := range want {
		g := got.RTT[i]
		if g.Count != w.n || g.Average != w.avg || g.Max != w.max {
			t.Errorf("window %v: %+v, want %+v", g.Window, g, w)
		}
	}
	if got.LostInWindow != 1 {
		t.Errorf("lost in window %d", got.LostInWindow)
	}

	// an hour later nothing is left in the windows, the counters stay
	later := s.Snapshot(now.Add(61 * time.Minute))
	if later.RTT[3].Count != 0 || later.Received != 4 {
		t.Errorf("%+v", later)
	}
}

func TestPrometheusOutput(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	e.step(3 * time.Second)
	e.box.reply(e.box.sent[0])
	e.step(2 * time.Second)

	var b bytes.Buffer
	e.r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`eas_probe_mail_monitor_sent_total{mail_monitor="echo"} 1`,
		`eas_probe_mail_monitor_received_total{mail_monitor="echo"} 1`,
		`eas_probe_mail_monitor_lost_total{mail_monitor="echo"} 0`,
		`eas_probe_mail_monitor_up{mail_monitor="echo"} 1`,
		`eas_probe_mail_monitor_last_rtt_seconds{mail_monitor="echo"} 5.000`,
		`eas_probe_mail_monitor_rtt_seconds{mail_monitor="echo",window="5m"} 5.000`,
		`eas_probe_mail_monitor_rtt_seconds{mail_monitor="echo",window="60m"} 5.000`,
		`eas_probe_mail_monitor_lost_in_window{mail_monitor="echo",window="300m"} 0`,
		`eas_probe_mail_monitor_sent_in_window{mail_monitor="echo",window="300m"} 1`,
		`eas_probe_mail_monitor_cleanup_failed_total 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// no lines of the same family may be interleaved with others
	var empty bytes.Buffer
	New(nil, nil).WritePrometheus(&empty)
	if empty.Len() != 0 {
		t.Errorf("output without targets: %q", empty.String())
	}
	_ = io.Discard
}

func TestSubjectHasTheTagAndTheParameters(t *testing.T) {
	ms := time.Date(2026, 10, 10, 12, 34, 56, 0, time.UTC).UnixMilli()
	want := fmt.Sprintf("[nshmail-probe] probe-id=partner-1-%d probe-posted=20261010123456", ms)
	if got := Subject("partner-1", ms); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	f := replyTag.FindStringSubmatch("Re: " + want)
	if f == nil || f[1] != "partner-1" || f[2] != fmt.Sprint(ms) {
		t.Fatalf("not read back: %v", f)
	}
}

func TestAccountWithNewSettingsGetsANewSession(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	opened := 0
	e.r.open = func(ctx context.Context, account string) (Mailbox, error) { opened++; return e.box, nil }
	set := func(fp string) {
		e.r.SetSettings(Settings{Targets: map[string]Target{"echo": tgt("echo")}, Poll: time.Second,
			Fingerprints: map[string]string{"acc": fp}})
	}
	set("one")
	e.step(0)
	e.step(2 * time.Second)
	if opened != 1 {
		t.Fatalf("opened %d", opened)
	}
	set("one") // same settings: the session stays
	e.step(2 * time.Second)
	if opened != 1 || e.box.closed {
		t.Fatalf("opened %d closed %v", opened, e.box.closed)
	}
	set("two") // for example a new password
	e.step(2 * time.Second)
	if opened != 2 || !e.box.closed {
		t.Fatalf("opened %d closed %v", opened, e.box.closed)
	}
}

func TestShorterIntervalAppliesAtOnce(t *testing.T) {
	long := tgt("echo")
	long.Interval = time.Hour
	e := newEnv(t, long)
	e.step(0)
	e.step(5 * time.Minute)
	if len(e.box.sent) != 1 {
		t.Fatalf("sent %d", len(e.box.sent))
	}
	short := tgt("echo")
	short.Interval = 2 * time.Minute
	e.r.SetSettings(Settings{Targets: map[string]Target{"echo": short}, Poll: time.Second})
	e.step(time.Second)
	if len(e.box.sent) != 2 {
		t.Fatalf("the new interval was not used: sent %d", len(e.box.sent))
	}
}

func TestSettingsChangesAreLogged(t *testing.T) {
	e := newEnv(t, tgt("keep"), tgt("gone"))
	if strings.Contains(e.log.String(), "entry") {
		t.Fatalf("the start must not log entries:\n%s", e.log.String())
	}
	apply := func(ts ...Target) {
		e.log.Reset()
		m := map[string]Target{}
		for _, x := range ts {
			m[x.Name] = x
		}
		e.r.SetSettings(Settings{Targets: m, Poll: time.Second})
	}

	apply(tgt("keep"), tgt("gone")) // nothing changed
	if e.log.Len() != 0 {
		t.Fatalf("an unchanged configuration logged:\n%s", e.log.String())
	}

	changed := tgt("keep")
	changed.Interval = 2 * time.Minute
	changed.To = "other@example.org"
	added := tgt("new")
	apply(changed, added) // keep changed, gone removed, new added
	out := e.log.String()
	for _, want := range []string{
		`mail monitor: entry changed name=keep to="echo@example.org -> other@example.org" interval="10m0s -> 2m0s"`,
		`mail monitor: entry removed name=gone`,
		`mail monitor: entry added name=new to=echo@example.org account=acc interval=10m0s timeout=5m0s`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// The result of every mail is info (answered), the sending is debug; entries and accounts are info.
func TestSignificantEventsAreInfo(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0) // the account is opened, a mail is sent
	e.step(2 * time.Second)
	e.box.reply(e.box.sent[0])
	e.step(2 * time.Second) // answered
	e.r.SetSettings(Settings{Targets: map[string]Target{"echo": tgt("echo"), "more": tgt("more")}, Poll: time.Second})

	out := e.log.String()
	for _, want := range []string{
		`INFO  mail monitor: account is ready`,
		`INFO  mail monitor: entry added`,
		`DEBUG mail monitor: sent`,
		`INFO  mail monitor: answered`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// A lost mail is counted as lost once. When its answer comes later it becomes a delayed one: it leaves lost_in_window and
// is not part of the averages, while the counters keep the loss.
func TestALostMailThatIsAnsweredLaterBecomesDelayed(t *testing.T) {
	var s Stats
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	sentAt := now.Add(-10 * time.Minute)
	s.Sent(sentAt)
	s.Lost(now.Add(-5*time.Minute), "m1", sentAt)
	s.Received(now.Add(-time.Minute), 8*time.Second, "m2") // another mail, answered in time

	before := s.Snapshot(now)
	if before.Lost != 1 || before.LostInWindow != 1 || before.DelayedInWindow != 0 || before.Late != 0 {
		t.Fatalf("before: %+v", before)
	}

	s.Late(now, 10*time.Minute, "m1")
	after := s.Snapshot(now)
	if after.Lost != 1 || after.Late != 1 {
		t.Errorf("the counters: lost %d late %d, want 1 and 1", after.Lost, after.Late)
	}
	if after.LostInWindow != 0 || after.DelayedInWindow != 1 {
		t.Errorf("windows: lost %d delayed %d, want 0 and 1", after.LostInWindow, after.DelayedInWindow)
	}
	if !after.HasDelayed || after.LastDelayedRTT != 10*time.Minute {
		t.Errorf("last delayed %v %v", after.HasDelayed, after.LastDelayedRTT)
	}
	// the averages stay those of the mails answered in time
	if g := after.RTT[0]; g.Count != 1 || g.Average != 8*time.Second {
		t.Errorf("the averages changed: %+v", g)
	}
	if after.SentInWindow != 1 {
		t.Errorf("sent in window %d", after.SentInWindow)
	}

	// a second answer of the same mail changes nothing in the records
	s.Late(now, 11*time.Minute, "m1")
	if again := s.Snapshot(now); again.DelayedInWindow != 1 || again.LostInWindow != 0 {
		t.Errorf("a second answer: %+v", again)
	}

	// after the window the records are gone, the counters stay
	later := s.Snapshot(now.Add(LostWindow + time.Minute))
	if later.DelayedInWindow != 0 || later.Lost != 1 || later.Late != 2 {
		t.Errorf("later: %+v", later)
	}
}

func TestDelayedMailInTheWholeChain(t *testing.T) {
	e := newEnv(t, tgt("echo"))
	e.step(0)
	first := e.box.sent[0]
	e.step(5*time.Minute + 5*time.Second)
	e.step(2 * time.Second) // lost
	if s, _ := e.r.Snapshot("echo"); s.Lost != 1 || s.LostInWindow != 1 {
		t.Fatalf("not lost: %+v", s)
	}
	e.box.reply(first)
	e.step(2 * time.Second)
	s, _ := e.r.Snapshot("echo")
	if s.Lost != 1 || s.Late != 1 || s.LostInWindow != 0 || s.DelayedInWindow != 1 {
		t.Fatalf("after the late answer: %+v", s)
	}

	var b bytes.Buffer
	e.r.WritePrometheus(&b)
	for _, want := range []string{
		`eas_probe_mail_monitor_lost_total{mail_monitor="echo"} 1`,
		`eas_probe_mail_monitor_late_total{mail_monitor="echo"} 1`,
		`eas_probe_mail_monitor_lost_in_window{mail_monitor="echo",window="300m"} 0`,
		`eas_probe_mail_monitor_delayed_in_window{mail_monitor="echo",window="300m"} 1`,
		`eas_probe_mail_monitor_last_delayed_rtt_seconds{mail_monitor="echo"} `,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
	d := e.r.Debug().Targets["echo"]
	if len(d.Problems) != 1 || d.Problems[0].State != "delayed" || d.Problems[0].RTTSeconds == nil || d.Problems[0].LostTime == nil {
		t.Errorf("debug problems: %+v", d.Problems)
	}
}
