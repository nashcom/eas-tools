package main

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/exporter"
	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/probe"
)

const docOne = `{"min_interval_seconds": 61, "targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com", "password": "pw"}]}`
const docTwo = `{"min_interval_seconds": 61, "targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com", "password": "pw"}, {"name": "second","server": "traveler2.example.com", "user": "v@example.com", "password": "pw2"}]}`

type remote struct {
	srv  *httptest.Server
	body atomic.Value
	down atomic.Bool
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	r := &remote{}
	r.body.Store(docOne)
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if req.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(r.body.Load().(string)))
	}))
	t.Cleanup(r.srv.Close)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASEXPORTER_CONFIG_CA_FILE", ca)
	t.Setenv("EASEXPORTER_CONFIG_TOKEN", "tok")
	t.Setenv("EASEXPORTER_CONFIG_CACHE", filepath.Join(dir, "cache.json"))
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "1")
	return r
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestStartFromTheRemoteLocationAndFromTheSavedCopy(t *testing.T) {
	r := newRemote(t)

	cs, err := startConfig(r.srv.URL+"/config.json", quiet)
	if err != nil || !cs.fresh || 1 != len(cs.cfg.Targets) || 61 != cs.cfg.MinIntervalSeconds {
		t.Fatalf("start: %v %+v", err, cs)
	}
	if _, err := os.Stat(cs.remote.CachePath); err != nil {
		t.Errorf("the document must be saved: %v", err)
	}

	// the location is down at the next start: the saved copy is used, and it is not "fresh"
	r.down.Store(true)
	cs2, err := startConfig(r.srv.URL+"/config.json", quiet)
	if err != nil || cs2.fresh || 1 != len(cs2.cfg.Targets) {
		t.Errorf("from the saved copy: %v %+v", err, cs2)
	}

	// no saved copy and no location: the start fails with both reasons
	if err := os.Remove(cs.remote.CachePath); err != nil {
		t.Fatal(err)
	}
	if _, err := startConfig(r.srv.URL+"/config.json", quiet); err == nil || !strings.Contains(err.Error(), "no saved copy") {
		t.Errorf("no copy: %v", err)
	}

	// only https
	if _, err := startConfig("http://example.com/c.json", quiet); err == nil || !strings.Contains(err.Error(), "only https://") {
		t.Errorf("http: %v", err)
	}
}

