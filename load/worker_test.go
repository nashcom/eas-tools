package load

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/herd"
	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/internal/testherd"
	"github.com/nashcom/eas-tools/probe"
)

type env struct {
	t     *testing.T
	herd  *testherd.Server
	eas   *easmockserver.Server
	easTS *httptest.Server
	hTS   *httptest.Server
	stats *Stats
}

func newEnv(t *testing.T, accounts int) *env {
	t.Helper()
	return newEnvWith(t, accounts, easmockserver.Options{})
}

// newEnvWith is newEnv with a configured mock ActiveSync server.
func newEnvWith(t *testing.T, accounts int, opts easmockserver.Options) *env {
	t.Helper()
	e := &env{t: t, herd: testherd.New(accounts, easmockserver.Password), eas: easmockserver.NewWithOptions(opts), stats: NewStats()}
	e.easTS = httptest.NewServer(e.eas)
	e.hTS = httptest.NewServer(e.herd)
	t.Cleanup(e.easTS.Close)
	t.Cleanup(e.hTS.Close)
	return e
}

// start runs a worker; the returned function cancels it and waits.
func (e *env) start(clients int, mutate func(*Config)) (done <-chan struct{}, stop func()) {
	e.t.Helper()

	cfg := Config{
		Herd:       herd.New(e.hTS.URL, ""),
		Name:       "t",
		Clients:    clients,
		Server:     e.easTS.URL,
		Poll:       20 * time.Millisecond,
		RetryDelay: 20 * time.Millisecond,
		Stats:      e.stats,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		_ = Run(ctx, cfg)
	}()
	stop = func() {
		cancel()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			e.t.Error("worker did not end")
		}
	}
	e.t.Cleanup(stop)
	return ch, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) last(testID int) testherd.Report {
	r, _ := e.herd.Last(testID)
	return r
}

func TestProbeJob(t *testing.T) {
	e := newEnv(t, 1)
	done, _ := e.start(1, nil)

	waitFor(t, "client reports idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "job result", func() bool {
		r := e.last(1)
		return r.LastJobID == id && r.LastJobResult == "ok" && r.State == "idle"
	})
	if r := e.last(1); r.LastJob != JobProbe || r.Ack != id {
		t.Errorf("report = %+v", r)
	}
	if e.eas.Count("Provision") == 0 || e.eas.Count("Sync") == 0 {
		t.Error("probe job must provision and sync")
	}

	e.herd.Command("stop", "", "", 0)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not end after stop")
	}
	if got := e.last(1).State; got != "done" {
		t.Errorf("final state = %q", got)
	}
	var sawStopping bool
	for _, r := range e.herd.Reports() {
		if r.State == "stopping" {
			sawStopping = true
		}
	}
	if !sawStopping {
		t.Error("client must report stopping before done")
	}
}

func TestSyncJobRunsUntilCommanded(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobSync, "think=0.02", 0)
	waitFor(t, "running with syncs", func() bool {
		r := e.last(1)
		return r.State == "running" && strings.Contains(r.Message, "syncs") && e.eas.Count("Sync") >= 5
	})

	// A newer idle ends the job as stopped and keeps the worker alive.
	e.herd.Command("idle", "", "", 0)
	waitFor(t, "job stopped", func() bool {
		r := e.last(1)
		return r.LastJobID == id && r.LastJobResult == "stopped" && r.State == "idle"
	})

	// ... and the same worker takes the next run.
	id2 := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "second job", func() bool { return e.last(1).LastJobID == id2 && e.last(1).LastJobResult == "ok" })
}

func TestSyncDurationEndsOK(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobSync, "think=0.05&duration=1", 0)
	waitFor(t, "job ok", func() bool {
		r := e.last(1)
		return r.LastJobID == id && r.LastJobResult == "ok"
	})
}

func TestBadJobsFailTheJobNotTheWorker(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	for _, c := range []struct{ job, params string }{
		{JobSync, "bogus=1"},
		{JobSync, "think=abc"},
		{JobProbe, "x=1"},
		{"mail", ""},
	} {
		id := e.herd.Command("run", c.job, c.params, 0)
		waitFor(t, "failed "+c.job+" "+c.params, func() bool {
			r := e.last(1)
			return r.LastJobID == id && r.LastJobResult == "failed"
		})
		if st := e.last(1).State; st != "idle" && st != "running" {
			t.Errorf("worker state after failed job = %q", st)
		}
	}
}

