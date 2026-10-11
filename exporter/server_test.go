package exporter

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

func newTestServer(t *testing.T, mutate func(*Config)) (*Server, *httptest.Server, *easmockserver.Server) {
	t.Helper()

	fake := easmockserver.New()
	eas := httptest.NewServer(fake)
	t.Cleanup(eas.Close)

	t.Setenv("TEST_EAS_PASSWORD", easmockserver.Password)
	cfg := &Config{
		Listen:                ":0",
		MinIntervalSeconds:    60,
		MaxConcurrent:         4,
		MaxPingSeconds:        5,
		DefaultTimeoutSeconds: 10,
		Targets: map[string]Target{
			"lab": {Server: eas.URL, User: easmockserver.User, PasswordEnv: "TEST_EAS_PASSWORD"},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}

	store, err := probe.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv.Handler())
	t.Cleanup(front.Close)
	return srv, front, fake
}

func get(t *testing.T, url string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestProbeEndpoint(t *testing.T) {
	_, front, _ := newTestServer(t, nil)

	code, body := get(t, front.URL+"/probe?target=lab")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, want := range []string{
		"eas_probe_success 1",
		`eas_probe_step_success{step="sync"} 1`,
		"eas_probe_timestamp_seconds",
		"eas_probe_cache_age_seconds 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q:\n%s", want, body)
		}
	}
}

func TestMinIntervalCache(t *testing.T) {
	srv, front, fake := newTestServer(t, nil)

	get(t, front.URL+"/probe?target=lab")
	syncs := fake.Count("Sync")
	if syncs == 0 {
		t.Fatal("first scrape did not probe")
	}

	// A second scrape inside the window must not touch the server.
	code, body := get(t, front.URL+"/probe?target=lab")
	if code != 200 || !strings.Contains(body, "eas_probe_success 1") {
		t.Fatalf("cached scrape: %d %s", code, body)
	}
	if fake.Count("Sync") != syncs {
		t.Error("cached scrape contacted the server")
	}
	if srv.cacheHits.Load() != 1 {
		t.Errorf("cache hits = %d", srv.cacheHits.Load())
	}

	// Age the cache entry: the next scrape probes again (incrementally).
	ts := srv.cur.Load().targets["lab"]
	ts.mu.Lock()
	c := ts.cache[probeKind{}]
	c.at = time.Now().Add(-2 * time.Minute)
	ts.cache[probeKind{}] = c
	ts.mu.Unlock()

	get(t, front.URL+"/probe?target=lab")
	if fake.Count("Sync") == syncs {
		t.Error("stale cache entry was not refreshed")
	}
}