func startServer(t *testing.T, cs *configStart) *exporter.Server {
	t.Helper()
	store, _ := probe.OpenStore("")
	srv, err := exporter.NewServer(cs.cfg, store, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func waitFor(t *testing.T, srv *exporter.Server, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 80; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out: %s (targets %v)", what, srv.TargetNames())
}

func metrics(srv *exporter.Server) string {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestReloadLoopAppliesChangesAndSurvivesFailures(t *testing.T) {
	r := newRemote(t)
	cs, err := startConfig(r.srv.URL+"/config.json", quiet)
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, cs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, r.srv.URL+"/config.json", quiet)

	// a changed document is applied
	r.body.Store(docTwo)
	waitFor(t, srv, "the second target", func() bool { return 2 == len(srv.TargetNames()) })

	// a broken document and an unreachable location keep the configuration and are counted
	r.body.Store(`{"targets": [{"name": "lab","server": "x"}]}`)
	time.Sleep(1500 * time.Millisecond)
	r.down.Store(true)
	time.Sleep(1500 * time.Millisecond)
	if 2 != len(srv.TargetNames()) {
		t.Errorf("a failed reload must keep the configuration: %v", srv.TargetNames())
	}
	body := metrics(srv)
	if strings.Contains(body, "eas_probe_config_reload_errors_total 0") || !strings.Contains(body, "eas_probe_config_reload_errors_total") {
		t.Errorf("the failures must be counted:\n%s", body)
	}
	if !strings.Contains(body, "eas_probe_config_reloads_total 1") {
		t.Errorf("exactly one reload was applied:\n%s", body)
	}

	// it recovers by itself
	r.down.Store(false)
	r.body.Store(docOne)
	waitFor(t, srv, "back to one target", func() bool { return 1 == len(srv.TargetNames()) })
}

// A document that only looks different is not a reload: same settings, other spaces and key order.
func TestReloadLoopIgnoresADocumentThatOnlyLooksDifferent(t *testing.T) {
	r := newRemote(t)
	cs, err := startConfig(r.srv.URL+"/config.json", quiet)
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, r.srv.URL+"/config.json", quiet)

	r.body.Store("{\n  \"targets\": [{\"name\": \"lab\", \"password\": \"pw\", \"user\": \"u@example.com\", \"server\": \"traveler.example.com\"}],\n  \"min_interval_seconds\": 61\n}\n")
	time.Sleep(2500 * time.Millisecond)
	body := metrics(srv)
	if !strings.Contains(body, "eas_probe_config_reloads_total 0") || !strings.Contains(body, "eas_probe_config_reload_errors_total 0") {
		t.Errorf("a document with the same settings is no reload:\n%s", body)
	}
	if strings.Contains(body, "eas_probe_config_last_check_timestamp_seconds 0\n") {
		t.Errorf("the check must be recorded:\n%s", body)
	}
}

// A configuration file is checked by its time and size, read when that changed, and applied when its settings changed.
func TestReloadLoopWatchesAFile(t *testing.T) {
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "1")
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(doc string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	write(docOne, t0)

	cs, err := startConfig(path, quiet)
	if err != nil || nil == cs.source || 1 != cs.checkEvery || nil != cs.remote {
		t.Fatalf("start: %v %+v", err, cs)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, path, quiet)

	// touched, same content: nothing to apply
	write(docOne, t0.Add(time.Minute))
	time.Sleep(1500 * time.Millisecond)
	if !strings.Contains(metrics(srv), "eas_probe_config_reloads_total 0") {
		t.Error("a touched file with the same content is no reload")
	}

	// changed content: applied
	write(docTwo, t0.Add(2*time.Minute))
	waitFor(t, srv, "the second target", func() bool { return 2 == len(srv.TargetNames()) })

	// a broken file keeps the configuration
	write(`{"targets": {`, t0.Add(3*time.Minute))
	time.Sleep(1500 * time.Millisecond)
	if 2 != len(srv.TargetNames()) || strings.Contains(metrics(srv), "eas_probe_config_reload_errors_total 0") {
		t.Errorf("a broken file must keep the configuration and be counted: %v", srv.TargetNames())
	}

	// fixed again
	write(docOne, t0.Add(4*time.Minute))
	waitFor(t, srv, "back to one target", func() bool { return 1 == len(srv.TargetNames()) })
}

// A configuration that comes from the environment alone has no file to check.
func TestNoSourceWhenTheFileDoesNotExist(t *testing.T) {
	t.Setenv("EASEXPORTER_SERVER", "traveler.example.com")
	t.Setenv("EASEXPORTER_USER", "u@example.com")
	t.Setenv("EASEXPORTER_PASSWORD", "pw")
	cs, err := startConfig(filepath.Join(t.TempDir(), "missing.json"), quiet)
	if err != nil || nil != cs.source || 1 != len(cs.cfg.Targets) {
		t.Errorf("env only: %v %+v", err, cs)
	}
}

// The log says where the configuration comes from and whether that worked.
func TestStartLogsTheSourceAndTheResult(t *testing.T) {
	r := newRemote(t)
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	location := r.srv.URL + "/config.json?secret=1"
	safe := r.srv.URL + "/config.json"

	if _, err := startConfig(location, log); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"loading the configuration", "source=" + safe, "configuration loaded", "from=remote", "targets=1", "check_every=1s"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret=1") {
		t.Errorf("the query string of the URL must not be logged:\n%s", out)
	}

	// the source is down: the saved copy is used, which is a warning
	buf.Reset()
	r.down.Store(true)
	if _, err := startConfig(location, log); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "from=\"saved copy\"") || !strings.Contains(out, "not usable") {
		t.Errorf("the saved copy must be reported as a warning:\n%s", out)
	}

	// nothing works: an error with the source
	buf.Reset()
	t.Setenv("EASEXPORTER_CONFIG_CACHE", filepath.Join(t.TempDir(), "none.json"))
	if _, err := startConfig(location, log); err == nil {
		t.Fatal("no source and no saved copy must fail")
	}
	out = buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "the configuration could not be loaded") || !strings.Contains(out, "source="+safe) {
		t.Errorf("the failure must be logged with the source:\n%s", out)
	}

	// a file
	buf.Reset()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(docOne), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := startConfig(path, log); err != nil {
		t.Fatal(err)
	}
	if out = buf.String(); !strings.Contains(out, "from=file") || !strings.Contains(out, "source="+path) {
		t.Errorf("a file: %s", out)
	}
}

