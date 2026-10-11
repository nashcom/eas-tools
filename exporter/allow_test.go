package exporter

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// request makes a request that seems to come from the given address (a real connection cannot choose it).
func request(srv *Server, method, path, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAllowFromGuardsEveryEndpointOfTheExporter(t *testing.T) {
	srv, _, _ := newTestServer(t, func(c *Config) { c.AllowFrom = []string{"10.0.0.0/8", "192.168.1.5"} })

	for _, path := range []string{"/health", "/metrics", "/probe?target=lab", "/debug/state", "/config/reload", "/nothing"} {
		if rec := request(srv, "GET", path, "203.0.113.9:4000"); rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != "Forbidden" {
			t.Errorf("GET %s from outside = %d %q, want 403", path, rec.Code, rec.Body.String())
		}
		if rec := request(srv, "GET", path, "10.1.2.3:4000"); rec.Code == 403 {
			t.Errorf("GET %s from 10.1.2.3 was refused", path)
		}
	}
	// the other allowed address, and loopback is always in (the health check of the container)
	for _, remote := range []string{"192.168.1.5:1", "127.0.0.1:1", "[::1]:1"} {
		if rec := request(srv, "GET", "/health", remote); rec.Code != 200 {
			t.Errorf("/health from %s = %d", remote, rec.Code)
		}
	}
	if rec := request(srv, "GET", "/health", "192.168.1.6:1"); rec.Code != 403 {
		t.Errorf("a neighbor of an allowed address = %d, want 403", rec.Code)
	}
}

func TestNoAllowFromMeansNoRestriction(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	if rec := request(srv, "GET", "/health", "203.0.113.9:1"); rec.Code != 200 {
		t.Errorf("no list: %d", rec.Code)
	}
}

func TestRefusedRequestsAreCountedAndShown(t *testing.T) {
	srv, _, _ := newTestServer(t, func(c *Config) { c.AllowFrom = []string{"10.0.0.0/8"} })
	for i := 0; i < 3; i++ {
		request(srv, "GET", "/metrics", "203.0.113.9:1")
	}
	request(srv, "GET", "/metrics", "10.0.0.1:1")

	rec := request(srv, "GET", "/metrics", "127.0.0.1:1")
	if !strings.Contains(rec.Body.String(), "eas_probe_denied_requests_total 3\n") {
		t.Errorf("the counter of refused requests is not 3:\n%s", rec.Body.String())
	}
	state := request(srv, "GET", "/debug/state", "127.0.0.1:1")
	if !strings.Contains(state.Body.String(), `"allow_from"`) || !strings.Contains(state.Body.String(), "10.0.0.0/8") {
		t.Errorf("the debug state does not show the list:\n%s", state.Body.String())
	}
}

// A reload applies the list at once, in both directions.
func TestReloadAppliesAllowFrom(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	cfg := *srv.cur.Load().cfg

	cfg.AllowFrom = []string{"10.0.0.0/8"}
	if _, err := srv.Reload(&cfg); err != nil {
		t.Fatal(err)
	}
	if rec := request(srv, "GET", "/health", "203.0.113.9:1"); rec.Code != 403 {
		t.Errorf("after the reload with a list: %d, want 403", rec.Code)
	}

	// a wrong list is a failed reload: the last good one stays
	bad := cfg
	bad.AllowFrom = []string{"10.0.0.0/8", "not-an-address"}
	if _, err := srv.Reload(&bad); err == nil || !strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("a wrong entry: %v", err)
	}
	if rec := request(srv, "GET", "/health", "203.0.113.9:1"); rec.Code != 403 {
		t.Errorf("after the failed reload the list is gone: %d", rec.Code)
	}
	if rec := request(srv, "GET", "/health", "10.9.9.9:1"); rec.Code != 200 {
		t.Errorf("after the failed reload an allowed address is refused: %d", rec.Code)
	}

	cfg.AllowFrom = nil
	if _, err := srv.Reload(&cfg); err != nil {
		t.Fatal(err)
	}
	if rec := request(srv, "GET", "/health", "203.0.113.9:1"); rec.Code != 200 {
		t.Errorf("after the reload without a list: %d, want 200", rec.Code)
	}
}

func TestAllowFromInTheConfigFileAndTheEnvironment(t *testing.T) {
	doc := `{"allow_from": ["10.0.0.0/8", "192.168.1.0/24"], "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`
	cfg, err := LoadConfigEnv(writeConfig(t, doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AllowFrom, " "); got != "10.0.0.0/8 192.168.1.0/24" {
		t.Errorf("from the file: %q", got)
	}

	// the environment wins over the file; a text with commas is a list
	cfg, err = LoadConfigEnv(writeConfig(t, doc), []string{"EASEXPORTER_ALLOW_FROM=172.16.0.0/12, 10.1.1.1-10.1.1.9"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AllowFrom, "|"); got != "172.16.0.0/12|10.1.1.1-10.1.1.9" {
		t.Errorf("from the environment: %q", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("warnings: %v", cfg.Warnings)
	}

	// a wrong entry is refused when the configuration is read, with its name
	bad := `{"allow_from": ["10.0.0.0/8", "garbage"], "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`
	if _, err := LoadConfigEnv(writeConfig(t, bad), nil); err == nil || !strings.Contains(err.Error(), "allow_from") || !strings.Contains(err.Error(), "garbage") {
		t.Errorf("a wrong entry: %v", err)
	}
	if _, err := LoadConfigEnv(writeConfig(t, doc), []string{"EASEXPORTER_ALLOW_FROM=10.0.0.300"}); err == nil || !strings.Contains(err.Error(), "10.0.0.300") {
		t.Errorf("a wrong entry of the environment: %v", err)
	}
}
