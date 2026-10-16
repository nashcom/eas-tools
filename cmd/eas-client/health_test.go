package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -health-check is the health check of a container: it asks the running client for /health with the options of that client.
func TestTheHealthCheckOfTheClient(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "OK") }))
	defer ok.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ok.URL, "http://"))
	run1 := func(args ...string) (int, string) {
		var errw strings.Builder
		code := run(args, strings.NewReader(""), io.Discard, &errw, func() []string { return nil })
		return code, errw.String()
	}
	// plain HTTP chosen: the main port is asked, and no account is needed
	if code, msg := run1("-health-check", "-allow-http", "-listen", "127.0.0.1:"+port); code != 0 {
		t.Errorf("a healthy client: %d %s", code, msg)
	}
	// nothing listens at the next port: with TLS the health listener is asked
	if code, _ := run1("-health-check", "-listen", "127.0.0.1:"+port, "-tls-cert", "x.crt", "-tls-key", "x.key"); code != 1 {
		t.Errorf("no health listener: %d, want 1", code)
	}
	// it is not a setting: a variable cannot turn the client into a health check
	var errw strings.Builder
	code := run([]string{"-allow-http", "-listen", "127.0.0.1:" + port, "-server", "s.example.com", "-email", "a@example.com", "-no-state", "accounts"}, strings.NewReader(""), io.Discard, &errw,
		func() []string { return []string{"EASCLIENT_HEALTH_CHECK=true", "EASCLIENT_PASSWORD=x"} })
	if code != 0 {
		t.Errorf("EASCLIENT_HEALTH_CHECK changed the command: %d %s", code, errw.String())
	}
}
