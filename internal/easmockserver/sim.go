package easmockserver

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// SimHeader is the request header that tells the mock server how to behave for
// this request. It is honored only when Options.Sim is set.
//
//	X-Mock-Sim: cmd=Sync;status=3;count=2
//
// The value is a list of key=value pairs separated by semicolons. More than
// one header line gives more than one directive; they are checked in order.
//
//	cmd=NAME     only for this command (Sync, FolderSync, Ping, Provision, Settings,
//	             Search, SendMail, or OPTIONS); without it, for every request
//	count=N      only for the first N matching requests of a device; later ones are
//	             answered normally (to test retries and recovery)
//	delay=3s     wait this long before the request is handled
//	http=502     answer with this HTTP status instead of handling the request
//	status=3     answer with this EAS status for the command (needs cmd=)
//	garbage=1    answer 200 with a body that is not WBXML
//	close=1      close the connection without an answer
//
// delay can be combined with one of the other actions.
const SimHeader = "X-Mock-Sim"

type simDirective struct {
	cmd     string
	count   int
	delay   time.Duration
	httpSt  int
	status  int
	garbage bool
	closeIt bool
	raw     string
}

func parseSim(v string) simDirective {
	d := simDirective{raw: v, status: -1}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' }) {
		k, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.TrimSpace(val)
		switch k {
		case "cmd":
			d.cmd = val
		case "count":
			d.count, _ = strconv.Atoi(val)
		case "delay":
			d.delay, _ = time.ParseDuration(val)
		case "http":
			d.httpSt, _ = strconv.Atoi(val)
		case "status":
			if n, err := strconv.Atoi(val); err == nil {
				d.status = n
			}
		case "garbage":
			d.garbage = val != "" && val != "0"
		case "close":
			d.closeIt = val != "" && val != "0"
		}
	}
	return d
}

// simulate applies the directives of the request. It returns true when it has
// answered the request (or dropped the connection); false means the request is
// handled normally.
func (f *Server) simulate(w http.ResponseWriter, r *http.Request) bool {
	if !f.opts.Sim {
		return false
	}

	cmd := r.URL.Query().Get("Cmd")
	if r.Method == http.MethodOptions {
		cmd = "OPTIONS"
	}
	device := r.URL.Query().Get("DeviceId")

	for _, v := range r.Header.Values(SimHeader) {
		d := parseSim(v)
		if d.cmd != "" && !strings.EqualFold(d.cmd, cmd) {
			continue
		}
		if d.count > 0 {
			key := device + "|" + d.raw
			f.mu.Lock()
			n := f.simUsed[key]
			f.simUsed[key] = n + 1
			f.mu.Unlock()
			if n >= d.count {
				continue
			}
		}

		f.mu.Lock()
		f.simHits++
		f.mu.Unlock()

		if d.delay > 0 {
			select {
			case <-time.After(d.delay):
			case <-r.Context().Done():
				return true
			}
		}

		switch {
		case d.closeIt:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return true
				}
			}
			panic(http.ErrAbortHandler) // fallback: abort the connection
		case d.httpSt >= 100:
			http.Error(w, "simulated", d.httpSt)
			return true
		case d.garbage:
			w.Header().Set("Content-Type", "application/vnd.ms-sync.wbxml")
			_, _ = w.Write([]byte("\x03\x01\x6a\x00\xff\xfe not a valid wbxml document"))
			return true
		case d.status >= 0 && cmd != "" && cmd != "OPTIONS":
			f.reply(w, f.statusOnly(cmd, d.status))
			return true
		}
	}
	return false
}

// statusOnlyOther is the answer of the commands that statusOnly does not know
// by name: a document with just the status.
func statusOnlyOther(cmd, st string) *wbxml.Element {
	switch cmd {
	case "Provision":
		return wbxml.E(wbxml.PageProvision, "Provision", txt(wbxml.PageProvision, "Status", st))
	case "Settings":
		return wbxml.E(wbxml.PageSettings, "Settings", txt(wbxml.PageSettings, "Status", st))
	case "Search":
		return wbxml.E(wbxml.PageSearch, "Search", txt(wbxml.PageSearch, "Status", st))
	case "SendMail":
		return wbxml.E(wbxml.PageComposeMail, "SendMail", txt(wbxml.PageComposeMail, "Status", st))
	}
	return nil
}

// SimHits returns how many requests an X-Mock-Sim directive acted on, so a
// test can be sure that the simulated misbehavior really happened.
func (f *Server) SimHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.simHits
}
