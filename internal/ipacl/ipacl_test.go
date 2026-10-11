package ipacl_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/internal/ipacl"
)

func mustParse(t *testing.T, entries ...string) *ipacl.List {
	t.Helper()
	l, err := ipacl.Parse(entries)
	if err != nil {
		t.Fatalf("%q: %v", entries, err)
	}
	return l
}

func TestAnEmptyListMeansNoRestriction(t *testing.T) {
	for _, in := range [][]string{nil, {}, {""}, {"  ", ","}} {
		l := mustParse(t, in...)
		if !l.Empty() {
			t.Errorf("%q is not empty: %v", in, l.Entries())
		}
	}
	var none *ipacl.List
	if !none.Empty() || !none.Allows(netip.MustParseAddr("203.0.113.9")) {
		t.Error("a nil list has to allow everything")
	}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:1"
	none.Wrap(next, nil).ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("a nil list kept a request out")
	}
}

func TestSingleAddressesNetworksAndRanges(t *testing.T) {
	l := mustParse(t, "10.1.2.3", "192.168.0.0/16", "2001:db8::/32", "fd00::1", "172.20.0.10-172.20.0.20")
	for addr, want := range map[string]bool{
		"10.1.2.3": true, "10.1.2.4": false, // a single address is a /32
		"192.168.0.1": true, "192.168.255.254": true, "192.169.0.1": false, // a network
		"2001:db8::1": true, "2001:db8:ffff::1": true, "2001:db9::1": false, // an IPv6 network
		"fd00::1": true, "fd00::2": false, // a single IPv6 address
		"172.20.0.10": true, "172.20.0.15": true, "172.20.0.20": true, "172.20.0.9": false, "172.20.0.21": false, // a range, both ends in
		"203.0.113.9": false,
	} {
		if got := l.Allows(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The loopback interface is always allowed, whatever the list says: the health check of a container comes from there.
func TestLoopbackIsAlwaysAllowed(t *testing.T) {
	l := mustParse(t, "10.0.0.0/8")
	for _, addr := range []string{"127.0.0.1", "127.5.5.5", "127.255.255.255", "::1", "::ffff:127.0.0.1", "::ffff:127.3.2.1"} {
		if !l.Allows(netip.MustParseAddr(addr)) {
			t.Errorf("%s (loopback) is refused", addr)
		}
	}
	for _, addr := range []string{"128.0.0.1", "126.255.255.255", "::2", "::ffff:128.0.0.1", "fe80::1"} {
		if l.Allows(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not loopback and is allowed", addr)
		}
	}
	// the word is accepted and changes nothing
	if lo := mustParse(t, "loopback", "10.0.0.0/8"); lo.Empty() || !lo.Allows(netip.MustParseAddr("10.9.9.9")) || lo.Allows(netip.MustParseAddr("192.0.2.1")) {
		t.Error("the word loopback broke the list")
	}
	// a list with only that word is not "no restriction": it allows this machine and nothing else
	only := mustParse(t, "loopback")
	if only.Empty() || only.Allows(netip.MustParseAddr("10.1.2.3")) || !only.Allows(netip.MustParseAddr("127.0.0.1")) {
		t.Errorf("the list loopback: empty %v, entries %v", only.Empty(), only.Entries())
	}
}

func TestEntriesCanBeSeparatedByCommasAndPrivateIsTheUsualNetworks(t *testing.T) {
	l := mustParse(t, "10.0.0.0/8, 192.168.1.5", "private")
	for addr, want := range map[string]bool{
		"10.9.9.9": true, "192.168.1.5": true, "192.168.200.1": true, "172.16.0.1": true, "172.31.255.255": true, "172.32.0.1": false,
		"fc00::1": true, "fdff::1": true, "fe80::1": false, "8.8.8.8": false, "100.64.0.1": false,
	} {
		if got := l.Allows(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allows(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestAnIPv4AddressInAnIPv6FormAndAZone(t *testing.T) {
	l := mustParse(t, "10.0.0.0/8", "fe80::/10")
	if !l.Allows(netip.MustParseAddr("::ffff:10.0.0.5")) {
		t.Error("an IPv4 peer that the system shows as ::ffff:10.0.0.5 is refused")
	}
	if l.Allows(netip.MustParseAddr("::ffff:11.0.0.5")) {
		t.Error("::ffff:11.0.0.5 is allowed")
	}
	if !l.Allows(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a link-local address with its zone is refused")
	}
}

func TestWrongEntriesAreErrorsThatSayWhich(t *testing.T) {
	for _, bad := range []string{"banana", "10.0.0.300", "10.0.0.0/33", "10.0.0.0/-1", "10.0.0.5-10.0.0.1", "10.0.0.1-::1", "10.0.0.1-", "-10.0.0.1", "10.0.0.1-10.0.0.2-10.0.0.3", "*", "0.0.0.0/0x"} {
		_, err := ipacl.Parse([]string{"192.168.0.0/16", bad})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: %v; want an error that names the entry", bad, err)
		}
	}
}

func TestEntriesAreWrittenBackInOneForm(t *testing.T) {
	l := mustParse(t, " 10.1.2.3 ", "10.0.0.7/8", "FD00::1", "private", "10.0.0.5-10.0.0.9")
	got := strings.Join(l.Entries(), " ")
	// a network is written with its own base address, a single address as it is, the rest as given
	for _, want := range []string{"10.1.2.3", "10.0.0.0/8", "fd00::1", "private", "10.0.0.5-10.0.0.9"} {
		if !strings.Contains(got, want) {
			t.Errorf("entries = %q, lacks %q", got, want)
		}
	}
}

func TestWrapKeepsOutWhatIsNotAllowed(t *testing.T) {
	l := mustParse(t, "10.0.0.0/8")
	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; _, _ = w.Write([]byte("hello")) })
	var denied []string
	h := l.Wrap(next, func(remote string, r *http.Request) { denied = append(denied, remote) })

	for remote, want := range map[string]int{
		"10.1.2.3:5555": 200, "[::ffff:10.1.2.3]:80": 200, "127.0.0.1:4000": 200, "[::1]:4000": 200, // the last two: loopback is always in
		"192.0.2.1:5555": 403, "[2001:db8::1]:443": 403,
		"garbage": 403, "": 403, "@": 403, // not an address: refused
	} {
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%q: %d, want %d", remote, rec.Code, want)
		}
		if want == 403 && (strings.TrimSpace(rec.Body.String()) != "Forbidden" || rec.Header().Get("Content-Type") == "") {
			t.Errorf("%q: the refusal says %q (type %q); it must say nothing about the list", remote, rec.Body.String(), rec.Header().Get("Content-Type"))
		}
	}
	if called != 4 {
		t.Errorf("the handler was called %d times, want 4 (only for the allowed)", called)
	}
	if len(denied) != 5 {
		t.Errorf("the refusals that were reported: %q", denied)
	}
	found := false
	for _, d := range denied {
		found = found || d == "192.0.2.1:5555"
	}
	if !found {
		t.Errorf("the refusal of 192.0.2.1 was not reported: %q", denied)
	}
}

// Every path is guarded, not only the API: the check comes before the router.
func TestWrapGuardsEveryPathAndMethod(t *testing.T) {
	l := mustParse(t, "10.0.0.0/8")
	h := l.Wrap(http.NotFoundHandler(), nil)
	for _, method := range []string{"GET", "POST", "DELETE", "OPTIONS", "HEAD"} {
		for _, path := range []string{"/", "/health", "/metrics", "/docs", "/openapi.json", "/nothing/at/all"} {
			req := httptest.NewRequest(method, path, nil)
			req.RemoteAddr = "192.0.2.1:1"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 403 {
				t.Errorf("%s %s from outside = %d, want 403", method, path, rec.Code)
			}
		}
	}
}