func TestUnknownTargetAndMethod(t *testing.T) {
	_, front, _ := newTestServer(t, nil)

	if code, _ := get(t, front.URL+"/probe?target=nope"); code != 404 {
		t.Errorf("unknown target: %d", code)
	}
	if code, _ := get(t, front.URL+"/probe"); code != 404 {
		t.Errorf("missing target: %d", code)
	}
	resp, err := http.Post(front.URL+"/probe?target=lab", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestAuthToken(t *testing.T) {
	t.Setenv("TEST_EAS_TOKEN", "s3cret")
	_, front, _ := newTestServer(t, func(c *Config) { c.AuthTokenEnv = "TEST_EAS_TOKEN" })

	if code, _ := get(t, front.URL+"/probe?target=lab"); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := get(t, front.URL+"/probe?target=lab", "Authorization", "Bearer wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := get(t, front.URL+"/probe?target=lab", "Authorization", "Bearer s3cret"); code != 200 {
		t.Errorf("header token: %d", code)
	}
	if code, _ := get(t, front.URL+"/probe?target=lab&auth=s3cret"); code != 200 {
		t.Errorf("param token: %d", code)
	}
	// health and metrics stay open
	if code, _ := get(t, front.URL+"/health"); code != 200 {
		t.Errorf("health: %d", code)
	}
	if code, _ := get(t, front.URL+"/metrics"); code != 200 {
		t.Errorf("metrics: %d", code)
	}
}

func TestPingParameter(t *testing.T) {
	_, front, _ := newTestServer(t, nil)

	if code, _ := get(t, front.URL+"/probe?target=lab&ping=999"); code != 400 {
		t.Errorf("ping above limit: %d", code)
	}
	if code, _ := get(t, front.URL+"/probe?target=lab&ping=abc"); code != 400 {
		t.Errorf("ping not a number: %d", code)
	}
	code, body := get(t, front.URL+"/probe?target=lab&ping=1")
	if code != 200 || !strings.Contains(body, `eas_probe_step_success{step="ping"} 1`) {
		t.Errorf("ping probe: %d\n%s", code, body)
	}
}

func TestBadPasswordConfigIsReported(t *testing.T) {
	_, front, _ := newTestServer(t, func(c *Config) {
		tg := c.Targets["lab"]
		tg.PasswordEnv = "TEST_EAS_UNSET_VARIABLE"
		c.Targets["lab"] = tg
	})

	code, body := get(t, front.URL+"/probe?target=lab")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, "eas_probe_success 0") || !strings.Contains(body, `class="config"`) {
		t.Errorf("config failure must show as failed probe:\n%s", body)
	}
	_, own := get(t, front.URL+"/metrics")
	if !strings.Contains(own, "eas_probe_probe_errors_total 1") {
		t.Errorf("config failure must be counted as a probe error:\n%s", own)
	}
}

func TestOwnMetrics(t *testing.T) {
	_, front, _ := newTestServer(t, nil)
	get(t, front.URL+"/probe?target=lab")

	_, body := get(t, front.URL+"/metrics")
	for _, want := range []string{
		"eas_probe_probes_total 1",
		"eas_probe_probe_errors_total 0",
		"eas_probe_targets 1",
		"eas_probe_devices 1",
		"eas_probe_uptime_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	c, err := LoadConfig(write(`{"targets":[{"name": "a","server":"h","user":"u","password_env":"X"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9117" || !c.TLSAuto || c.MinIntervalSeconds != 60 || c.MaxConcurrent != 10 {
		t.Errorf("defaults not applied: %+v", c)
	}

	bad := map[string]string{
		"no targets":       `{"targets":[]}`,
		"unknown field":    `{"bogus":1,"targets":[{"name": "a","server":"h","user":"u","password_env":"X"}]}`,
		"no password":      `{"targets":[{"name": "a","server":"h","user":"u"}]}`,
		"no server":        `{"targets":[{"name": "a","user":"u","password_env":"X"}]}`,
		"bad target name":  `{"targets":[{"name": "a b","server":"h","user":"u","password_env":"X"}]}`,
		"no user or email": `{"targets":[{"name": "a","server":"h","password_env":"X"}]}`,
	}
	for name, js := range bad {
		if _, err := LoadConfig(write(js)); err == nil {
			t.Errorf("%s: config must be rejected", name)
		}
	}
}

func TestPasswordFileFirstLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(p, []byte("hunter2\r\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Target{PasswordFile: p}.password()
	if err != nil || got != "hunter2" {
		t.Errorf("password = %q, %v", got, err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "deploy", "exporter-container.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 2 {
		t.Errorf("targets = %d", len(c.Targets))
	}
}

func TestLocalExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "deploy", "exporter-local.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 2 {
		t.Errorf("targets = %d", len(c.Targets))
	}
}

func TestAuthFailureBacksOff(t *testing.T) {
	fake := easmockserver.New()
	var requests atomic.Int64
	eas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fake.ServeHTTP(w, r)
	}))
	defer eas.Close()

	t.Setenv("TEST_EAS_WRONG", "not-the-password")
	cfg := &Config{
		MinIntervalSeconds: 60, AuthBackoffSeconds: 600, MaxConcurrent: 4, MaxPingSeconds: 5, DefaultTimeoutSeconds: 10,
		Targets: map[string]Target{"lab": {Server: eas.URL, User: "u", PasswordEnv: "TEST_EAS_WRONG"}},
	}
	store, _ := probe.OpenStore("")
	srv, err := NewServer(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	_, body := get(t, front.URL+"/probe?target=lab")
	if !strings.Contains(body, "eas_probe_success 0") || !strings.Contains(body, `class="auth"`) {
		t.Fatalf("first probe must report the auth failure:\n%s", body)
	}
	if !strings.Contains(body, "eas_probe_auth_backoff 1") {
		t.Errorf("backoff must be visible:\n%s", body)
	}
	first := requests.Load()
	if first == 0 {
		t.Fatal("first probe did not reach the server")
	}

	// Another scrape, also with a different ping variant, must not try the
	// login again.
	get(t, front.URL+"/probe?target=lab")
	get(t, front.URL+"/probe?target=lab&ping=1")
	if requests.Load() != first {
		t.Errorf("a refused login was retried: %d -> %d requests", first, requests.Load())
	}
}

