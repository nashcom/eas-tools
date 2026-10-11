package load

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Result is the outcome of a run as one JSON document: what was run, how it
// ended and the numbers per ActiveSync command. It holds no credentials.
type Result struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Name    string `json:"name"`
	Server  string `json:"server"`
	Mode    string `json:"mode"` // standalone | coordinator

	Job      string `json:"job,omitempty"`
	Params   string `json:"params,omitempty"`
	Profile  string `json:"profile"`
	Clients  int    `json:"clients"`
	Devices  int    `json:"devices_per_client"`
	Started  string `json:"started"`
	Finished string `json:"finished"`
	Seconds  int    `json:"seconds"`

	// Outcome as the exit code; Errors is the number of failed requests.
	Outcome string     `json:"outcome"`
	Jobs    JobsResult `json:"jobs"`
	Errors  int64      `json:"request_errors"`

	Requests []OpResult       `json:"requests"`
	Failures map[string]int64 `json:"failed_steps,omitempty"`
	Mail     *MailResult      `json:"mail,omitempty"`
	Items    map[string]int64 `json:"items,omitempty"`
	Bytes    map[string]int64 `json:"bytes"`
}

// JobsResult counts how the jobs of the clients ended.
type JobsResult struct {
	OK       int `json:"ok"`
	Stopped  int `json:"stopped"`
	Failed   int `json:"failed"`
	NoResult int `json:"no_result"`
}

// OpResult is one ActiveSync command (or HTTP method).
type OpResult struct {
	Op     string           `json:"op"`
	Count  int64            `json:"count"`
	Errors int64            `json:"errors"`
	MeanS  float64          `json:"mean_seconds"`
	P50S   float64          `json:"p50_seconds"`
	P95S   float64          `json:"p95_seconds"`
	MaxS   float64          `json:"max_seconds"`
	Status map[string]int64 `json:"http_status"`
}

// MailResult is the mail round trip count of the eas-mail job.
type MailResult struct {
	Sent              int64        `json:"sent"`
	Arrived           int64        `json:"arrived"`
	Lost              int64        `json:"lost"`
	Late              int64        `json:"late"`
	CleanupFailed     int64        `json:"cleanup_failed"`
	SentCopiesDeleted int64        `json:"sent_copies_deleted"`
	SentCopiesFailed  int64        `json:"sent_copies_failed"`
	LeftBehind        []string     `json:"left_behind,omitempty"`
	NotOnTime         []MissedMail `json:"not_on_time,omitempty"`
	MeanS             float64      `json:"mean_arrival_seconds"`
}

// Snapshot fills the numbers of the result from the statistics.
func (s *Stats) Snapshot(r *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r.Requests = nil
	for _, op := range sortedKeys(s.ops) {
		o := s.ops[op]
		n := o.ok + o.errs
		x := OpResult{Op: op, Count: n, Errors: o.errs, P50S: round3(o.quantile(0.5)), P95S: round3(o.quantile(0.95)),
			MaxS: round3(o.max), Status: map[string]int64{}}
		if n > 0 {
			x.MeanS = round3(o.sum / float64(n))
		}
		for c, k := range o.status {
			x.Status[fmt.Sprint(c)] = k
		}
		r.Requests = append(r.Requests, x)
	}
	if len(s.failures) > 0 {
		r.Failures = map[string]int64{}
		for k, v := range s.failures {
			r.Failures[k] = v
		}
	}
	if len(s.mailSent) > 0 {
		m := &MailResult{Arrived: s.mailArrival.n, Lost: s.mailLost, Late: s.mailLate, CleanupFailed: s.mailCleanupFailed, SentCopiesDeleted: s.sentDeleted, SentCopiesFailed: s.sentFailed, LeftBehind: append([]string(nil), s.leftBehind...), NotOnTime: append([]MissedMail(nil), s.missedMails...)}
		for _, n := range s.mailSent {
			m.Sent += n
		}
		if s.mailArrival.n > 0 {
			m.MeanS = round3(s.mailArrival.sum / float64(s.mailArrival.n))
		}
		r.Mail = m
	}
	if len(s.items) > 0 {
		r.Items = map[string]int64{}
		for k, v := range s.items {
			r.Items[k] = v
		}
	}
	r.Bytes = map[string]int64{"sent": s.sent, "received": s.recv}
}

// SetOutcome decides the verdict the same way the exit code does: failed when a
// job failed or a client never reported, ok otherwise. A stop by duration or
// by Ctrl-C is not a failure. Errors of single requests are counted in Errors.
func (r *Result) SetOutcome() {
	r.Outcome = "ok"
	if r.Jobs.Failed > 0 || r.Jobs.NoResult > 0 {
		r.Outcome = "failed"
	}
	r.Errors = 0
	for _, o := range r.Requests {
		r.Errors += o.Errors
	}
}

// SetTimes records the start and the end of the run.
func (r *Result) SetTimes(start, end time.Time) {
	r.Started = start.UTC().Format(time.RFC3339)
	r.Finished = end.UTC().Format(time.RFC3339)
	r.Seconds = int(end.Sub(start).Round(time.Second) / time.Second)
}

// Write stores the result as indented JSON. The file is written to a temporary
// file in the same folder first, so a reader never sees half a document.
func (r *Result) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".result-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := firstErr(werr, cerr); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil && !strings.Contains(err.Error(), "not supported") {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}
