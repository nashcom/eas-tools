package mailmonitor

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

func windowLabel(d time.Duration) string { return strconv.Itoa(int(d.Minutes())) + "m" }

// WritePrometheus writes the metrics of all targets, labelled mail_monitor="NAME". It writes nothing when no target is
// configured.
func (r *Runner) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	names := make([]string, 0, len(r.stats))
	snap := map[string]Snapshot{}
	up := map[string]bool{}
	now := r.now()
	for n, st := range r.stats {
		names = append(names, n)
		snap[n] = st.Snapshot(now)
		up[n] = r.up[n]
	}
	r.mu.Unlock()
	if len(names) == 0 {
		return
	}
	sort.Strings(names)

	family := func(name, kind, help string, each func(n string, s Snapshot, emit func(labels string, v any))) {
		fmt.Fprintf(w, "# HELP eas_probe_mail_monitor_%s %s\n# TYPE eas_probe_mail_monitor_%s %s\n", name, help, name, kind)
		for _, n := range names {
			each(n, snap[n], func(labels string, v any) {
				if labels != "" {
					labels = "," + labels
				}
				fmt.Fprintf(w, "eas_probe_mail_monitor_%s{mail_monitor=%q%s} %v\n", name, n, labels, v)
			})
		}
	}
	secs := func(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 3, 64) }

	family("sent_total", "counter", "Mails sent to the remote address.", func(n string, s Snapshot, e func(string, any)) { e("", s.Sent) })
	family("received_total", "counter", "Mails that were answered in time.", func(n string, s Snapshot, e func(string, any)) { e("", s.Received) })
	family("lost_total", "counter", "Mails that were not answered in time.", func(n string, s Snapshot, e func(string, any)) { e("", s.Lost) })
	family("late_total", "counter", "Answers that came after the wait time (the mail was counted as lost).", func(n string, s Snapshot, e func(string, any)) { e("", s.Late) })
	family("send_errors_total", "counter", "Mails that could not be sent.", func(n string, s Snapshot, e func(string, any)) { e("", s.SendErrors) })
	family("pending", "gauge", "Mails that wait for an answer.", func(n string, s Snapshot, e func(string, any)) { e("", s.Pending) })
	family("up", "gauge", "1 when the account of the target can be used.", func(n string, s Snapshot, e func(string, any)) {
		v := 0
		if up[n] {
			v = 1
		}
		e("", v)
	})
	family("last_rtt_seconds", "gauge", "Round-trip time of the last answered mail (absent until there was one).", func(n string, s Snapshot, e func(string, any)) {
		if !s.LastAt.IsZero() {
			e("", secs(s.LastRTT))
		}
	})
	family("last_answer_timestamp_seconds", "gauge", "Unix time of the last answer (absent until there was one).", func(n string, s Snapshot, e func(string, any)) {
		if !s.LastAt.IsZero() {
			e("", s.LastAt.Unix())
		}
	})
	family("rtt_seconds", "gauge", "Average round-trip time of the mails answered in the window (absent when there were none).", func(n string, s Snapshot, e func(string, any)) {
		for _, ws := range s.RTT {
			if ws.Count > 0 {
				e(fmt.Sprintf("window=%q", windowLabel(ws.Window)), secs(ws.Average))
			}
		}
	})
	family("rtt_max_seconds", "gauge", "Longest round-trip time of the mails answered in the window (absent when there were none).", func(n string, s Snapshot, e func(string, any)) {
		for _, ws := range s.RTT {
			if ws.Count > 0 {
				e(fmt.Sprintf("window=%q", windowLabel(ws.Window)), secs(ws.Max))
			}
		}
	})
	family("rtt_samples", "gauge", "Number of answered mails in the window.", func(n string, s Snapshot, e func(string, any)) {
		for _, ws := range s.RTT {
			e(fmt.Sprintf("window=%q", windowLabel(ws.Window)), ws.Count)
		}
	})
	family("sent_in_window", "gauge", "Mails sent in the window of the lost mails.", func(n string, s Snapshot, e func(string, any)) {
		e(fmt.Sprintf("window=%q", windowLabel(LostWindow)), s.SentInWindow)
	})
	family("lost_in_window", "gauge", "Mails that are lost now, in the window of the lost mails: one that was answered later is not counted any more.", func(n string, s Snapshot, e func(string, any)) {
		e(fmt.Sprintf("window=%q", windowLabel(LostWindow)), s.LostInWindow)
	})
	family("delayed_in_window", "gauge", "Mails that were lost and answered after all, in the window of the lost mails.", func(n string, s Snapshot, e func(string, any)) {
		e(fmt.Sprintf("window=%q", windowLabel(LostWindow)), s.DelayedInWindow)
	})
	family("last_delayed_rtt_seconds", "gauge", "Round-trip time of the last delayed mail in the window of the lost mails (absent when there is none).", func(n string, s Snapshot, e func(string, any)) {
		if s.HasDelayed {
			e("", secs(s.LastDelayedRTT))
		}
	})

	fmt.Fprintf(w, "# HELP eas_probe_mail_monitor_cleanup_failed_total Mails of the probe that could not be deleted again.\n# TYPE eas_probe_mail_monitor_cleanup_failed_total counter\neas_probe_mail_monitor_cleanup_failed_total %d\n", r.cleanupFailed.Load())
}
