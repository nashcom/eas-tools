package probe

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// traceMax is how much of one request or response body is kept for the trace.
const traceMax = 1 << 20

// Tracer writes every HTTP request and response of a run to a file, with the
// WBXML bodies decoded into readable text. It is for finding out what a server
// really says (the EAS status codes sit inside the bodies, not in HTTP). The
// Authorization and Cookie headers are left out, so the file holds no password.
type Tracer struct {
	mu sync.Mutex
	f  *os.File
}

// OpenTracer opens the trace file for appending (mode 0600).
func OpenTracer(path string) (*Tracer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	t := &Tracer{f: f}
	t.emit(fmt.Sprintf("# eas-probe %s wire trace, started %s\n# Authorization and Cookie headers are left out.\n\n",
		Version, time.Now().Format(time.RFC3339)))
	return t, nil
}

// Close closes the trace file.
func (t *Tracer) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.f.Close()
}

func (t *Tracer) emit(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	_, _ = t.f.WriteString(s)
}

// traceTransport records request and response in the trace. The entry is
// written when the response body is complete, so a held Ping shows up when it
// ends; the start time and the duration are in the entry.
type traceTransport struct {
	next http.RoundTripper
	rec  *recorder
	tr   *Tracer
}

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	step := t.rec.currentStep()

	var reqBody []byte
	if req.GetBody != nil {
		if b, err := req.GetBody(); err == nil {
			reqBody, _ = io.ReadAll(io.LimitReader(b, traceMax))
			b.Close()
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, ">>> %s step=%s %s %s\n", start.Format("15:04:05.000"), step, req.Method, req.URL.String())
	writeHeaders(&sb, req.Header)
	writeBody(&sb, req.Header, reqBody)

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(&sb, "<<< error after %s: %v\n\n", time.Since(start).Round(time.Millisecond), err)
		t.tr.emit(sb.String())
		return nil, err
	}

	resp.Body = &traceBody{rc: resp.Body, done: func(body []byte, rerr error) {
		fmt.Fprintf(&sb, "<<< %s after %s\n", resp.Status, time.Since(start).Round(time.Millisecond))
		writeHeaders(&sb, resp.Header)
		writeBody(&sb, resp.Header, body)
		if rerr != nil && rerr != io.EOF {
			fmt.Fprintf(&sb, "  (reading the body failed: %v)\n", rerr)
		}
		sb.WriteString("\n")
		t.tr.emit(sb.String())
	}}
	return resp, nil
}

// traceBody keeps a copy of what is read and reports it once at the end.
type traceBody struct {
	rc   io.ReadCloser
	buf  bytes.Buffer
	once sync.Once
	done func([]byte, error)
}

func (b *traceBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if room := traceMax - b.buf.Len(); room > 0 && n > 0 {
		b.buf.Write(p[:min(n, room)])
	}
	if err != nil {
		b.once.Do(func() { b.done(b.buf.Bytes(), err) })
	}
	return n, err
}

func (b *traceBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.done(b.buf.Bytes(), nil) })
	return err
}

func writeHeaders(sb *strings.Builder, h http.Header) {
	names := make([]string, 0, len(h))
	for k := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Cookie", "Set-Cookie":
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(sb, "    %s: %s\n", k, strings.Join(h.Values(k), ", "))
	}
}

func writeBody(sb *strings.Builder, h http.Header, body []byte) {
	if strings.EqualFold(h.Get("Content-Encoding"), "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if plain, err := io.ReadAll(io.LimitReader(zr, traceMax)); err == nil {
				body = plain
			}
		}
	}
	if len(body) == 0 {
		sb.WriteString("    (no body)\n")
		return
	}
	reg := wbxml.DefaultRegistry()
	doc, err := wbxml.Unmarshal(body, reg)
	if err != nil || doc.Root == nil {
		fmt.Fprintf(sb, "    (%d bytes, not WBXML)\n", len(body))
		if txt := printable(body); txt != "" {
			fmt.Fprintf(sb, "    %s\n", txt)
		}
		return
	}
	renderNode(sb, reg, doc.Root, 0, 0xff)
}

// printable returns the start of body if it is text.
func printable(b []byte) string {
	if len(b) > 500 {
		b = b[:500]
	}
	for _, c := range b {
		if c < 9 || (c > 13 && c < 32) || c > 126 {
			return ""
		}
	}
	return strings.TrimSpace(string(b))
}

// renderNode prints the element tree as indented XML-like text. The code page
// is named only where it changes, e.g. <Email:Subject>.
func renderNode(sb *strings.Builder, reg *wbxml.Registry, n wbxml.Node, depth int, page byte) {
	pad := strings.Repeat("  ", depth+2)
	switch v := n.(type) {
	case *wbxml.Element:
		name := v.Name
		if v.Codepage != page {
			name = reg.PageName(v.Codepage) + ":" + name
		}
		switch {
		case len(v.Children) == 0:
			fmt.Fprintf(sb, "%s<%s/>\n", pad, name)
		case len(v.Children) == 1 && isText(v.Children[0]):
			fmt.Fprintf(sb, "%s<%s>%s</%s>\n", pad, name, v.Children[0].(wbxml.Text), name)
		default:
			fmt.Fprintf(sb, "%s<%s>\n", pad, name)
			for _, c := range v.Children {
				renderNode(sb, reg, c, depth+1, v.Codepage)
			}
			fmt.Fprintf(sb, "%s</%s>\n", pad, name)
		}
	case wbxml.Text:
		fmt.Fprintf(sb, "%s%s\n", pad, string(v))
	case wbxml.Opaque:
		fmt.Fprintf(sb, "%s[opaque, %d bytes] %q\n", pad, len(v), clip(string(v), 300))
	}
}

func isText(n wbxml.Node) bool {
	_, ok := n.(wbxml.Text)
	return ok
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// newTransport builds the transport chain of a session: retry on 409 on top,
// then the optional trace, then the timing records, then the real transport.
func newTransport(base http.RoundTripper, rec *recorder, cfg Config) http.RoundTripper {
	var next http.RoundTripper = &timingTransport{next: base, rec: rec}
	if cfg.Trace != nil {
		next = &traceTransport{next: next, rec: rec, tr: cfg.Trace}
	}
	var out http.RoundTripper = &conflictTransport{next: next, log: cfg.Logger, delays: conflictDelays}
	if rec.hdr == nil {
		rec.hdr = newHeaderSet(nil)
	}
	return &headerTransport{next: out, set: rec.hdr}
}
