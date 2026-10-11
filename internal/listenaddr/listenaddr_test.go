package listenaddr

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
)

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "[::1]:1": true, "localhost:1": true, ":1": false, "0.0.0.0:1": false, "10.0.0.1:1": false, "nonsense": false} {
		if got := Loopback(addr); got != want {
			t.Errorf("Loopback(%q) = %v", addr, got)
		}
	}
}

func TestDefaultNextIsTheNextPort(t *testing.T) {
	// closed (the health port of the client): loopback; open (the metrics port): the interfaces of the main listener
	for listen, want := range map[string][2]string{":9116": {"127.0.0.1:9117", ":9117"}, "0.0.0.0:8090": {"127.0.0.1:8091", "0.0.0.0:8091"}, "[::]:443": {"127.0.0.1:444", "[::]:444"},
		"127.0.0.1:0": {"127.0.0.1:0", "127.0.0.1:0"}, "127.0.0.1:9116": {"127.0.0.1:9117", "127.0.0.1:9117"}} {
		for i, open := range []bool{false, true} {
			got, err := DefaultNext(listen, open)
			if err != nil || got != want[i] {
				t.Errorf("DefaultNext(%q, %v) = %q, %v; want %q", listen, open, got, err, want[i])
			}
		}
	}
	for _, bad := range []string{"9116", ":http", ":65535", ":x"} {
		if _, err := DefaultNext(bad, true); err == nil {
			t.Errorf("DefaultNext(%q): no error", bad)
		}
	}
}

func TestSecondAddressRules(t *testing.T) {
	closed := func(listen, addr string, allowed []string) (string, error) {
		return Second(listen, addr, allowed, "allow_from_health", false)
	}
	open := func(listen, addr string, allowed []string) (string, error) {
		return Second(listen, addr, allowed, "allow_from_metrics", true)
	}

	// the health port of the client: loopback by default, another address needs the list of who may connect
	if a, err := closed(":8090", "", nil); err != nil || a != "127.0.0.1:8091" {
		t.Errorf("closed, default: %q %v", a, err)
	}
	if _, err := closed(":8090", "0.0.0.0:8091", nil); err == nil || !strings.Contains(err.Error(), "allow_from_health") {
		t.Errorf("closed, open address without a list: %v", err)
	}
	if a, err := closed(":8090", "0.0.0.0:8091", []string{"10.1.2.0/24"}); err != nil || a != "0.0.0.0:8091" {
		t.Errorf("closed, with a list: %q %v", a, err)
	}
	// the metrics port: open by default, the list restricts it
	if a, err := open(":9116", "", nil); err != nil || a != ":9117" {
		t.Errorf("open, default: %q %v", a, err)
	}
	if a, err := open(":9116", "0.0.0.0:9000", nil); err != nil || a != "0.0.0.0:9000" {
		t.Errorf("open, own address without a list: %q %v", a, err)
	}
	if a, err := open(":9116", "127.0.0.1:9000", nil); err != nil || a != "127.0.0.1:9000" {
		t.Errorf("open, loopback: %q %v", a, err)
	}
	for _, f := range []func(string, string, []string) (string, error){closed, open} {
		if _, err := f(":9116", ":9116", []string{"10.0.0.0/8"}); err == nil {
			t.Error("the same port twice")
		}
		if _, err := f(":9116", "nonsense", nil); err == nil {
			t.Error("an address without a port")
		}
	}
}

func TestPlainHTTPIsAChoiceOffTheLoopback(t *testing.T) {
	for _, c := range []struct {
		listen          string
		cert, allow, ok bool
	}{
		{":9116", false, false, false}, // everything in clear text on the network: refused
		{"0.0.0.0:9116", false, false, false},
		{"10.0.0.5:9116", false, false, false},
		{":9116", true, false, true},           // HTTPS
		{":9116", false, true, true},           // chosen on purpose, behind a proxy
		{"127.0.0.1:9116", false, false, true}, // never leaves the machine
		{"[::1]:9116", false, false, true},
		{"localhost:9116", false, false, true},
	} {
		err := NeedsHTTPSwitch(c.listen, c.cert, c.allow)
		if (err == nil) != c.ok {
			t.Errorf("%+v: %v", c, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "allow_http") || !strings.Contains(err.Error(), "tls_cert")) {
			t.Errorf("the message names no way out: %v", err)
		}
	}
}