func TestPauseStopsWork(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	e.herd.Command("run", JobSync, "think=0.02", 0)
	waitFor(t, "syncing", func() bool { return e.eas.Count("Sync") >= 4 })

	pid := e.herd.Command("pause", "", "", 1)
	waitFor(t, "paused", func() bool { r := e.last(1); return r.State == "paused" && r.Ack == pid })

	// Let a request that was already under way finish, then nothing may
	// happen until the pause is over.
	time.Sleep(150 * time.Millisecond)
	before := e.eas.Count("Sync")
	time.Sleep(400 * time.Millisecond)
	if after := e.eas.Count("Sync"); after != before {
		t.Errorf("syncs during pause: %d -> %d", before, after)
	}

	waitFor(t, "running again", func() bool { return e.last(1).State == "running" })
	waitFor(t, "syncs resume", func() bool { return e.eas.Count("Sync") > before })
}

func TestWaitsForAccount(t *testing.T) {
	e := newEnv(t, 1)
	e.herd.ExhaustFirst(3)
	e.start(1, nil)
	waitFor(t, "registered after waiting", func() bool { return e.last(1).State == "idle" })
}

func TestCoordinatorRestartEndsClient(t *testing.T) {
	e := newEnv(t, 1)
	done, _ := e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	e.herd.Forget()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client must end when the coordinator forgot it")
	}
}

func TestTokenIsSent(t *testing.T) {
	e := newEnv(t, 1)
	e.herd.RequireToken("0123456789abcdef-token")
	e.start(1, func(c *Config) { c.Herd = herd.New(e.hTS.URL, "0123456789abcdef-token") })
	waitFor(t, "idle with token", func() bool { return e.last(1).State == "idle" })
}

func TestManyClients(t *testing.T) {
	const n = 8
	e := newEnv(t, n)
	e.start(n, func(c *Config) { c.Ramp = 5 * time.Millisecond })

	waitFor(t, "all idle", func() bool {
		for id := 1; id <= n; id++ {
			if e.last(id).State != "idle" {
				return false
			}
		}
		return true
	})

	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "all jobs ok", func() bool {
		for i := 1; i <= n; i++ {
			if r := e.last(i); r.LastJobID != id || r.LastJobResult != "ok" {
				return false
			}
		}
		return true
	})

	// Every client is its own device with its own registration.
	if got := e.eas.Count("Provision"); got != 2*n {
		t.Errorf("Provision requests = %d, want %d (two phases per client)", got, 2*n)
	}
}

func TestSameNameGetsSameAccounts(t *testing.T) {
	e := newEnv(t, 2)
	_, stop := e.start(2, nil)
	waitFor(t, "both idle", func() bool { return e.last(1).State == "idle" && e.last(2).State == "idle" })
	stop()

	// A restart with the same name must get the same test_ids back and
	// must not use up the (size 2) pool.
	e.start(2, nil)
	waitFor(t, "both idle again", func() bool {
		count := 0
		for _, r := range e.herd.Reports() {
			if r.State == "idle" {
				count++
			}
		}
		return count > 4
	})
}

