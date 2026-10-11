package probe

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func baseCfg(url string, st *Store) Config {
	return Config{
		ServerURL: url,
		Username:  easmockserver.User,
		Password:  easmockserver.Password,
		Store:     st,
	}
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFullRunAndRerun(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)

	res, err := Run(t.Context(), baseCfg(srv.URL, st))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	WriteText(&buf, res)
	if !res.OK {
		t.Fatalf("first run failed:\n%s", buf.String())
	}
	if res.Server.ProtocolVersion != "14.1" {
		t.Errorf("version = %q, want 14.1", res.Server.ProtocolVersion)
	}
	if !strings.HasPrefix(res.Server.Product, "HCL Traveler") {
		t.Errorf("product = %q", res.Server.Product)
	}
	if got := res.Step(StepSync).Info["first_sync"]; got != "true" {
		t.Errorf("first_sync = %q", got)
	}
	if got := res.Step(StepFolderSync).Info["inbox"]; got != "5" {
		t.Errorf("inbox = %q", got)
	}

	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st = openStore(t, path)

	// Second run after a simulated restart: the state is reloaded from disk, provisioning not repeated, incremental
	// FolderSync must still find the Inbox from the remembered hierarchy.
	res, err = Run(t.Context(), baseCfg(srv.URL, st))
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	WriteText(&buf, res)
	if !res.OK {
		t.Fatalf("second run failed:\n%s", buf.String())
	}
	if !res.Step(StepProvision).Skipped {
		t.Errorf("provision should be skipped on rerun")
	}
	if got := res.Step(StepSync).Info["first_sync"]; got != "false" {
		t.Errorf("rerun first_sync = %q", got)
	}
	if got := res.Step(StepSync).Info["added"]; got != "1" {
		t.Errorf("rerun added = %q, want 1", got)
	}
}

func TestIPhoneProfile(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Profile = "iphone"
	res, err := Run(t.Context(), cfg)
	if err != nil || !res.OK {
		var buf bytes.Buffer
		WriteText(&buf, res)
		t.Fatalf("run failed: %v\n%s", err, buf.String())
	}
	if res.Step(StepSettings) == nil || !res.Step(StepSettings).OK {
		t.Fatalf("settings step missing or failed: %+v", res.Steps)
	}

	ua, typ := fake.SettingsSeen()
	// Traveler's Device Name is the User-Agent part before the "/".
	if name, _, _ := strings.Cut(ua, "/"); name != "Apple-iPhone13C3-easprobe" {
		t.Errorf("user agent = %q", ua)
	}
	if typ != "iPhone" {
		t.Errorf("device type = %q", typ)
	}
	for _, bad := range []string{"BlackBerry", "Windows Phone", "Boxer", "WorxMail"} {
		if strings.Contains(ua+typ, bad) {
			t.Errorf("identity contains excluded word %q", bad)
		}
	}

	// A different device type must not share the default device's id.
	plainCfg := baseCfg(srv.URL, nil)
	plainCfg.Profile = "easprobe"
	plain, _ := Run(t.Context(), plainCfg)
	if plain.DeviceID == res.DeviceID {
		t.Error("each profile must get its own device id")
	}
	if plain.Step(StepSettings) != nil {
		t.Error("the plain profile sends no DeviceInformation")
	}

	// iphone is the default profile.
	def, _ := Run(t.Context(), baseCfg(srv.URL, nil))
	if def.DeviceID != res.DeviceID {
		t.Error("default profile must be iphone")
	}
}

func TestUnknownProfile(t *testing.T) {
	cfg := baseCfg("http://127.0.0.1:1", nil)
	cfg.Profile = "toaster"
	if _, err := Run(t.Context(), cfg); err == nil {
		t.Error("unknown profile must be rejected")
	}
}

func TestFreshReprovisions(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)

	if res, _ := Run(t.Context(), baseCfg(srv.URL, st)); !res.OK {
		t.Fatal("setup run failed")
	}
	cfg := baseCfg(srv.URL, st)
	cfg.Fresh = true
	res, err := Run(t.Context(), cfg)
	if err != nil || !res.OK {
		t.Fatalf("fresh run failed: %v", err)
	}
	if res.Step(StepProvision).Skipped {
		t.Error("fresh run must provision")
	}
}