// At the trace level every check says what it compared and why nothing changed.
func TestReloadLoopExplainsAtTraceLevelWhyNothingChanged(t *testing.T) {
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "1")
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: logging.LevelTrace}))

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(docOne), 0o600); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	_ = os.Chtimes(path, t0, t0)
	cs, err := startConfig(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, path, log)

	// nothing touched: time and size
	time.Sleep(1500 * time.Millisecond)
	out := buf.String()
	for _, want := range []string{"checking the configuration", "modification time", "result=\"not modified\"", "time and size of the file are those of the file in use"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	// touched, same content: the hash
	_ = os.Chtimes(path, t0.Add(time.Minute), t0.Add(time.Minute))
	time.Sleep(1500 * time.Millisecond)
	if out = buf.String(); !strings.Contains(out, "result=\"same content\"") || !strings.Contains(out, "sha256=") {
		t.Errorf("same content not explained:\n%s", out)
	}

	// other bytes, same settings: both hashes
	same := strings.Replace(docOne, `{"min_interval_seconds": 61,`, "{\n \"min_interval_seconds\": 61,\n", 1)
	if err := os.WriteFile(path, []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, t0.Add(2*time.Minute), t0.Add(2*time.Minute))
	time.Sleep(1500 * time.Millisecond)
	if out = buf.String(); !strings.Contains(out, "the bytes differ, but the settings are the same") || !strings.Contains(out, " -> ") {
		t.Errorf("same settings not explained:\n%s", out)
	}
}

// syncBuffer is a log destination that the reload loop and the test can use at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLogLevelFromTheConfigurationAndTheCommandLine(t *testing.T) {
	defer func(v, d bool) { cmdVerbose, cmdDebug = v, d; logState.SetLevel(slog.LevelInfo) }(cmdVerbose, cmdDebug)
	cfg := func(level string) *exporter.Config { return &exporter.Config{LogLevel: level} }

	cmdVerbose, cmdDebug = false, false
	for level, want := range map[string]slog.Level{"": slog.LevelInfo, "warn": slog.LevelWarn, "info": slog.LevelInfo, "debug": slog.LevelDebug, "trace": logging.LevelTrace} {
		applyLogging(cfg(level))
		if logState.Level() != want {
			t.Errorf("log_level %q: %v, want %v", level, logState.Level(), want)
		}
	}

	// a reload that changes log_level changes the level; the command line wins
	cmdVerbose = true
	applyLogging(cfg("debug"))
	if logState.Level() != slog.LevelDebug {
		t.Errorf("-verbose: %v", logState.Level())
	}
	cmdDebug = true
	applyLogging(cfg("warn"))
	if logState.Level() != logging.LevelTrace {
		t.Errorf("-debug: %v", logState.Level())
	}
}

