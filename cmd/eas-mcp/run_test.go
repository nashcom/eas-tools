package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests run the program itself, run(), the way a container does: the options, the listeners, the log. They are slow to write and worth it: the
// glue between the options and the parts is what a test of the parts does not see.

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// the account is one that nobody listens at: the tools are listed and the endpoint is tried, no mailbox is read
var baseArgs = []string{"-server", "http://127.0.0.1:1", "-email", "a@example.com", "-no-state"}

func runEnv() []string { return []string{"EASCLIENT_PASSWORD=x"} }

// start runs the program in the background, as a server does, and returns what it writes to stderr. The server runs until the test binary ends.
func start(t *testing.T, extra ...string) *syncBuf {
	t.Helper()
	errw := &syncBuf{}
	args := append(append([]string{}, baseArgs...), extra...)
	go run(args, strings.NewReader(""), io.Discard, errw, runEnv)
	return errw
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}

func healthy(url string, c *http.Client) func() bool {
	return func() bool {
		r, err := c.Get(url)
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	}
}

func TestTheProgramRefusesWhatIsWrong(t *testing.T) {
	idp := []string{"-oidc-issuer", "https://idp.invalid.example", "-oidc-audience", "https://x.example/mcp", "-oidc-users", "*"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"oidc options without the issuer":  {[]string{"-oidc-users", "*"}, "need -oidc-issuer"},
		"an issuer without an audience":    {[]string{"-oidc-issuer", "https://idp.example.com"}, "-oidc-audience is needed"},
		"an issuer without users":          {[]string{"-oidc-issuer", "https://idp.example.com", "-oidc-audience", "a"}, "users that are allowed"},
		"an issuer that is not https":      {[]string{"-oidc-issuer", "http://idp.example.com", "-oidc-audience", "a", "-oidc-users", "*"}, "not an https URL"},
		"a choice for the old addresses":   {append(append([]string{}, idp...), "-oidc-compat-issuer", "nonsense"), "use provider, origin or off"},
		"a transport that does not exist":  {[]string{"-transport", "pigeon"}, "use http or stdio"},
		"the password on stdin with stdio": {[]string{"-transport", "stdio", "-password-stdin"}, "-password-stdin cannot be used"},
		"stdout for the audit with stdio":  {[]string{"-transport", "stdio", "-audit-log", "stdout"}, "cannot be used with -transport stdio"},
		"an audit log that cannot be made": {[]string{"-audit-log", filepath.Join(t.TempDir(), "no", "dir", "a.json")}, "-audit-log"},
		"a certificate without a key":      {[]string{"-tls-cert", "x.crt"}, "go together"},
		"plain HTTP and a certificate":     {[]string{"-allow-http", "-tls-cert", "a", "-tls-key", "b"}, "exclude each other"},
		"a stray argument":                 {[]string{"stray"}, "takes options only"},
	} {
		var errw syncBuf
		code := run(append(append([]string{}, baseArgs...), c.args...), strings.NewReader(""), io.Discard, &errw, runEnv)
		if code != 2 || !strings.Contains(errw.String(), c.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 2 and %q", name, code, errw.String(), c.want)
		}
	}
}

func TestTheProgramServesTheEndpointOverPlainHTTP(t *testing.T) {
	port := freePort(t)
	args := []string{"-allow-http", "-listen", "127.0.0.1:" + port}
	errw := start(t, args...)
	url := "http://127.0.0.1:" + port
	waitFor(t, "the endpoint", healthy(url+"/health", http.DefaultClient))

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	names := strings.Join(toolNames(t, cs), " ")
	if !strings.Contains(names, "list_messages") || !strings.Contains(names, "list_events") || strings.Contains(names, "send_message") {
		t.Errorf("tools: %s", names)
	}
	if r, err := http.Get(url + "/nothing"); err == nil {
		r.Body.Close()
	}
	waitFor(t, "the log", func() bool { return strings.Contains(errw.String(), "/nothing") })
	out := errw.String()
	for _, want := range []string{"MCP endpoint started", "stateless=true", "protocol_versions=2026-07-28", "a request for an address that does not exist"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log has no %q:\n%s", want, out)
		}
	}
	// the health check of a container asks the same program with the same options
	var chk syncBuf
	if code := run(append(append(append([]string{}, baseArgs...), args...), "-health-check"), strings.NewReader(""), io.Discard, &chk, runEnv); code != 0 {
		t.Errorf("-health-check: %d %s", code, chk.String())
	}
}

func TestTheProgramServesHTTPSWithACertificateOfItsOwnAndAHealthPort(t *testing.T) {
	dir := t.TempDir()
	port, hport := freePort(t), freePort(t)
	args := []string{"-listen", "127.0.0.1:" + port, "-tls-cert", filepath.Join(dir, "tls.crt"), "-tls-key", filepath.Join(dir, "tls.key"), "-tls-generate", "-listen-health", "127.0.0.1:" + hport}
	errw := start(t, args...)
	waitFor(t, "the health port", healthy("http://127.0.0.1:"+hport+"/health", http.DefaultClient))

	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if !healthy("https://127.0.0.1:"+port+"/health", c)() {
		t.Error("the HTTPS port does not answer for a client that trusts the CA")
	}
	if r, err := http.Get("http://127.0.0.1:" + hport + "/mcp"); err != nil || r.StatusCode != http.StatusNotFound { // the health port has /health only
		t.Errorf("/mcp on the health port: %v %v", r, err)
	}
	// plain HTTP on the HTTPS port: the failure of the handshake is in the log, structured, as a warning
	if r, err := http.Get("http://127.0.0.1:" + port + "/health"); err == nil {
		r.Body.Close()
	}
	waitFor(t, "the handshake error in the log", func() bool { return strings.Contains(errw.String(), "TLS handshake error") })
	if out := errw.String(); !strings.Contains(out, " WARN  http: TLS handshake error") || !strings.Contains(out, "listener name=health") || !strings.Contains(out, "proto=https") {
		t.Errorf("the log:\n%s", out)
	}
	var chk syncBuf
	if code := run(append(append(append([]string{}, baseArgs...), args...), "-health-check"), strings.NewReader(""), io.Discard, &chk, runEnv); code != 0 {
		t.Errorf("-health-check with TLS: %d %s", code, chk.String())
	}
}