func TestEachKindOfProbeHasItsOwnCache(t *testing.T) {
	srv, front, fake := newTestServer(t, nil)

	// a plain probe: no Ping step at all
	_, plain := get(t, front.URL+"/probe?target=lab")
	if strings.Contains(plain, `step="ping"`) {
		t.Fatalf("a plain probe must not contain a ping step:\n%s", plain)
	}
	if fake.Count("Ping") != 0 {
		t.Fatalf("a plain probe sent %d Ping(s)", fake.Count("Ping"))
	}

	// the push probe must not be answered from the plain probe's cache
	_, push := get(t, front.URL+"/probe?target=lab&ping=1")
	if !strings.Contains(push, `eas_probe_step_success{step="ping"} 1`) || fake.Count("Ping") == 0 {
		t.Fatalf("the ping probe must really run a Ping (Ping requests: %d):\n%s", fake.Count("Ping"), push)
	}

	// each is cached under its own key: asking again hits the cache and neither mixes
	pings, hits := fake.Count("Ping"), srv.cacheHits.Load()
	_, plain2 := get(t, front.URL+"/probe?target=lab")
	_, push2 := get(t, front.URL+"/probe?target=lab&ping=1")
	if fake.Count("Ping") != pings || srv.cacheHits.Load() != hits+2 {
		t.Errorf("repeated scrapes must be cache hits: pings %d -> %d, hits %d -> %d", pings, fake.Count("Ping"), hits, srv.cacheHits.Load())
	}
	if strings.Contains(plain2, `step="ping"`) || !strings.Contains(push2, `step="ping"`) {
		t.Error("the cached results got mixed up")
	}

	// a different heartbeat is a different probe again
	pings = fake.Count("Ping")
	get(t, front.URL+"/probe?target=lab&ping=2")
	if fake.Count("Ping") == pings {
		t.Error("ping=2 must not be served from the ping=1 cache")
	}
}

func TestMailProbeIsOptInAndSeparate(t *testing.T) {
	// a target without the mail option refuses the mail probe and sends nothing
	_, front, fake := newTestServer(t, nil)
	if code, body := get(t, front.URL+"/probe?target=lab&mail=1"); code != 400 || !strings.Contains(body, "not enabled") {
		t.Errorf("mail probe without opt-in: %d %s", code, body)
	}
	if fake.SentCount() != 0 {
		t.Fatal("nothing may be sent without the opt-in")
	}

	// a target with it: its own probe, results kept apart from the plain probe
	fake2 := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true})
	eas := httptest.NewServer(fake2)
	defer eas.Close()
	t.Setenv("TEST_EAS_PASSWORD", easmockserver.Password)
	cfg := &Config{
		MinIntervalSeconds: 60, AuthBackoffSeconds: 600, MaxConcurrent: 4, MaxPingSeconds: 5, DefaultTimeoutSeconds: 10,
		Targets: map[string]Target{"lab": {Server: eas.URL, User: "mia@example.com", PasswordEnv: "TEST_EAS_PASSWORD", Mail: true, MailWaitSeconds: 10}},
	}
	store, _ := probe.OpenStore("")
	srv, err := NewServer(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	front2 := httptest.NewServer(srv.Handler())
	defer front2.Close()

	_, plain := get(t, front2.URL+"/probe?target=lab")
	if strings.Contains(plain, "eas_probe_mail_") || fake2.SentCount() != 0 {
		t.Fatalf("a plain scrape of a mail-enabled target must not send mail (sent %d)", fake2.SentCount())
	}

	_, mail := get(t, front2.URL+"/probe?target=lab&mail=1")
	for _, want := range []string{"eas_probe_success 1", "eas_probe_mail_roundtrip_seconds ", "eas_probe_mail_cleanup_failed 0", "eas_probe_mail_lost 0"} {
		if !strings.Contains(mail, want) {
			t.Errorf("mail probe lacks %q:\n%s", want, mail)
		}
	}
	if fake2.SentCount() != 1 || fake2.MailboxCount("mia@example.com") != 0 {
		t.Errorf("one mail sent and deleted again: sent %d, left %d", fake2.SentCount(), fake2.MailboxCount("mia@example.com"))
	}

	// repeated mail scrapes are cached: no second mail inside the interval
	get(t, front2.URL+"/probe?target=lab&mail=1")
	if fake2.SentCount() != 1 {
		t.Errorf("a repeated mail scrape sent another mail (%d)", fake2.SentCount())
	}
	if code, _ := get(t, front2.URL+"/probe?target=lab&mail=maybe"); code != 400 {
		t.Errorf("a bad mail value: %d", code)
	}
}

