package mailmonitor

import (
	"sync"
	"time"
)

// RTTWindows are the periods the round-trip time is averaged over. The numbers are calculated from the records of the
// mails when they are asked for, so a record that changes its state changes every window that contains it.
var RTTWindows = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute}

// LostWindow is how long the records are kept: how many mails were sent and how many were not answered, lately. A lost
// mail is rare, so a longer period than the ones of the round-trip time says more.
const LostWindow = 300 * time.Minute

// State is where a mail is on its way.
type State int

const (
	Answered State = iota // the answer came within the wait time
	Lost                  // no answer within the wait time
	Delayed               // it was lost, and the answer came after all
)

func (s State) String() string {
	switch s {
	case Lost:
		return "lost"
	case Delayed:
		return "delayed"
	}
	return "answered"
}

// Record is one mail that was sent and has an outcome (a mail that still waits has none yet).
type Record struct {
	Tag        string
	SentAt     time.Time
	State      State
	AnsweredAt time.Time     // when the answer came: Answered and Delayed
	LostAt     time.Time     // when it was given up: Lost and Delayed
	RTT        time.Duration // Answered and Delayed
}

// Stats are the numbers of one mail monitor entry.
type Stats struct {
	mu sync.Mutex

	// the counters only go up, as Prometheus wants: a lost mail that is answered later stays counted as lost, and late counts it
	sent, received, lost, late, sendErrors int64

	lastRTT time.Duration
	lastAt  time.Time // when the last answer came in time; zero = none yet
	// records are the mails of the last LostWindow, in the order they were sent (a mail that waits is not in them)
	records []*Record
	sentAt  []time.Time
	pending int
}

// WindowStats are the numbers for one period.
type WindowStats struct {
	Window  time.Duration
	Count   int
	Average time.Duration
	Max     time.Duration
}

// Snapshot is a consistent copy of the numbers.
type Snapshot struct {
	Sent, Received, Lost, Late, SendErrors int64
	Pending                                int
	LastRTT                                time.Duration
	LastAt                                 time.Time
	RTT                                    []WindowStats // one per RTTWindows: the mails that were answered in time
	// within LostWindow: the mails sent, those that are lost now, and those that were lost and answered later
	SentInWindow, LostInWindow, DelayedInWindow int
	LastDelayedRTT                              time.Duration
	HasDelayed                                  bool
}

func (s *Stats) Sent(now time.Time) {
	s.mu.Lock()
	s.sent++
	s.sentAt = append(s.sentAt, now)
	s.mu.Unlock()
}

// find returns the record of a mail, or nil.
func (s *Stats) find(tag string) *Record {
	for _, r := range s.records {
		if r.Tag == tag {
			return r
		}
	}
	return nil
}

// Received records an answer within the wait time. A mail that this process did not send (it was sent before a restart) gets
// its record now.
func (s *Stats) Received(now time.Time, rtt time.Duration, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received++
	s.lastRTT, s.lastAt = rtt, now
	if r := s.find(tag); r != nil {
		r.State, r.AnsweredAt, r.RTT = Answered, now, rtt
		return
	}
	s.records = append(s.records, &Record{Tag: tag, SentAt: now.Add(-rtt), State: Answered, AnsweredAt: now, RTT: rtt})
}

// Lost records a mail that was not answered in time.
func (s *Stats) Lost(now time.Time, tag string, sentAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost++
	s.records = append(s.records, &Record{Tag: tag, SentAt: sentAt, State: Lost, LostAt: now})
}

// Late records the answer of a mail that came after the wait time. When the mail is still a lost one in the records it
// becomes a delayed one: it is not lost any more as far as the windows go, and it is not part of the averages. The
// counters keep the loss.
func (s *Stats) Late(now time.Time, rtt time.Duration, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.late++
	if r := s.find(tag); r != nil && r.State == Lost {
		r.State, r.AnsweredAt, r.RTT = Delayed, now, rtt
		return
	}
	if s.find(tag) == nil {
		s.records = append(s.records, &Record{Tag: tag, SentAt: now.Add(-rtt), State: Delayed, AnsweredAt: now, LostAt: now, RTT: rtt})
	}
}

func (s *Stats) SendError() {
	s.mu.Lock()
	s.sendErrors++
	s.mu.Unlock()
}

func (s *Stats) SetPending(n int) {
	s.mu.Lock()
	s.pending = n
	s.mu.Unlock()
}

// prune drops what is older than LostWindow; the caller holds the lock.
func (s *Stats) prune(now time.Time) {
	from := now.Add(-LostWindow)
	kept := s.records[:0:0]
	for _, r := range s.records {
		if !r.SentAt.Before(from) {
			kept = append(kept, r)
		}
	}
	s.records = kept
	i := 0
	for i < len(s.sentAt) && s.sentAt[i].Before(from) {
		i++
	}
	s.sentAt = append(s.sentAt[:0:0], s.sentAt[i:]...)
}

// Snapshot calculates the numbers at the given time from the records.
func (s *Stats) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(now)
}

func (s *Stats) snapshot(now time.Time) Snapshot {
	s.prune(now)
	out := Snapshot{
		Sent: s.sent, Received: s.received, Lost: s.lost, Late: s.late, SendErrors: s.sendErrors,
		Pending: s.pending, LastRTT: s.lastRTT, LastAt: s.lastAt, SentInWindow: len(s.sentAt),
	}
	var lastDelayed time.Time
	for _, r := range s.records {
		switch r.State {
		case Lost:
			out.LostInWindow++
		case Delayed:
			out.DelayedInWindow++
			if r.AnsweredAt.After(lastDelayed) {
				lastDelayed, out.LastDelayedRTT, out.HasDelayed = r.AnsweredAt, r.RTT, true
			}
		}
	}
	for _, w := range RTTWindows {
		ws := WindowStats{Window: w}
		var sum time.Duration
		for _, r := range s.records {
			if r.State != Answered || r.AnsweredAt.Before(now.Add(-w)) {
				continue
			}
			ws.Count++
			sum += r.RTT
			if r.RTT > ws.Max {
				ws.Max = r.RTT
			}
		}
		if ws.Count > 0 {
			ws.Average = sum / time.Duration(ws.Count)
		}
		out.RTT = append(out.RTT, ws)
	}
	return out
}

// Detail returns the numbers together with the records they are calculated from.
func (s *Stats) Detail(now time.Time) (Snapshot, []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snapshot(now)
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	return snap, out
}