func TestRecovery449(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)

	if res, _ := Run(t.Context(), baseCfg(srv.URL, st)); !res.OK {
		t.Fatal("setup run failed")
	}
	fake.Demand449()

	res, err := Run(t.Context(), baseCfg(srv.URL, st))
	if err != nil || !res.OK {
		var buf bytes.Buffer
		WriteText(&buf, res)
		t.Fatalf("run after policy loss failed: %v\n%s", err, buf.String())
	}
}

func TestBadPassword(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Password = "wrong"
	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("run with wrong password must fail")
	}
	opt := res.Step(StepOptions)
	if opt == nil || opt.OK || opt.ErrorClass != "auth" {
		t.Fatalf("options step = %+v, want auth failure", opt)
	}
	if s := res.Step(StepSync); s == nil || !s.Skipped {
		t.Errorf("sync should be skipped after failure: %+v", s)
	}

	var buf bytes.Buffer
	WriteText(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "server offers auth: Basic") {
		t.Errorf("report must show the offered auth methods:\n%s", out)
	}
	if strings.Contains(out, "provision    FAIL") {
		t.Errorf("a skipped step must not be reported as FAIL:\n%s", out)
	}
}

func TestPing(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.PingSeconds = 1
	res, err := Run(t.Context(), cfg)
	if err != nil || !res.OK {
		t.Fatalf("run failed: %v", err)
	}
	if p := res.Step(StepPing); p == nil || p.Info["ping_status"] != "1" {
		t.Errorf("ping step = %+v", p)
	}
}

