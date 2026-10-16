package listenaddr

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheHealthPort(t *testing.T) {
	for _, c := range []struct {
		name                string
		listen, health      string
		haveCert, allowHTTP bool
		want                string
	}{
		{"TLS by default: the port after the main one", ":8445", "", false, false, "8446"},
		{"a certificate on loopback", "127.0.0.1:8445", "", true, false, "8446"},
		{"an own health address", ":8445", "127.0.0.1:9000", false, false, "9000"},
		{"plain HTTP chosen: the main port", ":8081", "", false, true, "8081"},
		{"loopback without a certificate: the main port", "127.0.0.1:8081", "", false, false, "8081"},
	} {
		got, err := HealthPort(c.listen, c.health, c.haveCert, c.allowHTTP)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	if _, err := HealthPort("nonsense", "", false, false); err == nil {
		t.Error("an address without a port was accepted")
	}
}

func TestCheckHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "OK") }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 503) }))
	defer bad.Close()
	port := func(s *httptest.Server) string {
		_, p, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
		return p
	}

	var out strings.Builder
	if code := CheckHealth(port(ok), &out); code != 0 {
		t.Errorf("a healthy server: %d %s", code, out.String())
	}
	out.Reset()
	if code := CheckHealth(port(bad), &out); code != 1 || !strings.Contains(out.String(), "503") {
		t.Errorf("a server that says 503: %d %q", code, out.String())
	}
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	_, p, _ := net.SplitHostPort(free.Addr().String())
	free.Close()
	out.Reset()
	if code := CheckHealth(p, &out); code != 1 {
		t.Errorf("nothing listens: %d", code)
	}
}
