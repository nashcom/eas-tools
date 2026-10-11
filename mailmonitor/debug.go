package mailmonitor

import (
	"sort"
	"strconv"
	"time"
)

// Debug is what the monitor knows right now, for the debug request of the exporter: the numbers and the single values
// that the averages are made of, the state of each account and the mails that wait for an answer.
type Debug struct {
	Targets  map[string]TargetDebug  `json:"targets"`
	Accounts map[string]AccountDebug `json:"accounts"`
}

type TargetDebug struct {
	To             string  `json:"to"`
	Account        string  `json:"account"`
	IntervalSecond float64 `json:"interval_seconds"`
	TimeoutSecond  float64 `json:"timeout_seconds"`
	Up             bool    `json:"up"`
	NextSend       *string `json:"next_send,omitempty"`

	Sent       int64 `json:"sent_total"`
	Received   int64 `json:"received_total"`
	Lost       int64 `json:"lost_total"`
	Late       int64 `json:"late_total"`
	SendErrors int64 `json:"send_errors_total"`
	Pending    int   `json:"pending"`

	LastRTTSeconds *float64 `json:"last_rtt_seconds,omitempty"`
	LastAnswer     *string  `json:"last_answer,omitempty"`
	// Windows are the averages that /metrics reports, calculated from Samples.
	Windows []WindowDebug `json:"windows"`
	// Samples are the answered mails of the last hour, the single values that the averages are made of.
	Samples []SampleDebug `json:"samples"`
	// SentInWindow, LostInWindow and DelayedInWindow are calculated from the records of the last 300 minutes.
	SentInWindow    int `json:"sent_in_300m"`
	LostInWindow    int `json:"lost_in_300m"`
	DelayedInWindow int `json:"delayed_in_300m"`
	// Problems lists the mails of the last 300 minutes that were not answered in time: lost ones, and delayed ones that
	// were lost and answered after all (with their round trip). A delayed mail is not part of the averages.
	Problems []ProblemDebug `json:"problems"`
}

// ProblemDebug is a mail that was not answered in time: lost, or delayed (lost, and answered after all). It has the same
// terms as a sample: sent and received, each as Unix nanoseconds and as a readable time.
type ProblemDebug struct {
	Tag              string   `json:"tag"`
	State            string   `json:"state"`
	SentUnixNano     string   `json:"sent_unix_nano"`
	SentTime         string   `json:"sent_time"`
	LostTime         *string  `json:"lost_time,omitempty"`
	ReceivedUnixNano string   `json:"received_unix_nano,omitempty"`
	ReceivedTime     *string  `json:"received_time,omitempty"`
	RTTSeconds       *float64 `json:"rtt_seconds,omitempty"`
}

type WindowDebug struct {
	Window         string  `json:"window"`
	Count          int     `json:"count"`
	AverageSeconds float64 `json:"average_seconds"`
	MaxSeconds     float64 `json:"max_seconds"`
}

// SampleDebug is one answered mail: when it was sent and when it was received, each as Unix nanoseconds (strings, like
// time_unix_nano of the log) and as a readable time, and the round trip measured between the two.
type SampleDebug struct {
	SentUnixNano     string  `json:"sent_unix_nano"`
	ReceivedUnixNano string  `json:"received_unix_nano"`
	SentTime         string  `json:"sent_time"`
	ReceivedTime     string  `json:"received_time"`
	RTTSeconds       float64 `json:"rtt_seconds"`
}

type AccountDebug struct {
	Open      bool           `json:"open"`
	RetryAt   *string        `json:"retry_at,omitempty"`
	LastError string         `json:"last_error,omitempty"`
	LastPoll  *string        `json:"last_poll,omitempty"`
	Pending   []PendingDebug `json:"pending"`
	// CleanupQueue is the number of copies in Sent Items that are still to be deleted.
	CleanupQueue int `json:"cleanup_queue"`
}

type PendingDebug struct {
	Tag        string  `json:"tag"`
	Target     string  `json:"target"`
	AgeSeconds float64 `json:"age_seconds"`
}

// nanoTime formats a time in UTC with all the digits it has (nanoseconds, where the clock has them); secondsOf gives a
// duration in seconds, unrounded.
func nanoTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// unixNano is a time as nanoseconds since 1970, in a string, the way OpenTelemetry writes time_unix_nano.
func unixNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func secondsOf(d time.Duration) float64 { return d.Seconds() }

func stamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := nanoTime(t)
	return &s
}

// publish copies the state of an account for Debug. Only the goroutine of Run touches the account itself; the copy is
// what other goroutines may read.
func (r *Runner) publish(name string, a *account, targets []string) {
	info := AccountDebug{Open: a.mb != nil, LastError: a.lastErr, Pending: []PendingDebug{}, CleanupQueue: len(a.clean)}
	if a.mb == nil {
		info.RetryAt = stamp(a.retryAt)
	}
	info.LastPoll = stamp(a.lastPoll)
	now := r.now()
	for tag, p := range a.pending {
		info.Pending = append(info.Pending, PendingDebug{Tag: tag, Target: p.target, AgeSeconds: secondsOf(now.Sub(p.at))})
	}
	sort.Slice(info.Pending, func(i, j int) bool { return info.Pending[i].Tag < info.Pending[j].Tag })

	next := map[string]string{}
	if s := r.set.Load(); s != nil {
		for _, tn := range targets {
			if last, ok := r.last[tn]; ok {
				next[tn] = nanoTime(last.Add(s.Targets[tn].Interval))
			}
		}
	}

	r.mu.Lock()
	if r.pub == nil {
		r.pub = map[string]AccountDebug{}
		r.pubNext = map[string]string{}
	}
	r.pub[name] = info
	for tn, v := range next {
		r.pubNext[tn] = v
	}
	r.mu.Unlock()
}

// Debug returns the current state. It is safe to call while the monitor runs.
func (r *Runner) Debug() Debug {
	out := Debug{Targets: map[string]TargetDebug{}, Accounts: map[string]AccountDebug{}}
	s := r.set.Load()
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, info := range r.pub {
		out.Accounts[name] = info
	}
	if s == nil {
		return out
	}
	for name, t := range s.Targets {
		st := r.stats[name]
		if st == nil {
			continue
		}
		snap, records := st.Detail(now)
		d := TargetDebug{
			To: t.To, Account: t.Account, IntervalSecond: secondsOf(t.Interval), TimeoutSecond: secondsOf(t.Timeout),
			Up: r.up[name], Sent: snap.Sent, Received: snap.Received, Lost: snap.Lost, Late: snap.Late,
			SendErrors: snap.SendErrors, Pending: snap.Pending, LastAnswer: stamp(snap.LastAt), SentInWindow: snap.SentInWindow, LostInWindow: snap.LostInWindow, DelayedInWindow: snap.DelayedInWindow,
			Samples: []SampleDebug{}, Problems: []ProblemDebug{}, Windows: []WindowDebug{},
		}
		if v, ok := r.pubNext[name]; ok {
			d.NextSend = &v
		}
		if !snap.LastAt.IsZero() {
			v := secondsOf(snap.LastRTT)
			d.LastRTTSeconds = &v
		}
		for _, w := range snap.RTT {
			d.Windows = append(d.Windows, WindowDebug{Window: windowLabel(w.Window), Count: w.Count,
				AverageSeconds: secondsOf(w.Average), MaxSeconds: secondsOf(w.Max)})
		}
		for _, rec := range records {
			switch rec.State {
			case Answered:
				if !rec.AnsweredAt.Before(now.Add(-RTTWindows[len(RTTWindows)-1])) {
					d.Samples = append(d.Samples, SampleDebug{SentUnixNano: unixNano(rec.SentAt), ReceivedUnixNano: unixNano(rec.AnsweredAt), SentTime: nanoTime(rec.SentAt), ReceivedTime: nanoTime(rec.AnsweredAt), RTTSeconds: secondsOf(rec.RTT)})
				}
			default:
				p := ProblemDebug{Tag: rec.Tag, State: rec.State.String(), SentUnixNano: unixNano(rec.SentAt), SentTime: nanoTime(rec.SentAt), LostTime: stamp(rec.LostAt)}
				if rec.State == Delayed {
					p.ReceivedUnixNano = unixNano(rec.AnsweredAt)
					p.ReceivedTime = stamp(rec.AnsweredAt)
					v := secondsOf(rec.RTT)
					p.RTTSeconds = &v
				}
				d.Problems = append(d.Problems, p)
			}
		}
		out.Targets[name] = d
	}
	return out
}