func TestScopeAndFromAreShort(t *testing.T) {
	for addr, want := range map[string]string{":9116": "all interfaces", "0.0.0.0:1": "all interfaces", "[::]:1": "all interfaces",
		"127.0.0.1:1": "loopback only", "[::1]:1": "loopback only", "10.1.2.3:1": "the interface 10.1.2.3"} {
		if got := Scope(addr); !strings.HasPrefix(got, want) {
			t.Errorf("Scope(%q) = %q, want %q", addr, got, want)
		}
	}
	for l, want := range map[string]Listener{
		"open":                 {Configured: ":9117"},
		"10.1.2.0/24,10.1.1.1": {Configured: ":9117", Allow: []string{"10.1.2.0/24", "10.1.1.1"}},
		"loopback":             {Configured: "127.0.0.1:1"},
	} {
		if got := want.From(); got != l {
			t.Errorf("From() = %q, want %q", got, l)
		}
	}
	if (Listener{}).Proto() != "http" || (Listener{TLS: true}).Proto() != "https" {
		t.Error("the protocol is http or https")
	}
}

// One short line for each port: nobody wants to read prose in the log of the start.
func TestOneShortLinePerPort(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil)) // info: the explanations at debug level are not here
	Listener{Name: "main", Configured: ":9117", Bound: &net.TCPAddr{IP: net.IPv4zero, Port: 9117}, TLS: true, Allow: []string{"10.0.0.0/8"},
		Endpoints: []Endpoint{{"/probe", "the probe", "token", "header Authorization"}, {"/metrics", "the metrics", "none", ""}, {"/health", "liveness", "none", ""},
			{"/debug/state", "state", "token", "header Authorization"}}, Note: "HTTPS only: there is no plain HTTP on this port"}.Log(log)
	Listener{Name: "metrics", Configured: ":9100", Bound: &net.TCPAddr{IP: net.IPv4zero, Port: 9100},
		Endpoints: []Endpoint{{"/metrics", "the metrics", "none", ""}, {"/health", "liveness", "none", ""}}}.Log(log)
	out := buf.String()
	t.Log("\n" + out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want one for each of the two ports:\n%s", len(lines), out)
	}
	for i, want := range [][]string{
		{"msg=listener", "name=main", "addr=0.0.0.0:9117", "proto=https", "from=10.0.0.0/8", "auth=\"token(/probe /debug/state) none(/metrics /health)\""},
		{"name=metrics", "addr=0.0.0.0:9100", "proto=http", "from=open", "auth=\"none(/metrics /health)\""},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d lacks %q:\n%s", i, w, lines[i])
			}
		}
		if len(lines[i]) > 180 {
			t.Errorf("line %d is %d characters: too long to read", i, len(lines[i]))
		}
		for _, no := range []string{"note=", "interfaces=", "not encrypted", "configured=", "everyone"} {
			if strings.Contains(lines[i], no) {
				t.Errorf("line %d has %q: that is for debug level", i, no)
			}
		}
	}

	// at debug level there is the explanation, and every endpoint has its line with what it is
	buf.Reset()
	debug := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	Listener{Name: "main", Configured: ":9117", Bound: &net.TCPAddr{IP: net.IPv4zero, Port: 9117}, Note: "HTTPS only",
		Endpoints: []Endpoint{{"/probe", "the probe", "token", "header Authorization: Bearer"}}}.Log(debug)
	d := buf.String()
	for _, w := range []string{"msg=\"listener detail\"", "interfaces=\"all interfaces\"", "note=\"HTTPS only\"", "msg=endpoint", "what=\"the probe\"", "detail=\"header Authorization: Bearer\""} {
		if !strings.Contains(d, w) {
			t.Errorf("debug level lacks %q:\n%s", w, d)
		}
	}
}

// Plain HTTP says why on the line of the start, so that nobody has to ask why there is no TLS.
func TestAPlainLineSaysWhyThereIsNoTLS(t *testing.T) {
	for _, c := range []struct {
		listen string
		allow  bool
		want   string
	}{{"127.0.0.1:9116", false, "tls_off=\"listen is loopback\""}, {":9116", true, "tls_off=allow_http"}} {
		var buf bytes.Buffer
		Listener{Name: "main", Configured: c.listen, PlainWhy: PlainWhy(c.listen, c.allow), Endpoints: []Endpoint{{"/metrics", "", "none", ""}}}.Log(slog.New(slog.NewTextHandler(&buf, nil)))
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s: %s", c.listen, buf.String())
		}
	}
	var buf bytes.Buffer
	Listener{Name: "main", Configured: ":9117", TLS: true, PlainWhy: "allow_http"}.Log(slog.New(slog.NewTextHandler(&buf, nil)))
	if strings.Contains(buf.String(), "tls_off") {
		t.Errorf("a TLS line says why there is no TLS: %s", buf.String())
	}
}