func TestMailTargetNeedsAnAddress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	js := `{"targets":[{"name": "a","server":"h","user":"shortname","password_env":"X","mail":true}]}`
	if err := os.WriteFile(p, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Error("a mail probe needs an e-mail address as login")
	}
	js = `{"targets":[{"name": "a","server":"h","user":"a@b.c","password_env":"X","mail":true,"mail_wait_seconds":9999}]}`
	_ = os.WriteFile(p, []byte(js), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Error("mail_wait_seconds above 600 must be refused")
	}
}

// A reload swaps the configuration while the server runs: new targets work at once, removed ones are gone, a target
// that did not change keeps its state, and the settings that need a restart are reported.
func TestReloadAppliesTheNewConfiguration(t *testing.T) {
	srv, front, fake := newTestServer(t, nil)
	_ = fake
	cur := srv.cur.Load()

	status, body := get(t, front.URL+"/probe?target=lab")
	if status != 200 || !strings.Contains(body, "eas_probe_success 1") {
		t.Fatalf("before the reload: %d\n%s", status, body)
	}
	if s, _ := get(t, front.URL+"/probe?target=extra"); s != http.StatusNotFound {
		t.Fatalf("extra must not exist yet: %d", s)
	}
	labState := cur.targets["lab"]

	next := *cur.cfg
	next.Targets = map[string]Target{"lab": cur.cfg.Targets["lab"], "extra": cur.cfg.Targets["lab"]}
	next.Listen = ":9999"
	restart, err := srv.Reload(&next)
	if err != nil {
		t.Fatal(err)
	}
	if 1 != len(restart) || "listen" != restart[0] {
		t.Errorf("restart needed for: %v", restart)
	}
	if srv.cur.Load().targets["lab"] != labState {
		t.Error("an unchanged target must keep its state")
	}
	if status, body := get(t, front.URL+"/probe?target=extra"); status != 200 || !strings.Contains(body, "eas_probe_success 1") {
		t.Errorf("the new target: %d\n%s", status, body)
	}

	// a changed target gets fresh state
	changed := next
	changed.Targets = map[string]Target{"lab": cur.cfg.Targets["lab"]}
	t2 := changed.Targets["lab"]
	t2.Profile = "ipad"
	changed.Targets["lab"] = t2
	if _, err := srv.Reload(&changed); err != nil {
		t.Fatal(err)
	}
	if srv.cur.Load().targets["lab"] == labState {
		t.Error("a changed target must not reuse the old state")
	}
	if s, _ := get(t, front.URL+"/probe?target=extra"); s != http.StatusNotFound {
		t.Errorf("a removed target must be gone: %d", s)
	}

	_, metrics := get(t, front.URL+"/metrics")
	for _, want := range []string{"eas_probe_config_reloads_total 2", "eas_probe_config_reload_errors_total 0", "eas_probe_config_loaded_timestamp_seconds "} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	srv.ReloadFailed(errors.New("test"))
	if _, metrics := get(t, front.URL+"/metrics"); !strings.Contains(metrics, "eas_probe_config_reload_errors_total 1") {
		t.Error("a failed reload must be counted")
	}
}

func TestReloadRefusesAConfigurationWithABadToken(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	before := srv.cur.Load()

	bad := *before.cfg
	bad.AuthTokenFile = filepath.Join(t.TempDir(), "missing-token")
	if _, err := srv.Reload(&bad); err == nil {
		t.Fatal("an unreadable token file must refuse the reload")
	}
	if srv.cur.Load() != before {
		t.Error("the current configuration must stay")
	}
}

