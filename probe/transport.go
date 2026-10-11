package probe

import (
	"crypto/tls"
	"github.com/nashcom/eas-tools/internal/logging"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

// Exchange is one HTTP round trip as seen on the wire. The EAS library
// issues several of these per logical command (OPTIONS, the two-phase
// Provision, the bootstrap Sync), so the probe records each one.
type Exchange struct {
	Step       string        `json:"step"`
	Method     string        `json:"method"`
	Cmd        string        `json:"cmd,omitempty"`
	Status     int           `json:"status"`
	Duration   time.Duration `json:"duration_ns"`
	DNS        time.Duration `json:"dns_ns,omitempty"`
	Connect    time.Duration `json:"connect_ns,omitempty"`
	TLS        time.Duration `json:"tls_ns,omitempty"`
	FirstByte  time.Duration `json:"first_byte_ns,omitempty"`
	ReqBytes   int64         `json:"req_bytes"`
	RespBytes  int64         `json:"resp_bytes"`
	Err        string        `json:"error,omitempty"`
	TLSVersion string        `json:"tls_version,omitempty"`
	ServerHdr  string        `json:"server,omitempty"`
	WWWAuth    string        `json:"www_authenticate,omitempty"`
}

// recorder collects Exchanges and tags them with the currently running step.
type recorder struct {
	mu        sync.Mutex
	step      string
	exchanges []Exchange
	// headers of the most recent OPTIONS reply, kept for server detection
	optionsHdr http.Header
	log        *slog.Logger
	// keep stores every exchange (the one-shot probe reports them); the load
	// worker turns it off and only uses hook, so memory stays flat.
	keep bool
	hook func(Exchange)
	// extra request headers (see headerSet)
	hdr *headerSet
}

func (r *recorder) setStep(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.step = name
}

func (r *recorder) currentStep() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.step
}

func (r *recorder) add(x Exchange) {
	r.mu.Lock()
	if r.keep {
		r.exchanges = append(r.exchanges, x)
	}
	r.mu.Unlock()

	if r.hook != nil {
		r.hook(x)
	}

	if r.log != nil {
		what := x.Method
		if x.Cmd != "" {
			what += " " + x.Cmd
		}
		logging.Trace(r.log, "http", "step", x.Step, "request", what, "status", x.Status,
			"duration", x.Duration.Round(time.Millisecond), "first_byte", x.FirstByte.Round(time.Millisecond),
			"sent", x.ReqBytes, "received", x.RespBytes, "tls", x.TLSVersion, "error", x.Err)
	}
}

func (r *recorder) all() []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Exchange, len(r.exchanges))
	copy(out, r.exchanges)
	return out
}

// timingTransport wraps a RoundTripper and records timing per request.
type timingTransport struct {
	next http.RoundTripper
	rec  *recorder
}

func (t *timingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	x := Exchange{
		Step:   t.rec.currentStep(),
		Method: req.Method,
		Cmd:    req.URL.Query().Get("Cmd"),
	}
	if req.ContentLength > 0 {
		x.ReqBytes = req.ContentLength
	}

	// What goes on the wire, without secrets: lets a 401 be compared with
	// a working curl call (user agent, auth scheme and length, cookie).
	if t.rec.log != nil {
		auth := req.Header.Get("Authorization")
		scheme, _, _ := strings.Cut(auth, " ")
		logging.Trace(t.rec.log, "http request", "step", x.Step, "method", req.Method, "host", req.URL.Host,
			"path", req.URL.Path, "user_agent", req.UserAgent(), "auth_scheme", scheme, "auth_length", len(auth),
			"cookie_sent", req.Header.Get("Cookie") != "", "policy_key_sent", req.Header.Get("X-MS-PolicyKey") != "",
			"protocol_version", req.Header.Get("MS-ASProtocolVersion"))
	}

	var dnsStart, connStart, tlsStart time.Time
	start := time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:           func(httptrace.DNSDoneInfo) { x.DNS = time.Since(dnsStart) },
		ConnectStart:      func(_, _ string) { connStart = time.Now() },
		ConnectDone:       func(_, _ string, _ error) { x.Connect = time.Since(connStart) },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(cs tls.ConnectionState, _ error) {
			x.TLS = time.Since(tlsStart)
			x.TLSVersion = tlsVersionName(cs.Version)
		},
		GotFirstResponseByte: func() { x.FirstByte = time.Since(start) },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		x.Duration = time.Since(start)
		x.Err = err.Error()
		t.rec.add(x)
		return nil, err
	}

	x.Status = resp.StatusCode
	x.ServerHdr = resp.Header.Get("Server")
	x.WWWAuth = strings.Join(resp.Header.Values("WWW-Authenticate"), " | ")

	if req.Method == http.MethodOptions {
		t.rec.mu.Lock()
		t.rec.optionsHdr = resp.Header.Clone()
		t.rec.mu.Unlock()
	}

	// The exchange completes when the body has been consumed or closed,
	// which is the number a user cares about (long Ping holds, big Syncs).
	resp.Body = &countingBody{
		rc: resp.Body,
		done: func(n int64, rerr error) {
			x.RespBytes = n
			x.Duration = time.Since(start)
			if rerr != nil && rerr != io.EOF {
				x.Err = rerr.Error()
			}
			t.rec.add(x)
		},
	}
	return resp, nil
}

// conflictDelays are the waits before the retries of a request the server
// answered with HTTP 409.
var conflictDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// conflictTransport retries a request that the server answers with HTTP 409
// Conflict. That is not a failure: the request clashes with another request of
// the same device that the server still holds (for example a Ping that is not
// finished yet), so the same request is tried again after a short wait. It sits
// above timingTransport, so every attempt is recorded.
type conflictTransport struct {
	next   http.RoundTripper
	log    *slog.Logger
	delays []time.Duration
}

func (t *conflictTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusConflict || attempt >= len(t.delays) {
			return resp, err
		}

		// The request can only be sent again if its body can be recreated.
		again := req
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return resp, nil
			}
			body, berr := req.GetBody()
			if berr != nil {
				return resp, nil
			}
			again = req.Clone(req.Context())
			again.Body = body
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if t.log != nil {
			t.log.Warn("HTTP 409 Conflict: another request of this device is pending; trying again",
				"cmd", req.URL.Query().Get("Cmd"), "attempt", attempt+1, "wait", t.delays[attempt])
		}
		select {
		case <-time.After(t.delays[attempt]):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		req = again
	}
}

// countingBody counts bytes and fires done exactly once at EOF/error/Close.
type countingBody struct {
	rc   io.ReadCloser
	n    int64
	once sync.Once
	done func(n int64, err error)
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n += int64(n)
	if err != nil {
		b.once.Do(func() { b.done(b.n, err) })
	}
	return n, err
}

func (b *countingBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.done(b.n, nil) })
	return err
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	}
	return ""
}