func TestTheProgramChecksTokensOfAProviderAndWritesTheAuditLog(t *testing.T) {
	p := newIdP(t)
	dir, err := os.MkdirTemp("", "eas-mcp-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) }) // the server keeps the audit file open until the test program ends: on Windows that stops the removal, and the error is not one
	audit, keys := filepath.Join(dir, "audit.json"), filepath.Join(dir, "oidc-keys.json")
	port := freePort(t)
	start(t, "-allow-http", "-listen", "127.0.0.1:"+port, "-oidc-issuer", p.URL, "-oidc-audience", resourceURL, "-oidc-users", "*@example.com",
		"-audit-log", audit, "-oidc-cache-file", keys)
	url := "http://127.0.0.1:" + port
	waitFor(t, "the endpoint", healthy(url+"/health", http.DefaultClient))

	// no token: 401, and the pointer to the metadata names the address that the client used
	resp := post(t, url+"/mcp", "")
	if want := `resource_metadata="` + url + `/.well-known/oauth-protected-resource/mcp"`; resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), want) {
		t.Errorf("%d %q, want %s", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
	}
	// the old address of the login server: the document of the provider as it is
	if r, err := http.Get(url + "/.well-known/oauth-authorization-server"); err != nil || r.StatusCode != 200 {
		t.Errorf("the old address: %v %v", r, err)
	} else {
		r.Body.Close()
	}
	// a token of the provider opens the endpoint
	tr := &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: withToken{p.token(t, resourceURL, "alice@example.com")}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if len(toolNames(t, cs)) < 10 {
		t.Error("the tools are not listed for a user with a token")
	}
	post(t, url+"/mcp", "Bearer "+p.token(t, resourceURL, "mallory@evil.org"))

	waitFor(t, "the audit log", func() bool {
		raw, _ := os.ReadFile(audit)
		return strings.Contains(string(raw), "mcp.auth.denied") && strings.Contains(string(raw), "mcp.auth.config") && strings.Contains(string(raw), "mcp.server.start")
	})
	if _, err := os.Stat(keys); err != nil {
		t.Errorf("the signing keys were not saved: %v", err)
	}
}

func TestTheCertificateOfTheProviderIsTrustedFromTheCAFile(t *testing.T) {
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer idp.Close()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := issuerClient(file); err != nil {
		t.Fatal(err)
	} else if r, err := c.Get(idp.URL); err != nil {
		t.Errorf("a provider with a certificate of the CA file: %v", err)
	} else {
		r.Body.Close()
	}
	if c, _ := issuerClient(""); c != nil {
		if r, err := c.Get(idp.URL); err == nil {
			r.Body.Close()
			t.Error("a certificate of an unknown CA was trusted without the CA file")
		}
	}
	if _, err := issuerClient(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("a CA file that is not there was accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	if _, err := issuerClient(bad); err == nil {
		t.Error("a CA file without a certificate was accepted")
	}
}

func TestWhatTheAuditLogTakesFromTheArgumentsOfACall(t *testing.T) {
	got := func(tool, args string) string {
		var sb strings.Builder
		for i, v := range auditArguments(tool, []byte(args)) {
			if i%2 == 0 {
				sb.WriteString(v.(string) + "=")
			} else {
				sb.WriteString(strings.TrimSpace(strings.ReplaceAll(strings.Join(toStrings(v), ","), "\n", " ")) + ";")
			}
		}
		return sb.String()
	}
	if g := got("reply_message", `{"id":"4:1","all":true,"body":"secret"}`); !strings.Contains(g, "mcp.id=4:1") || !strings.Contains(g, "mail.reply_all=true") || strings.Contains(g, "secret") {
		t.Errorf("reply: %s", g)
	}
	if g := got("create_event", `{"subject":"private subject","attendees":[{"email":"a@x.org"},{"email":"b@y.org","name":"B"}]}`); !strings.Contains(g, "event.attendees=a@x.org,b@y.org") || strings.Contains(g, "private") {
		t.Errorf("create_event: %s", g)
	}
	if g := got("send_message", `not json`); g != "" {
		t.Errorf("arguments that are no JSON: %s", g)
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []string:
		return x
	case bool:
		if x {
			return []string{"true"}
		}
		return []string{"false"}
	}
	return []string{"?"}
}

func TestWhatTheEndpointAcceptsIsDescribed(t *testing.T) {
	p := newIdP(t)
	a, err := newAuthn("tok", nil, "", nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := a.describe(); kind != "token" {
		t.Errorf("the local token: %s", kind)
	}
	var none *authn
	if kind, _ := none.describe(); kind != "none" {
		t.Errorf("no authentication: %s", kind)
	}
	api := oidcEndpoint(t, p, "tok")
	_ = api
}