// Probes and reloads at the same time: every request uses one complete configuration, and nothing that is shared is
// unlocked. (With the race detector, `go test -race`, this shows any unsynchronized access.)
func TestProbesWhileReloading(t *testing.T) {
	srv, front, _ := newTestServer(t, nil)
	base := srv.cur.Load().cfg

	stop := make(chan struct{})
	var reloaders, readers sync.WaitGroup

	// three goroutines reload two variants over and over
	for g := 0; g < 3; g++ {
		reloaders.Add(1)
		go func(g int) {
			defer reloaders.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				next := *base
				next.Targets = map[string]Target{"lab": base.Targets["lab"]}
				if (i+g)%2 == 0 {
					next.Targets["extra"] = base.Targets["lab"]
					next.MaxConcurrent = base.MaxConcurrent + 1
				}
				if _, err := srv.Reload(&next); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}

	// and clients probe, read the metrics and the target list
	for g := 0; g < 6; g++ {
		readers.Add(1)
		go func(g int) {
			defer readers.Done()
			for i := 0; i < 20; i++ {
				switch (i + g) % 3 {
				case 0:
					if s, _ := get(t, front.URL+"/probe?target=lab"); s != 200 && s != http.StatusServiceUnavailable {
						t.Errorf("probe: status %d", s)
					}
				case 1:
					if s, _ := get(t, front.URL+"/metrics"); s != 200 {
						t.Errorf("metrics: status %d", s)
					}
				default:
					_ = srv.TargetNames()
				}
			}
		}(g)
	}

	readers.Wait()
	close(stop)
	reloaders.Wait()

	if 0 == srv.reloads.Load() {
		t.Error("the reloads must have run")
	}
}

func TestLogLevelSetting(t *testing.T) {
	doc := func(extra string) []byte {
		return []byte(`{` + extra + `"targets": [{"name": "a", "server": "h", "user": "u", "password": "x"}]}`)
	}
	for _, level := range []string{"warn", "info", "debug", "trace"} {
		c, err := LoadConfigBytes(doc(`"log_level": "`+level+`",`), "t", nil)
		if err != nil || c.LogLevel != level {
			t.Errorf("%s: %v %+v", level, err, c)
		}
	}
	for _, bad := range []string{"verbose", "notice", "error", "loud"} {
		if _, err := LoadConfigBytes(doc(`"log_level": "`+bad+`",`), "t", nil); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// the environment wins over the file, and is checked as well
	c, err := LoadConfigBytes(doc(`"log_level": "warn",`), "t", []string{"EASEXPORTER_LOG_LEVEL=debug"})
	if err != nil || c.LogLevel != "debug" {
		t.Errorf("environment: %v %q", err, c.LogLevel)
	}
	if _, err := LoadConfigBytes(doc(""), "t", []string{"EASEXPORTER_LOG_LEVEL=loud"}); err == nil {
		t.Error("a bad level in the environment is accepted")
	}
}

func TestLogFormatSetting(t *testing.T) {
	doc := func(extra string) []byte {
		return []byte(`{` + extra + `"targets": [{"name": "a", "server": "h", "user": "u", "password": "x"}]}`)
	}
	for _, f := range []string{"text", "json", "JSON"} {
		if c, err := LoadConfigBytes(doc(`"log_format": "`+f+`",`), "t", nil); err != nil || c.LogFormat != f {
			t.Errorf("%s: %v", f, err)
		}
	}
	if _, err := LoadConfigBytes(doc(`"log_format": "xml",`), "t", nil); err == nil {
		t.Error("xml accepted")
	}
	c, err := LoadConfigBytes(doc(`"log_format": "text",`), "t", []string{"EASEXPORTER_LOG_FORMAT=json"})
	if err != nil || c.LogFormat != "json" {
		t.Errorf("environment: %v %q", err, c.LogFormat)
	}
	if _, err := LoadConfigBytes(doc(""), "t", []string{"EASEXPORTER_LOG_FORMAT=yaml"}); err == nil {
		t.Error("a bad format in the environment is accepted")
	}
}