func TestLogFormatFromTheConfigurationAndTheCommandLine(t *testing.T) {
	defer func(f string) { cmdLogFormat = f; _ = logState.SetFormat("text") }(cmdLogFormat)

	cmdLogFormat = ""
	applyLogging(&exporter.Config{})
	if logState.Format() != "text" {
		t.Errorf("default: %s", logState.Format())
	}
	applyLogging(&exporter.Config{LogFormat: "json"})
	if logState.Format() != "json" {
		t.Errorf("log_format json: %s", logState.Format())
	}
	// the command line wins, and a reload that changes the file does not undo it
	cmdLogFormat = "text"
	applyLogging(&exporter.Config{LogFormat: "json"})
	if logState.Format() != "text" {
		t.Errorf("-log-format text: %s", logState.Format())
	}
}

// A check on request does what the timer does, at once, and also works when the interval is 0 (only on request).
func TestReloadOnRequest(t *testing.T) {
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "0")
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(doc string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(docOne)

	cs, err := startConfig(path, quiet)
	if err != nil || cs.source == nil || cs.checkEvery != 0 {
		t.Fatalf("start: %v %+v", err, cs)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, path, quiet)

	ask := func() reloadResult {
		t.Helper()
		rctx, c := context.WithTimeout(ctx, 10*time.Second)
		defer c()
		res, ok := cs.reloadNow(rctx)
		if !ok {
			t.Fatal("no source")
		}
		return res
	}

	if res := ask(); res.Result != "unchanged" {
		t.Errorf("nothing changed: %+v", res)
	}
	write(docTwo)
	// the file time is the same second: change it, the way an editor does
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, later, later)
	res := ask()
	if res.Result != "applied" || len(res.Targets) != 2 || len(srv.TargetNames()) != 2 {
		t.Fatalf("a changed file: %+v, targets %v", res, srv.TargetNames())
	}
	if res = ask(); res.Result != "unchanged" {
		t.Errorf("the same file again: %+v", res)
	}

	write(`{"targets": [`)
	later = later.Add(time.Minute)
	_ = os.Chtimes(path, later, later)
	if res = ask(); res.Result != "failed" || res.Error == "" || len(srv.TargetNames()) != 2 {
		t.Errorf("a broken file must fail and keep the configuration: %+v, targets %v", res, srv.TargetNames())
	}
}

func TestReloadOnRequestWithoutASource(t *testing.T) {
	t.Setenv("EASEXPORTER_SERVER", "traveler.example.com")
	t.Setenv("EASEXPORTER_USER", "u@example.com")
	t.Setenv("EASEXPORTER_PASSWORD", "pw")
	cs, err := startConfig(filepath.Join(t.TempDir(), "missing.json"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cs.reloadNow(context.Background()); ok {
		t.Error("a configuration from the environment has nothing to check")
	}
}

// The status is 1 while the source is broken and 0 again after the next check that works; the counter keeps counting.
func TestConfigStatusFollowsTheSource(t *testing.T) {
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "0")
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(doc string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(path, mtime, mtime)
	}
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	write(docOne, t0)

	cs, err := startConfig(path, quiet)
	if err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, nil, path, quiet)

	has := func(want string) bool { return strings.Contains(metrics(srv), want) }
	check := func() {
		t.Helper()
		rctx, c := context.WithTimeout(ctx, 10*time.Second)
		defer c()
		if _, ok := cs.reloadNow(rctx); !ok {
			t.Fatal("no source")
		}
	}

	check()
	if !has("eas_probe_config_status 0\n") {
		t.Errorf("the start:\n%s", metrics(srv))
	}
	write(`{"targets": [`, t0.Add(time.Minute))
	check()
	if !has("eas_probe_config_status 1\n") || !has("eas_probe_config_reload_errors_total 1\n") {
		t.Errorf("a broken file:\n%s", metrics(srv))
	}
	check() // still broken: every check of a broken document fails and counts, and the status stays 1
	if !has("eas_probe_config_status 1\n") || !has("eas_probe_config_reload_errors_total 2\n") {
		t.Errorf("still broken:\n%s", metrics(srv))
	}
	write(docTwo, t0.Add(2*time.Minute))
	check()
	if !has("eas_probe_config_status 0\n") || !has("eas_probe_config_reload_errors_total 2\n") {
		t.Errorf("fixed again (the counter stays):\n%s", metrics(srv))
	}
}
