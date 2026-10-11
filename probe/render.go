package probe

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// WriteText prints a human readable report.
func WriteText(w io.Writer, r *Result) {
	verdict := "FAILED"
	if r.OK {
		verdict = "OK"
	}
	fmt.Fprintf(w, "eas-probe  %s  user=%s  total=%s\n", verdict, r.User, round(r.Duration))
	fmt.Fprintf(w, "  server   : %s\n", r.Server.URL)
	if r.Server.Product != "" {
		fmt.Fprintf(w, "  product  : %s\n", r.Server.Product)
	}
	if len(r.Server.Versions) > 0 {
		fmt.Fprintf(w, "  versions : %s (using %s)\n", strings.Join(r.Server.Versions, ", "), r.Server.ProtocolVersion)
	}
	if r.Server.TLSVersion != "" {
		fmt.Fprintf(w, "  tls      : %s\n", r.Server.TLSVersion)
	}
	if r.Server.Policy != "" {
		fmt.Fprintf(w, "  policy   : %s\n", r.Server.Policy)
	}
	fmt.Fprintf(w, "  device   : %s (%s)\n", r.DeviceID, r.DeviceType)
	if r.StatePath != "" {
		fmt.Fprintf(w, "  state    : %s\n", r.StatePath)
	}
	fmt.Fprintln(w)

	for _, s := range r.Steps {
		// Skipped && !OK: not run because an earlier step failed.
		// Skipped && OK: ran as a no-op (e.g. already provisioned).
		status := "ok"
		switch {
		case s.Skipped && !s.OK:
			status = "skipped"
		case !s.OK:
			status = "FAIL"
		case s.Skipped:
			status = "ok (not needed)"
		}
		fmt.Fprintf(w, "  %-12s %-16s %8s", s.Name, status, round(s.Duration))
		if s.HTTPStatus != 0 {
			fmt.Fprintf(w, "  http=%d", s.HTTPStatus)
		}
		fmt.Fprintln(w)

		if s.Error != "" {
			fmt.Fprintf(w, "      error [%s]: %s\n", s.ErrorClass, s.Error)
		}
		for _, k := range SortedKeys(s.Info) {
			fmt.Fprintf(w, "      %s=%s\n", k, s.Info[k])
		}

		// For a failed step show where the time went and what the server offered.
		if !s.OK && !s.Skipped {
			for _, x := range r.Exchanges {
				if x.Step != s.Name {
					continue
				}
				what := x.Method
				if x.Cmd != "" {
					what += " " + x.Cmd
				}
				fmt.Fprintf(w, "      request %s -> %d in %s (dns %s, connect %s, tls %s, first byte %s)\n",
					what, x.Status, round(x.Duration), round(x.DNS), round(x.Connect), round(x.TLS), round(x.FirstByte))
				if x.WWWAuth != "" {
					fmt.Fprintf(w, "      server offers auth: %s\n", x.WWWAuth)
				}
				if x.ServerHdr != "" {
					fmt.Fprintf(w, "      server header: %s\n", x.ServerHdr)
				}
			}
		}
	}

	if hint := authHint(r); hint != "" {
		fmt.Fprintf(w, "\n  !! %s\n", hint)
	}
}

// authHint explains a refused login in plain words; it is the failure that
// most often hides the real state of the server, and repeating it can get the
// account or the client address blocked.
func authHint(r *Result) string {
	for _, s := range r.Steps {
		if s.OK || s.Skipped {
			continue
		}
		switch s.ErrorClass {
		case "auth":
			return "login refused (HTTP 401): check the user name and password, and whether the account or this client address is locked out or blocked. Do not repeat the probe in a loop until this is clear: repeated failed logins can extend a block."
		case "forbidden":
			return "access refused (HTTP 403): the user or the device may be blocked or not allowed (device access rules, deny lists)."
		}
	}
	return ""
}

func round(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	}
	return d.String()
}