func TestPrometheusOutput(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	res, _ := Run(t.Context(), baseCfg(srv.URL, nil))
	var buf bytes.Buffer
	WritePrometheus(&buf, res)
	out := buf.String()
	for _, want := range []string{
		"eas_probe_success 1",
		`eas_probe_step_success{step="foldersync"} 1`,
		`eas_probe_step_duration_seconds{step="sync"}`,
		`protocol_version="14.1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"mail.example.com":                       "https://mail.example.com/Microsoft-Server-ActiveSync",
		"https://mail.example.com/":              "https://mail.example.com/Microsoft-Server-ActiveSync",
		"http://h:8080":                          "http://h:8080/Microsoft-Server-ActiveSync",
		"https://h/custom/eas?x=1":               "https://h/custom/eas",
		"https://h/Microsoft-Server-ActiveSync/": "https://h/Microsoft-Server-ActiveSync/",
	}
	for in, want := range cases {
		got, err := NormalizeServerURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeServerURL(""); err == nil {
		t.Error("empty server must be an error")
	}
}

func TestDeriveDeviceID(t *testing.T) {
	a := DeriveDeviceID("Alice", "https://H/x")
	b := DeriveDeviceID("alice", "https://h/x")
	if len(a) != 32 || a != b {
		t.Errorf("device id not stable/32 chars: %q %q", a, b)
	}
	if a == DeriveDeviceID("bob", "https://h/x") {
		t.Error("different users must get different ids")
	}
}

func TestStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)

	d := st.Device("AAAA")
	_ = d.SetPolicyKey(t.Context(), "k")
	_ = d.SetSyncKey(t.Context(), "5", "s9")
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	again := openStore(t, path)
	if again.Len() != 1 {
		t.Fatalf("devices = %d, want 1", again.Len())
	}
	d2 := again.Device("AAAA")
	if !d2.HasPolicyKey() {
		t.Error("policy key lost")
	}
	if k, _ := d2.SyncKey(t.Context(), "5"); k != "s9" {
		t.Errorf("sync key = %q", k)
	}
	if k, _ := again.Device("BBBB").SyncKey(t.Context(), "5"); k != "0" {
		t.Errorf("unknown device must start at 0, got %q", k)
	}
}

func TestStoreFlushOnlyWhenDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)

	_ = st.Device("AAAA").SetPolicyKey(t.Context(), "k")
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	fi1, _ := os.Stat(path)

	time.Sleep(20 * time.Millisecond)
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(path)
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Error("unchanged store must not rewrite the file")
	}
}

func TestAutoSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)
	stop := st.AutoSave(10*time.Millisecond, nil)

	_ = st.Device("AAAA").SetPolicyKey(t.Context(), "k")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !openStore(t, path).Device("AAAA").HasPolicyKey() {
		t.Error("autosave did not persist the device")
	}
}

func TestSecondProbeOfSameDeviceRefused(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := baseCfg(srv.URL, openStore(t, ""))
	cfg.DeviceID = DeriveDeviceID("alice", srv.URL)
	dev := cfg.Store.Device(cfg.DeviceID)
	if !dev.TryLock() {
		t.Fatal("setup lock failed")
	}
	defer dev.Unlock()

	res, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(res.Steps) == 0 || res.Steps[0].ErrorClass != "busy" {
		t.Errorf("expected busy refusal, got %+v", res.Steps)
	}
}

func TestAuthHintInReport(t *testing.T) {
	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Password = "wrong"
	res, _ := Run(t.Context(), cfg)

	var buf bytes.Buffer
	WriteText(&buf, res)
	if !strings.Contains(buf.String(), "login refused (HTTP 401)") {
		t.Errorf("report must explain a refused login:\n%s", buf.String())
	}

	ok, _ := Run(t.Context(), baseCfg(srv.URL, nil))
	buf.Reset()
	WriteText(&buf, ok)
	if strings.Contains(buf.String(), "!!") {
		t.Errorf("a good run must have no warning:\n%s", buf.String())
	}
}

func TestProbeMailStep(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "frank@example.com"
	cfg.Mail = true
	cfg.MailWait = 10 * time.Second
	res, err := Run(t.Context(), cfg)
	if err != nil || !res.OK {
		var buf bytes.Buffer
		WriteText(&buf, res)
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	m := res.Step(StepMail)
	if m == nil || !m.OK || m.Info["arrived"] != "true" || m.Info["roundtrip_seconds"] == "" || m.Info["deleted"] != "true" {
		t.Fatalf("mail step = %+v", m)
	}
	if fake.SentCount() != 1 || fake.MailboxCount("frank@example.com") != 0 {
		t.Errorf("sent %d, left in the mailbox %d (the mail must be deleted again)", fake.SentCount(), fake.MailboxCount("frank@example.com"))
	}

	var buf bytes.Buffer
	WritePrometheus(&buf, res)
	for _, want := range []string{"eas_probe_mail_roundtrip_seconds ", "eas_probe_mail_lost 0"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, buf.String())
		}
	}

	// without the option nothing is sent
	cfg.Mail = false
	if r, _ := Run(t.Context(), cfg); r.Step(StepMail) != nil || fake.SentCount() != 1 {
		t.Error("the mail step must not run without -mail")
	}
}

func TestProbeMailKeepAndLost(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "gina@example.com"
	cfg.Mail = true
	cfg.MailKeep = true
	cfg.MailArrival = "poll"
	cfg.MailWait = 10 * time.Second
	if res, _ := Run(t.Context(), cfg); !res.OK || fake.MailboxCount("gina@example.com") != 1 {
		t.Errorf("-mail-keep must leave the mail: ok=%v, mailbox %d", res.OK, fake.MailboxCount("gina@example.com"))
	}

	drop := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true, DropMail: true})
	srv2 := httptest.NewServer(drop)
	defer srv2.Close()
	cfg = baseCfg(srv2.URL, nil)
	cfg.Username = "hank@example.com"
	cfg.Mail = true
	cfg.MailWait = time.Second
	res, _ := Run(t.Context(), cfg)
	m := res.Step(StepMail)
	if res.OK || m == nil || m.OK || m.ErrorClass != "mail_lost" {
		t.Fatalf("a lost mail must fail the mail step with class mail_lost: %+v", m)
	}
	if s := res.Step(StepSync); s == nil || !s.OK {
		t.Error("a lost mail must not fail the other steps")
	}
	var buf bytes.Buffer
	WritePrometheus(&buf, res)
	if !strings.Contains(buf.String(), "eas_probe_mail_lost 1") {
		t.Errorf("metrics must show the lost mail:\n%s", buf.String())
	}
}

func TestRunDevices(t *testing.T) {
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "ivy@example.com"
	results, err := RunDevices(t.Context(), cfg, 3, []string{"iphone", "ipad"}, 1)
	if err != nil || len(results) != 3 {
		t.Fatalf("results %d, %v", len(results), err)
	}
	ids := map[string]bool{}
	for i, r := range results {
		if !r.OK {
			t.Errorf("device %d failed", i+1)
		}
		ids[r.DeviceID] = true
	}
	if len(ids) != 3 || fake.DeviceCount() != 3 {
		t.Errorf("three devices need three ids: %d ids, server saw %d", len(ids), fake.DeviceCount())
	}
	if results[0].DeviceType != "iPhone" || results[1].DeviceType != "iPad" || results[2].DeviceType != "iPhone" {
		t.Errorf("profiles in turn: %s %s %s", results[0].DeviceType, results[1].DeviceType, results[2].DeviceType)
	}

	// a refused login is not repeated for every further device
	bad := baseCfg(srv.URL, nil)
	bad.Username = "ivy@example.com"
	bad.Password = "wrong"
	results, _ = RunDevices(t.Context(), bad, 3, []string{"iphone"}, 1)
	if len(results) != 1 || results[0].OK {
		t.Errorf("after a refused login the loop must stop: %d results", len(results))
	}
}

func TestStoreRecoversFromDamagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// a write that was cut off: valid start, no end
	if err := os.WriteFile(path, []byte(`{"version":1,"devices":{"AAAA":{"policy_key":"k","sync_ke`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("a damaged state file must not stop the tool: %v", err)
	}
	if st.Recovered() == "" || st.Len() != 0 {
		t.Errorf("recovered %q, devices %d", st.Recovered(), st.Len())
	}
	asides, _ := filepath.Glob(path + ".corrupt-*")
	if len(asides) != 1 {
		t.Fatalf("the damaged file must be kept aside for inspection: %v", asides)
	}

	// the store works and writes a good file again
	_ = st.Device("BBBB").SetPolicyKey(t.Context(), "k2")
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(path)
	if err != nil || again.Recovered() != "" || !again.Device("BBBB").HasPolicyKey() {
		t.Errorf("reload after recovery: %v, recovered %q", err, again.Recovered())
	}

	// a state of another program version is set aside the same way
	if err := os.WriteFile(path, []byte(`{"version":99,"devices":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := OpenStore(path); err != nil || st.Recovered() == "" {
		t.Errorf("foreign version: %v, recovered %q", err, st.Recovered())
	}
}

func TestStoreInterruptedWriteKeepsTheOldState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := openStore(t, path)
	_ = st.Device("AAAA").SetPolicyKey(t.Context(), "good")
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	// An interrupted write only ever leaves the temp file half written; the real
	// state file is replaced in one step.
	if err := os.WriteFile(path+".tmp", []byte(`{"version":1,"dev`), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(path)
	if err != nil || again.Recovered() != "" || !again.Device("AAAA").HasPolicyKey() {
		t.Fatalf("the old state must survive a leftover temp file: %v, recovered %q", err, again.Recovered())
	}

	// the next save overwrites the stale temp file and leaves none behind
	_ = again.Device("AAAA").SetPolicyKey(t.Context(), "newer")
	if err := again.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("no temp file may be left after a save")
	}
	last := openStore(t, path)
	if k, _ := last.Device("AAAA").PolicyKey(t.Context()); k != "newer" {
		t.Errorf("policy key = %q", k)
	}
}

func TestMailPushWakesAPendingPing(t *testing.T) {
	// a server that holds the Ping and wakes it for new mail: real push
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "jane@example.com"
	cfg.Mail = true
	cfg.MailArrival = "push" // the experimental push mode
	cfg.MailWait = 10 * time.Second
	res, _ := Run(t.Context(), cfg)
	m := res.Step(StepMail)
	if m == nil || !m.OK || m.Info["push"] != "woke" || m.Info["wake_seconds"] == "" {
		t.Fatalf("the mail must wake the pending Ping: %+v", m)
	}
	var buf bytes.Buffer
	WritePrometheus(&buf, res)
	if !strings.Contains(buf.String(), "eas_probe_mail_push_wake 1") || !strings.Contains(buf.String(), "eas_probe_mail_wake_seconds ") {
		t.Errorf("metrics lack the push result:\n%s", buf.String())
	}

	// a server whose Ping never signals: the mail arrives, the push is reported as missed
	plain := httptest.NewServer(easmockserver.New())
	defer plain.Close()
	cfg = baseCfg(plain.URL, nil)
	cfg.Username = "kim@example.com"
	cfg.Mail = true
	cfg.MailWait = 10 * time.Second
	cfg.MailArrival = "push"
	res, _ = Run(t.Context(), cfg)
	m = res.Step(StepMail)
	if m == nil || !m.OK || m.Info["arrived"] != "true" || m.Info["push"] != "missed" {
		t.Fatalf("mail without a wake-up must still arrive and count as a missed push: %+v", m)
	}
	buf.Reset()
	WritePrometheus(&buf, res)
	if !strings.Contains(buf.String(), "eas_probe_mail_push_wake 0") {
		t.Errorf("a missed push must show as 0:\n%s", buf.String())
	}

	// poll mode makes no push claim at all
	cfg.MailArrival = "poll"
	res, _ = Run(t.Context(), cfg)
	if m := res.Step(StepMail); m == nil || !m.OK || m.Info["push"] != "" {
		t.Errorf("poll mode must not report a push result: %+v", m)
	}
}

func TestRunDevicesInParallelPushWakesEveryDevice(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true, Latency: 20 * time.Millisecond})
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "lee@example.com"
	cfg.Mail = true
	cfg.MailArrival = "push" // the experimental push mode
	cfg.MailWait = 20 * time.Second

	start := time.Now()
	results, err := RunDevices(t.Context(), cfg, 4, []string{"iphone", "ipad"}, 3)
	if err != nil || len(results) != 4 {
		t.Fatalf("results %d, %v", len(results), err)
	}
	t.Logf("4 devices, 3 workers: %v", time.Since(start))

	for i, r := range results {
		m := r.Step(StepMail)
		if !r.OK || m == nil || m.Info["arrived"] != "true" {
			var buf bytes.Buffer
			WriteText(&buf, r)
			t.Errorf("device %d did not get its mail:\n%s", i+1, buf.String())
			continue
		}
		// Several devices share the mailbox: a Ping woken by somebody else's
		// mail is renewed, so every device still sees its own mail through push.
		if m.Info["push"] != "woke" {
			t.Errorf("device %d: push = %q, want woke", i+1, m.Info["push"])
		}
	}
	if fake.SentCount() != 4 || fake.MailboxCount("lee@example.com") != 0 {
		t.Errorf("4 mails sent and all deleted again: sent %d, left %d", fake.SentCount(), fake.MailboxCount("lee@example.com"))
	}
	if fake.DeviceCount() != 4 {
		t.Errorf("devices seen by the server = %d, want 4", fake.DeviceCount())
	}
}

func TestParallelDevicesAreGentleOnAWrongPassword(t *testing.T) {
	fake := easmockserver.New()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()

	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "lee@example.com"
	cfg.Password = "wrong"
	results, err := RunDevices(t.Context(), cfg, 5, []string{"iphone"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("only the pilot may run after a refused login: %d results", len(results))
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("a wrong password cost %d requests, want exactly 1 (the pilot's)", got)
	}
}

func TestRunDevicesSequentialAndParallelAgree(t *testing.T) {
	for _, workers := range []int{1, 4} {
		srv := httptest.NewServer(easmockserver.New())
		cfg := baseCfg(srv.URL, nil)
		cfg.Username = "max@example.com"
		results, err := RunDevices(t.Context(), cfg, 4, []string{"iphone", "ipad"}, workers)
		srv.Close()
		if err != nil || len(results) != 4 {
			t.Fatalf("workers %d: %d results, %v", workers, len(results), err)
		}
		ids := map[string]bool{}
		for _, r := range results {
			if !r.OK {
				t.Errorf("workers %d: a device failed", workers)
			}
			ids[r.DeviceID] = true
		}
		if len(ids) != 4 {
			t.Errorf("workers %d: %d distinct device ids, want 4", workers, len(ids))
		}
		if results[0].DeviceType != "iPhone" || results[1].DeviceType != "iPad" || results[2].DeviceType != "iPhone" || results[3].DeviceType != "iPad" {
			t.Errorf("workers %d: profiles out of order", workers)
		}
	}
}

func TestStatusNamesArePerCommand(t *testing.T) {
	if got := StatusName("FolderSync", 9); !strings.Contains(got, "SyncKeyMismatch") || strings.Contains(got, "OutOfSpace") {
		t.Errorf("FolderSync 9 = %q", got)
	}
	if got := StatusName("Sync", 9); got != "OutOfSpace" {
		t.Errorf("Sync 9 = %q", got)
	}
	if got := StatusName("Sync", 12); !strings.Contains(got, "FolderHierarchyChanged") {
		t.Errorf("Sync 12 = %q", got)
	}
	if got := StatusName("Ping", 5); !strings.Contains(got, "HeartbeatOutOfRange") {
		t.Errorf("Ping 5 = %q", got)
	}
	if got := StatusName("Ping", 7); !strings.Contains(got, "FolderHierarchySyncRequired") {
		t.Errorf("Ping 7 = %q", got)
	}
	if got := StatusName("Anything", 128); got != "DeviceIsBlockedForThisUser" {
		t.Errorf("common status = %q", got)
	}
	if got := StatusName("FolderSync", 77); got != "unknown status" {
		t.Errorf("unknown = %q", got)
	}

	// the library's error text is corrected, wrapping text kept
	err := fmt.Errorf("foldersync: %w", &eas.StatusError{Command: "FolderSync", Code: 9})
	text := ErrorText(err)
	if !strings.HasPrefix(text, "foldersync: ") || !strings.Contains(text, "SyncKeyMismatch") || strings.Contains(text, "OutOfSpace") {
		t.Errorf("ErrorText = %q", text)
	}
	if ErrorText(errors.New("plain")) != "plain" || ErrorText(nil) != "" {
		t.Error("other errors must pass through")
	}
}

func TestRecoveryWhenTheServerLostItsState(t *testing.T) {
	// The server's database is recreated: the policy key is gone (HTTP 449, which
	// the library repairs) and the stored folder and sync keys are stale
	// (FolderSync status 9). The probe must rebuild the hierarchy by itself.
	fake := easmockserver.New()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	store, _ := OpenStore("")

	cfg := baseCfg(srv.URL, store)
	cfg.Username = "nina@example.com"
	if res, _ := Run(t.Context(), cfg); !res.OK {
		t.Fatal("setup run failed")
	}

	fake.Reset()
	res, err := Run(t.Context(), cfg)
	var buf bytes.Buffer
	WriteText(&buf, res)
	if err != nil || !res.OK {
		t.Fatalf("a server that lost its state must not break the probe:\n%s", buf.String())
	}
	f := res.Step(StepFolderSync)
	if f == nil || !strings.Contains(f.Info["resync"], "status 9") {
		t.Errorf("the rebuild must be visible in the report: %+v", f)
	}
	if s := res.Step(StepSync); s == nil || !s.OK {
		t.Error("sync after the rebuild failed")
	}

	// and the next run is quiet again
	res, _ = Run(t.Context(), cfg)
	if !res.OK || strings.Contains(res.Step(StepFolderSync).Info["resync"], "status 9") {
		t.Error("after the recovery the keys are valid again")
	}
}

func TestSyncStatus12AndPingStatus7Recover(t *testing.T) {
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true})
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s, err := NewSession(sessionCfg(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) },
		func() error { return s.Provision(ctx, nil) },
		func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Sync(ctx, nil); err != nil {
		t.Fatal(err)
	}

	folderSyncs := fake.Count("FolderSync")
	fake.Inject("Sync", 12)
	st := &Step{}
	if _, err := s.Sync(ctx, st); err != nil || st.Info["resync"] == "" || fake.Count("FolderSync") != folderSyncs+1 {
		t.Errorf("Sync status 12 must run a FolderSync and retry: %v %v (FolderSyncs %d -> %d)", err, st.Info, folderSyncs, fake.Count("FolderSync"))
	}

	folderSyncs = fake.Count("FolderSync")
	fake.Inject("Ping", 7)
	st = &Step{}
	pr, err := s.Ping(ctx, 1, st)
	if err != nil || pr == nil || st.Info["resync"] == "" || fake.Count("FolderSync") != folderSyncs+1 {
		t.Errorf("Ping status 7 must run a FolderSync and retry: %v %v", err, st.Info)
	}
}

func TestPingHeartbeatLimitFromTheServer(t *testing.T) {
	// status 5 carries the allowed heartbeat, which is adopted (not 3 or 4)
	fake := easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true, MaxHeartbeat: 1})
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s, _ := NewSession(sessionCfg(srv.URL, nil))
	defer s.Close()
	ctx := t.Context()
	_ = s.Options(ctx, nil)
	_ = s.Provision(ctx, nil)
	_ = s.FolderSync(ctx, nil)

	st := &Step{}
	pr, err := s.Ping(ctx, 30, st)
	if err != nil {
		t.Fatalf("a heartbeat above the server's limit must be adapted: %v", err)
	}
	if !pr.HeartbeatChange || pr.HeartbeatUsed != 1 || st.Info["heartbeat_adjusted_to"] != "1" {
		t.Errorf("heartbeat adaptation: %+v %v", pr, st.Info)
	}

	// the mail push check works with such a server too
	cfg := baseCfg(srv.URL, nil)
	cfg.Username = "omar@example.com"
	cfg.Mail = true
	cfg.MailArrival = "push" // the experimental push mode
	cfg.MailWait = 10 * time.Second
	res, _ := Run(t.Context(), cfg)
	if m := res.Step(StepMail); m == nil || !m.OK || m.Info["push"] != "woke" {
		t.Errorf("push check with heartbeat limits: %+v", m)
	}
}
