package probe

import (
	"fmt"
	"io"
	"strings"
)

// WritePrometheus renders the result in the Prometheus text exposition
// format, shaped like the blackbox exporter: one probe_success plus
// per-step gauges. The same output can be dropped into a node_exporter
// textfile collector directory or served from a /probe handler later.
func WritePrometheus(w io.Writer, r *Result) {
	b2f := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}

	fmt.Fprintln(w, "# HELP eas_probe_success 1 if every ActiveSync step succeeded.")
	fmt.Fprintln(w, "# TYPE eas_probe_success gauge")
	fmt.Fprintf(w, "eas_probe_success %d\n", b2f(r.OK))

	fmt.Fprintln(w, "# HELP eas_probe_duration_seconds Total probe duration.")
	fmt.Fprintln(w, "# TYPE eas_probe_duration_seconds gauge")
	fmt.Fprintf(w, "eas_probe_duration_seconds %g\n", r.Duration.Seconds())

	fmt.Fprintln(w, "# HELP eas_probe_step_success 1 if the step succeeded (skipped steps are omitted).")
	fmt.Fprintln(w, "# TYPE eas_probe_step_success gauge")
	for _, s := range r.Steps {
		if s.Skipped && !s.OK {
			continue
		}
		fmt.Fprintf(w, "eas_probe_step_success{step=%q} %d\n", s.Name, b2f(s.OK))
	}

	fmt.Fprintln(w, "# HELP eas_probe_step_duration_seconds Duration of each step.")
	fmt.Fprintln(w, "# TYPE eas_probe_step_duration_seconds gauge")
	for _, s := range r.Steps {
		if s.Skipped {
			continue
		}
		fmt.Fprintf(w, "eas_probe_step_duration_seconds{step=%q} %g\n", s.Name, s.Duration.Seconds())
	}

	fmt.Fprintln(w, "# HELP eas_probe_step_http_status HTTP status of the last request of the step.")
	fmt.Fprintln(w, "# TYPE eas_probe_step_http_status gauge")
	for _, s := range r.Steps {
		if s.HTTPStatus != 0 {
			fmt.Fprintf(w, "eas_probe_step_http_status{step=%q} %d\n", s.Name, s.HTTPStatus)
		}
	}

	fmt.Fprintln(w, "# HELP eas_probe_failure 1 for the step and error class that failed.")
	fmt.Fprintln(w, "# TYPE eas_probe_failure gauge")
	for _, s := range r.Steps {
		if !s.OK && !s.Skipped {
			fmt.Fprintf(w, "eas_probe_failure{step=%q,class=%q} 1\n", s.Name, s.ErrorClass)
		}
	}

	fmt.Fprintln(w, "# HELP eas_probe_http_phase_seconds Connection phases of the first request of the run.")
	fmt.Fprintln(w, "# TYPE eas_probe_http_phase_seconds gauge")
	for _, x := range r.Exchanges {
		if x.Connect > 0 || x.TLS > 0 || x.DNS > 0 {
			fmt.Fprintf(w, "eas_probe_http_phase_seconds{phase=\"dns\"} %g\n", x.DNS.Seconds())
			fmt.Fprintf(w, "eas_probe_http_phase_seconds{phase=\"connect\"} %g\n", x.Connect.Seconds())
			fmt.Fprintf(w, "eas_probe_http_phase_seconds{phase=\"tls\"} %g\n", x.TLS.Seconds())
			break
		}
	}

	var reqBytes, respBytes int64
	for _, x := range r.Exchanges {
		reqBytes += x.ReqBytes
		respBytes += x.RespBytes
	}
	fmt.Fprintln(w, "# HELP eas_probe_http_requests Number of HTTP requests of the run.")
	fmt.Fprintln(w, "# TYPE eas_probe_http_requests gauge")
	fmt.Fprintf(w, "eas_probe_http_requests %d\n", len(r.Exchanges))
	fmt.Fprintln(w, "# HELP eas_probe_http_bytes Request and response payload bytes of the run.")
	fmt.Fprintln(w, "# TYPE eas_probe_http_bytes gauge")
	fmt.Fprintf(w, "eas_probe_http_bytes{direction=\"sent\"} %d\n", reqBytes)
	fmt.Fprintf(w, "eas_probe_http_bytes{direction=\"received\"} %d\n", respBytes)

	fmt.Fprintln(w, "# HELP eas_probe_info Static facts detected on the server.")
	fmt.Fprintln(w, "# TYPE eas_probe_info gauge")
	fmt.Fprintf(w, "eas_probe_info{protocol_version=%q,product=%q,tls=%q,policy=%q} 1\n",
		r.Server.ProtocolVersion, r.Server.Product, r.Server.TLSVersion, r.Server.Policy)

	if f := r.Step(StepFolderSync); f != nil && f.OK {
		fmt.Fprintln(w, "# HELP eas_probe_folders Folders in the account hierarchy.")
		fmt.Fprintln(w, "# TYPE eas_probe_folders gauge")
		fmt.Fprintf(w, "eas_probe_folders %s\n", num(f.Info["folders"]))
	}
	if d := r.Step(StepDirectory); d != nil && !d.Skipped && d.Info != nil {
		fmt.Fprintln(w, "# HELP eas_probe_directory_results Entries the directory search for the own user returned.")
		fmt.Fprintln(w, "# TYPE eas_probe_directory_results gauge")
		fmt.Fprintf(w, "eas_probe_directory_results %s\n", num(d.Info["results"]))
		if v, ok := d.Info["self_found"]; ok {
			found := 0
			if v == "true" {
				found = 1
			}
			fmt.Fprintln(w, "# HELP eas_probe_directory_self_found 1 if the own user was found in the directory.")
			fmt.Fprintln(w, "# TYPE eas_probe_directory_self_found gauge")
			fmt.Fprintf(w, "eas_probe_directory_self_found %d\n", found)
		}
	}
	if m := r.Step(StepMail); m != nil && !m.Skipped {
		fmt.Fprintln(w, "# HELP eas_probe_mail_roundtrip_seconds Time from SendMail until the mail was in the Inbox (to self).")
		fmt.Fprintln(w, "# TYPE eas_probe_mail_roundtrip_seconds gauge")
		if m.OK && m.Info["roundtrip_seconds"] != "" {
			fmt.Fprintf(w, "eas_probe_mail_roundtrip_seconds %s\n", m.Info["roundtrip_seconds"])
		}
		lost := 0
		if m.ErrorClass == "mail_lost" {
			lost = 1
		}
		if push := m.Info["push"]; push != "" {
			wake := 0
			if push == "woke" {
				wake = 1
			}
			fmt.Fprintln(w, "# HELP eas_probe_mail_push_wake Only with arrival=push (experimental): 1 if the Ping that was restarted after the mail was sent ended with changes. Not a reliable push test with one device.")
			fmt.Fprintln(w, "# TYPE eas_probe_mail_push_wake gauge")
			fmt.Fprintf(w, "eas_probe_mail_push_wake %d\n", wake)
			if m.Info["wake_seconds"] != "" {
				fmt.Fprintln(w, "# HELP eas_probe_mail_wake_seconds Time from SendMail until the pending Ping ended with changes.")
				fmt.Fprintln(w, "# TYPE eas_probe_mail_wake_seconds gauge")
				fmt.Fprintf(w, "eas_probe_mail_wake_seconds %s\n", m.Info["wake_seconds"])
			}
		}
		if d := m.Info["deleted"]; d != "" {
			failed := 0
			if d != "true" {
				failed = 1
			}
			fmt.Fprintln(w, "# HELP eas_probe_mail_cleanup_failed 1 if the test mail could not be deleted again (it stays in the Inbox; its subject is in the probe report).")
			fmt.Fprintln(w, "# TYPE eas_probe_mail_cleanup_failed gauge")
			fmt.Fprintf(w, "eas_probe_mail_cleanup_failed %d\n", failed)
		}
		fmt.Fprintln(w, "# HELP eas_probe_mail_lost 1 if the mail did not come back within the wait time.")
		fmt.Fprintln(w, "# TYPE eas_probe_mail_lost gauge")
		fmt.Fprintf(w, "eas_probe_mail_lost %d\n", lost)
	}
	if p := r.Step(StepPing); p != nil && p.OK {
		fmt.Fprintln(w, "# HELP eas_probe_ping_held_seconds How long the server held the Ping request.")
		fmt.Fprintln(w, "# TYPE eas_probe_ping_held_seconds gauge")
		fmt.Fprintf(w, "eas_probe_ping_held_seconds %s\n", num(p.Info["held_seconds"]))
	}
}

func num(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}