func TestStatsCountRequests(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })
	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "ok", func() bool { return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" })

	var b strings.Builder
	e.stats.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		`eas_load_requests_total{op="Sync",result="ok"}`,
		`eas_load_requests_total{op="OPTIONS",result="ok"}`,
		`eas_load_jobs_total{job="eas-probe",result="ok"} 1`,
		`eas_load_request_duration_seconds_count{op="Provision"}`,
		`eas_load_clients{state="idle"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(e.stats.Summary(), "Sync") {
		t.Errorf("summary = %q", e.stats.Summary())
	}
}

func TestLoginShortname(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, func(c *Config) { c.Login = "shortname" })
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })
	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "ok", func() bool { return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" })
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no herd":    {Name: "x", Clients: 1, Server: "h"},
		"no server":  {Herd: herd.New("http://x", ""), Name: "x", Clients: 1},
		"no clients": {Herd: herd.New("http://x", ""), Name: "x", Server: "h"},
		"no name":    {Herd: herd.New("http://x", ""), Clients: 1, Server: "h"},
		"bad login":  {Herd: herd.New("http://x", ""), Name: "x", Clients: 1, Server: "h", Login: "x"},
	} {
		if err := Run(t.Context(), cfg); err == nil {
			t.Errorf("%s: config must be rejected", name)
		}
	}
}

// compile-time check that the store type used by the worker is the probe's.
var _ *probe.Store

func TestRefusedLoginEndsSyncJobAtOnce(t *testing.T) {
	e := newEnv(t, 1)
	// The coordinator hands out a password the ActiveSync server refuses.
	e.herd = testherd.New(1, "wrong-password")
	e.hTS.Close()
	e.hTS = httptest.NewServer(e.herd)
	t.Cleanup(e.hTS.Close)

	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobSync, "think=0.02", 0)
	waitFor(t, "failed", func() bool {
		r := e.last(1)
		return r.LastJobID == id && r.LastJobResult == "failed"
	})
	if !strings.Contains(e.last(1).Message, "refused") && !strings.Contains(e.last(1).Message, "401") && !strings.Contains(e.last(1).Message, "auth") {
		t.Logf("message: %q", e.last(1).Message)
	}
	// setup fails at the first request; the worker must not have hammered
	if got := e.stats.failures["auth"]; got > 2 {
		t.Errorf("auth failures = %d, a refused login must not be retried", got)
	}
}

func TestRefusedLoginIsInSummary(t *testing.T) {
	s := NewStats()
	s.Failure("auth")
	s.Failure("timeout")
	if s.LoginRefusals() != 1 {
		t.Errorf("refusals = %d", s.LoginRefusals())
	}
	if !strings.Contains(s.Summary(), "refused a login") || !strings.Contains(s.Summary(), "auth=1") {
		t.Errorf("summary = %q", s.Summary())
	}
}

func TestMultipleDevicesPerClient(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, func(c *Config) {
		c.Devices = 3
		c.Profiles = []string{"iphone", "ipad"}
	})
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "probe ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	if got := e.eas.DeviceCount(); got != 3 {
		t.Errorf("device ids seen by the server = %d, want 3 (one account, three devices)", got)
	}
	if got := e.eas.Count("Provision"); got != 6 {
		t.Errorf("Provision requests = %d, want 6 (two phases per device)", got)
	}
	if msg := e.last(1).Message; !strings.Contains(msg, "3 devices") {
		t.Errorf("result message = %q", msg)
	}
}

func TestSyncJobRunsOnAllDevices(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, func(c *Config) { c.Devices = 3 })
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobSync, "think=0.02", 0)
	waitFor(t, "all devices syncing", func() bool {
		r := e.last(1)
		return r.State == "running" && strings.Contains(r.Message, "d1 ") &&
			strings.Contains(r.Message, "d2 ") && strings.Contains(r.Message, "d3 ") && e.eas.DeviceCount() == 3
	})

	e.herd.Command("idle", "", "", 0)
	waitFor(t, "stopped", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "stopped" })
}

func TestFirstDeviceKeepsItsRegistration(t *testing.T) {
	e := newEnv(t, 1)
	store, _ := probe.OpenStore("")

	_, stop := e.start(1, func(c *Config) { c.Store = store })
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })
	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "first probe", func() bool { return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" })
	if got := e.eas.Count("Provision"); got != 2 {
		t.Fatalf("Provision after one device = %d, want 2", got)
	}
	stop()

	// The same account now with three devices: the first one is already
	// provisioned, only the two new devices register.
	e.start(1, func(c *Config) { c.Store = store; c.Devices = 3 })
	waitFor(t, "idle again", func() bool {
		n := 0
		for _, r := range e.herd.Reports() {
			if r.State == "idle" {
				n++
			}
		}
		return n > 3
	})
	id2 := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "second probe", func() bool { return e.last(1).LastJobID == id2 && e.last(1).LastJobResult == "ok" })

	if got := e.eas.Count("Provision"); got != 6 {
		t.Errorf("Provision after three devices = %d, want 6 (2 + two new devices)", got)
	}
	if got := e.eas.DeviceCount(); got != 3 {
		t.Errorf("devices = %d, want 3", got)
	}
}

func TestRefusedLoginWithManyDevicesStopsAll(t *testing.T) {
	e := newEnv(t, 1)
	e.herd = testherd.New(1, "wrong-password")
	e.hTS.Close()
	e.hTS = httptest.NewServer(e.herd)
	t.Cleanup(e.hTS.Close)

	e.start(1, func(c *Config) { c.Devices = 3 })
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobSync, "think=0.02", 0)
	waitFor(t, "failed", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "failed" })

	if got := e.stats.failures["auth"]; got > 3 {
		t.Errorf("auth failures = %d, each device may try once at most", got)
	}
}

func TestUnknownProfileIsRejected(t *testing.T) {
	cfg := Config{
		Herd: herd.New("http://x", ""), Name: "x", Clients: 1, Server: "h",
		Profiles: []string{"iphone", "toaster"},
	}
	if err := Run(t.Context(), cfg); err == nil {
		t.Error("unknown profile must be rejected")
	}
}

func TestGalJobFindsOwnUser(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobGal, "think=0.02", 0)
	waitFor(t, "own user found", func() bool {
		r := e.last(1)
		return r.State == "running" && strings.Contains(r.Message, "own user found") && e.stats.dir["found"] >= 3
	})
	if e.stats.dir["missing"] != 0 {
		t.Errorf("own user missing %d times, the fake directory has every logged in user", e.stats.dir["missing"])
	}
	if e.eas.Count("Search") < 3 {
		t.Errorf("Search requests = %d", e.eas.Count("Search"))
	}

	e.herd.Command("idle", "", "", 0)
	waitFor(t, "stopped", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "stopped" })
}

func TestGalTypeahead(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	e.herd.Command("run", JobGal, "mode=typeahead&think=0.05", 0)
	// load000001@example.com: prefixes of "load000001" from 3 to 9 letters,
	// then the full address, in every round
	waitFor(t, "a round of typing", func() bool { return e.eas.Count("Search") >= 8 })
	waitFor(t, "found the full address", func() bool { return e.stats.dir["found"] >= 1 })
	e.herd.Command("idle", "", "", 0)
}

func TestGalBadParams(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	for _, params := range []string{"mode=sing", "query=%20", "foo=1", "think=x"} {
		id := e.herd.Command("run", JobGal, params, 0)
		waitFor(t, "failed "+params, func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "failed" })
	}
}

func TestGalOnAllDevices(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, func(c *Config) { c.Devices = 2 })
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	e.herd.Command("run", JobGal, "think=0.02", 0)
	waitFor(t, "both devices looked up", func() bool {
		r := e.last(1)
		return strings.Contains(r.Message, "d1 ") && strings.Contains(r.Message, "d2 ") && e.stats.dir["found"] >= 4
	})
}

const user1 = "load000001@example.com"

func TestMailToSelfRoundTrip(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=3&wait=10", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	if msg := e.last(1).Message; !strings.Contains(msg, "3 sent, 3 arrived, 0 late, 0 lost") {
		t.Errorf("message = %q", msg)
	}
	sent, arrived, lost := e.stats.MailTotals()
	if sent != 3 || arrived != 3 || lost != 0 {
		t.Errorf("sent %d, arrived %d, lost %d", sent, arrived, lost)
	}
	if got := e.eas.MailboxCount(user1); got != 0 {
		t.Errorf("cleanup must delete the mails again, %d left", got)
	}

	var b strings.Builder
	e.stats.WritePrometheus(&b)
	for _, want := range []string{`eas_load_mail_sent_total{to="self"} 3`, "eas_load_mail_arrival_seconds_count 3", "eas_load_mail_lost_total 0"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestMailWithoutCleanupKeepsMail(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=2&wait=10&cleanup=no", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })
	if got := e.eas.MailboxCount(user1); got != 2 {
		t.Errorf("mails left = %d, want 2 without cleanup", got)
	}
}

func TestMailPollArrival(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=2&wait=10&arrival=poll", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })
	if _, arrived, _ := e.stats.MailTotals(); arrived != 2 {
		t.Errorf("arrived = %d, want 2", arrived)
	}
}

func TestLostMailIsCounted(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true, DropMail: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=1&wait=1", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	sent, arrived, lost := e.stats.MailTotals()
	if sent != 1 || arrived != 0 || lost != 1 {
		t.Errorf("sent %d, arrived %d, lost %d; want 1, 0, 1", sent, arrived, lost)
	}
	if !strings.Contains(e.last(1).Message, "1 lost") {
		t.Errorf("message = %q", e.last(1).Message)
	}
}

func TestLostMailDetailIsKept(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true, DropMail: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=1&wait=1", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	e.stats.mu.Lock()
	defer e.stats.mu.Unlock()
	if 1 != len(e.stats.missedMails) || 0 != e.stats.mailLate || 1 != e.stats.mailLost {
		t.Fatalf("lost mails %+v, late %d", e.stats.missedMails, e.stats.mailLate)
	}
	l := e.stats.missedMails[0]
	if l.Late || l.Polls < 1 || !strings.HasPrefix(l.Subject, "[easload ") || 1 != l.Device {
		t.Errorf("lost mail %+v", l)
	}
	if 0 == len(l.Timeline) || !strings.Contains(l.Timeline[0], "SendMail accepted") ||
		!strings.Contains(l.Timeline[len(l.Timeline)-1], "extra wait") {
		t.Errorf("timeline %v", l.Timeline)
	}
}

// A mail that is delivered after the wait time is lost, but it is also late: it is found in the extra wait and deleted.
func TestLateMailIsMarkedAndDeleted(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true, MailDelay: 1500 * time.Millisecond})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=1&wait=1", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	e.stats.mu.Lock()
	defer e.stats.mu.Unlock()
	if 1 != len(e.stats.missedMails) || 1 != e.stats.mailLate || 0 != e.stats.mailLost {
		t.Fatalf("lost mails %+v, late %d", e.stats.missedMails, e.stats.mailLate)
	}
	if l := e.stats.missedMails[0]; !l.Late || l.LateSeconds < 1 {
		t.Errorf("lost mail %+v", l)
	}
	// the server delivered it 1.5 s after SendMail: its DateReceived says so, not the time the device saw it
	if l := e.stats.missedMails[0]; nil == l.ReceivedAfter || *l.ReceivedAfter < 1.2 || *l.ReceivedAfter > 1.9 {
		t.Errorf("server received after %v, want about 1.5 s", l.ReceivedAfter)
	}
	if n := e.eas.MailboxCount(user1); 0 != n {
		t.Errorf("a late mail must be deleted again, the mailbox has %d", n)
	}
}

func TestMailToOthersUsesTheDirectory(t *testing.T) {
	e := newEnv(t, 2)
	e.start(2, nil)
	waitFor(t, "both idle", func() bool { return e.last(1).State == "idle" && e.last(2).State == "idle" })

	// both users have to be known to the (stand-in) directory: they log in
	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "both logged in", func() bool {
		return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" && e.last(2).LastJobID == id && e.last(2).LastJobResult == "ok"
	})

	id = e.herd.Command("run", JobMail, "to=others&think=0.05&count=2&cleanup=no", 0)
	waitFor(t, "mail jobs ok", func() bool {
		return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" && e.last(2).LastJobID == id && e.last(2).LastJobResult == "ok"
	})

	// every mail goes to the other account: 2 clients x 2 mails
	if got := e.eas.MailboxCount(user1) + e.eas.MailboxCount("load000002@example.com"); got != 4 {
		t.Errorf("mails in the two mailboxes = %d, want 4", got)
	}
	if got := e.eas.MailboxCount(user1); got != 2 {
		t.Errorf("mails in mailbox 1 = %d, want 2 (only from the other user)", got)
	}
}

func TestMailToExplicitList(t *testing.T) {
	e := newEnv(t, 2)
	e.start(2, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" && e.last(2).State == "idle" })
	id := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "both in", func() bool { return e.last(1).LastJobID == id && e.last(2).LastJobID == id })

	id = e.herd.Command("run", JobMail, "to=load000002@example.com&think=0.05&count=1&cleanup=no", 0)
	waitFor(t, "ok", func() bool { return e.last(1).LastJobID == id && e.last(1).LastJobResult == "ok" })
	if e.eas.MailboxCount("load000002@example.com") != 2 {
		t.Errorf("both clients send one mail to user 2, got %d", e.eas.MailboxCount("load000002@example.com"))
	}
}

func TestMailBadParams(t *testing.T) {
	e := newEnv(t, 1)
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	for _, params := range []string{"to=nobody", "arrival=smoke", "cleanup=maybe", "size=9-1", "wait=0", "count=x", "foo=1"} {
		id := e.herd.Command("run", JobMail, params, 0)
		waitFor(t, "failed "+params, func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "failed" })
	}
	if sent, _, _ := e.stats.MailTotals(); sent != 0 {
		t.Errorf("nothing may be sent for bad parameters, sent %d", sent)
	}
}

// sim=... in the parameters of a job is sent as the X-Mock-Sim header for the
// length of that job only: the same worker runs the next job normally.
func TestSimParameterAppliesToTheJobOnly(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{Sim: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	bad := e.herd.Command("run", JobProbe, "sim=cmd=FolderSync,http=502", 0)
	waitFor(t, "failed job", func() bool {
		r := e.last(1)
		return r.LastJobID == bad && r.LastJobResult == "failed" && r.State == "idle"
	})
	if e.eas.SimHits() == 0 {
		t.Error("the mock server never saw the simulation header")
	}
	if msg := e.last(1).Message; !strings.Contains(msg, "502") && !strings.Contains(msg, "server_error") {
		t.Errorf("the failure must name the simulated error, got %q", msg)
	}

	hits := e.eas.SimHits()
	good := e.herd.Command("run", JobProbe, "", 0)
	waitFor(t, "next job ok", func() bool {
		r := e.last(1)
		return r.LastJobID == good && r.LastJobResult == "ok" && r.State == "idle"
	})
	if e.eas.SimHits() != hits {
		t.Error("the header must be removed after the job")
	}
}

func TestSplitSim(t *testing.T) {
	rest, sim, err := splitSim("think=10-30&sim=cmd=Sync,status=3,count=1")
	if err != nil || sim != "cmd=Sync;status=3;count=1" || rest != "think=10-30" {
		t.Errorf("got %q, %q, %v", rest, sim, err)
	}
	if rest, sim, _ := splitSim("think=1-2"); rest != "think=1-2" || sim != "" {
		t.Errorf("without sim: %q, %q", rest, sim)
	}
}

func TestMailJobDeletesTheCopiesInSentItems(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true, KeepSentCopy: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=2", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	if n := e.eas.SentboxCount(user1); 0 != n {
		t.Errorf("the copies in Sent Items must be deleted at the end: %d left", n)
	}
	e.stats.mu.Lock()
	defer e.stats.mu.Unlock()
	if 2 != e.stats.sentDeleted || 0 != e.stats.sentFailed {
		t.Errorf("deleted %d, failed %d", e.stats.sentDeleted, e.stats.sentFailed)
	}
	if !strings.Contains(e.last(1).Message, "2 copies deleted from Sent Items") {
		t.Errorf("message = %q", e.last(1).Message)
	}
}

func TestMailJobKeepsTheCopiesWithSaveSent(t *testing.T) {
	e := newEnvWith(t, 1, easmockserver.Options{HoldPing: true, KeepSentCopy: true})
	e.start(1, nil)
	waitFor(t, "idle", func() bool { return e.last(1).State == "idle" })

	id := e.herd.Command("run", JobMail, "to=self&think=0.05&count=2&save_sent=yes", 0)
	waitFor(t, "mail job ok", func() bool { r := e.last(1); return r.LastJobID == id && r.LastJobResult == "ok" })

	if n := e.eas.SentboxCount(user1); 2 != n {
		t.Errorf("with save_sent=yes the copies stay: %d", n)
	}
}
