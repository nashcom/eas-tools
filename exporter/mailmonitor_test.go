package exporter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

const mailMonitorBase = `{"targets": [{"name": "lab","server": "https://eas.example.org", "user": "me@example.org", "password": "x"}],
 "mail_monitor": %s}`

func loadMailMonitor(t *testing.T, rm string) (*Config, error) {
	t.Helper()
	return LoadConfigBytes([]byte(strings.Replace(mailMonitorBase, "%s", rm, 1)), "test", nil)
}

func TestMailMonitorDefaults(t *testing.T) {
	c, err := loadMailMonitor(t, `[{"name": "ext", "to": "echo@example.net"}]`)
	if err != nil {
		t.Fatal(err)
	}
	m := c.MailMonitor["ext"]
	if m.Account != "lab" || m.IntervalSeconds != 600 || m.TimeoutSeconds != 300 || c.MailMonitorPollSeconds != 2 {
		t.Fatalf("%+v poll %d", m, c.MailMonitorPollSeconds)
	}
}

func TestMailMonitorIsRefusedWhenWrong(t *testing.T) {
	for name, rm := range map[string]string{
		"no address":        `[{"name": "ext", "to": ""}]`,
		"two addresses":     `[{"name": "ext", "to": "a@example.net, b@example.net"}]`,
		"unknown account":   `[{"name": "ext", "to": "a@example.net", "account": "nope"}]`,
		"too short":         `[{"name": "ext", "to": "a@example.net", "interval_seconds": 10}]`,
		"timeout too small": `[{"name": "ext", "to": "a@example.net", "timeout_seconds": 2}]`,
		"bad name":          `[{"name": "e x", "to": "a@example.net"}]`,
		"unknown key":       `[{"name": "ext", "to": "a@example.net", "every": 5}]`,
	} {
		if _, err := loadMailMonitor(t, rm); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMailMonitorAccountNeedsServerAndAddress(t *testing.T) {
	_, err := LoadConfigBytes([]byte(`{"targets": [{"name": "a","email": "me@example.org", "password": "x"}, {"name": "b","server": "h", "user": "shortname", "password": "x"}],
 "mail_monitor": [{"name": "ext","to": "a@example.net", "account": "a"}]}`), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Errorf("no server: %v", err)
	}
	_, err = LoadConfigBytes([]byte(`{"targets": [{"name": "a","email": "me@example.org", "password": "x"}, {"name": "b","server": "h", "user": "shortname", "password": "x"}],
 "mail_monitor": [{"name": "ext","to": "a@example.net", "account": "b"}]}`), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "e-mail address") {
		t.Errorf("no address: %v", err)
	}
	// with several targets the account has to be named
	_, err = LoadConfigBytes([]byte(`{"targets": [{"name": "a","server": "h", "email": "me@example.org", "password": "x"}, {"name": "b","server": "h", "email": "me@example.org", "password": "x"}],
 "mail_monitor": [{"name": "ext","to": "a@example.net"}]}`), "test", nil)
	if err == nil {
		t.Error("account left out with two targets")
	}
}

const mailUser = "mia@example.com"

// The mock server delivers a mail to the mailbox of a known user, so the account sends to its own address: the mail is
// its own answer. That runs the whole path: session, SendMail, Inbox, round trip, metrics and cleanup.
func TestMailMonitorRoundTripAgainstTheMockServer(t *testing.T) {
	srv, front, fake := newTestServer(t, func(c *Config) {
		t := c.Targets["lab"]
		t.User = mailUser
		c.Targets["lab"] = t
		c.MailMonitorPollSeconds = 1
		c.MailMonitor = map[string]MailMonitorTarget{
			"loop": {To: mailUser, Account: "lab", IntervalSeconds: 3600, TimeoutSeconds: 60},
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunMailMonitor(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var body string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, body = get(t, front.URL+"/metrics")
		if strings.Contains(body, `eas_probe_mail_monitor_received_total{mail_monitor="loop"} 1`) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, want := range []string{
		`eas_probe_mail_monitor_sent_total{mail_monitor="loop"} 1`,
		`eas_probe_mail_monitor_received_total{mail_monitor="loop"} 1`,
		`eas_probe_mail_monitor_lost_total{mail_monitor="loop"} 0`,
		`eas_probe_mail_monitor_up{mail_monitor="loop"} 1`,
		`eas_probe_mail_monitor_rtt_seconds{mail_monitor="loop",window="5m"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if n := fake.MailboxCount(mailUser); n != 0 {
		t.Errorf("the answer was not deleted: %d mails in the mailbox", n)
	}
}

func TestReloadChangesTheMailMonitorTargets(t *testing.T) {
	srv, front, _ := newTestServer(t, nil)
	_, body := get(t, front.URL+"/metrics")
	if strings.Contains(body, "mail_monitor") {
		t.Fatal("mail monitor metrics without a target")
	}
	cfg := *srv.cur.Load().cfg
	cfg.MailMonitorPollSeconds = 2
	cfg.MailMonitor = map[string]MailMonitorTarget{"ext": {To: "a@example.net", Account: "lab", IntervalSeconds: 600, TimeoutSeconds: 300}}
	if _, err := srv.Reload(&cfg); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, front.URL+"/metrics")
	if !strings.Contains(body, `eas_probe_mail_monitor_sent_total{mail_monitor="ext"} 0`) {
		t.Fatalf("target not there after the reload:\n%s", body)
	}
}

func TestMailMonitorExampleLoads(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "deploy", "exporter-mail-monitor.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m := c.MailMonitor["partner-domain"]; m.Account != "traveler" || m.IntervalSeconds != 600 {
		t.Errorf("%+v", m)
	}
}

func TestTargetsAndMailMonitorAreLists(t *testing.T) {
	lab := `{"name": "lab", "server": "h", "user": "u@example.org", "password": "x"}`
	for name, doc := range map[string]string{
		"targets as an object":      `{"targets": {"lab": {"server": "h", "user": "u@example.org", "password": "x"}}}`,
		"target without a name":     `{"targets": [{"server": "h", "user": "u@example.org", "password": "x"}]}`,
		"target with an empty name": `{"targets": [{"name": "", "server": "h", "user": "u@example.org", "password": "x"}]}`,
		"a name used twice":         `{"targets": [` + lab + `,` + lab + `]}`,
		"unknown key in an entry":   `{"targets": [{"name": "lab", "server": "h", "user": "u@example.org", "password": "x", "bogus": 1}]}`,
		"mail_monitor as an object": `{"targets": [` + lab + `], "mail_monitor": {"ext": {"to": "a@example.net"}}}`,
		"mail_monitor name twice":   `{"targets": [` + lab + `], "mail_monitor": [{"name": "e", "to": "a@example.net"}, {"name": "e", "to": "b@example.net"}]}`,
	} {
		_, err := LoadConfigBytes([]byte(doc), "test", nil)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := LoadConfigBytes([]byte(`{"targets": [`+lab+`, {"name": "two", "server": "h", "user": "v@example.org", "password": "y"}]}`), "test", nil)
	if err != nil || len(c.Targets) != 2 || c.Targets["two"].User != "v@example.org" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestMailMonitorOwnAccount(t *testing.T) {
	own := `"server": "h", "user": "me@example.org", "password": "x"`
	c, err := LoadConfigBytes([]byte(`{"targets": [{"name": "a", "server": "h", "user": "u@example.org", "password": "x"}, {"name": "b", "server": "h", "user": "v@example.org", "password": "y"}],
 "mail_monitor": [{"name": "own", "to": "a@example.net", `+own+`}, {"name": "ref", "to": "a@example.net", "account": "b"}]}`), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	k1, t1 := c.monitorAccount("own", c.MailMonitor["own"])
	k2, t2 := c.monitorAccount("ref", c.MailMonitor["ref"])
	if k1 != "monitor:own" || t1.User != "me@example.org" || k2 != "b" || t2.User != "v@example.org" {
		t.Errorf("%s %+v / %s %+v", k1, t1, k2, t2)
	}

	for name, rm := range map[string]string{
		"account and own fields":  `[{"name": "e", "to": "a@example.net", "account": "a", ` + own + `}]`,
		"own without a password":  `[{"name": "e", "to": "a@example.net", "server": "h", "user": "me@example.org"}]`,
		"own without a server":    `[{"name": "e", "to": "a@example.net", "user": "me@example.org", "password": "x"}]`,
		"own without an address":  `[{"name": "e", "to": "a@example.net", "server": "h", "user": "me", "password": "x"}]`,
		"two targets, no account": `[{"name": "e", "to": "a@example.net"}]`,
	} {
		_, err := LoadConfigBytes([]byte(`{"targets": [{"name": "a", "server": "h", "user": "u@example.org", "password": "x"}, {"name": "b", "server": "h", "user": "v@example.org", "password": "y"}], "mail_monitor": `+rm+`}`), "test", nil)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMailMonitorWithAnAccountOfItsOwnAgainstTheMockServer(t *testing.T) {
	srv, front, fake := newTestServer(t, func(c *Config) {
		c.MailMonitorPollSeconds = 1
		lab := c.Targets["lab"]
		c.MailMonitor = map[string]MailMonitorTarget{
			"solo": {To: mailUser, IntervalSeconds: 3600, TimeoutSeconds: 60,
				MailMonitorAccount: MailMonitorAccount{Server: lab.Server, User: mailUser, PasswordEnv: "TEST_EAS_PASSWORD"}},
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunMailMonitor(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var body string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		_, body = get(t, front.URL+"/metrics")
		if strings.Contains(body, `eas_probe_mail_monitor_received_total{mail_monitor="solo"} 1`) {
			break
		}
	}
	if !strings.Contains(body, `eas_probe_mail_monitor_received_total{mail_monitor="solo"} 1`) {
		t.Fatalf("no answer:\n%s", body)
	}
	if fake.MailboxCount(mailUser) != 0 {
		t.Error("the answer was not deleted")
	}
}

func TestAChangeOfTheMailMonitorCountsAsAChangedConfiguration(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	cfg := *srv.cur.Load().cfg
	if !srv.SameConfig(&cfg) {
		t.Fatal("the same configuration is reported as changed")
	}
	cfg.MailMonitor = map[string]MailMonitorTarget{"ext": {To: "a@example.net", Account: "lab", IntervalSeconds: 600, TimeoutSeconds: 300}}
	if srv.SameConfig(&cfg) {
		t.Fatal("a new mail monitor entry is not seen as a change")
	}
	if _, err := srv.Reload(&cfg); err != nil || !srv.SameConfig(&cfg) {
		t.Fatalf("reload: %v", err)
	}
	changed := cfg
	changed.MailMonitor = map[string]MailMonitorTarget{"ext": {To: "a@example.net", Account: "lab", IntervalSeconds: 120, TimeoutSeconds: 300}}
	if srv.SameConfig(&changed) {
		t.Fatal("a changed interval is not seen as a change")
	}
}

func TestDebugStateFollowsTheTokenOfProbe(t *testing.T) {
	// without a configured token the request is open, like /probe
	_, front, _ := newTestServer(t, nil)
	if code, body := get(t, front.URL+"/debug/state"); code != 200 || !strings.Contains(body, `"exporter"`) {
		t.Errorf("no token configured: %d %s", code, body)
	}

	_, front, _ = newTestServer(t, func(c *Config) { c.AuthToken = "s3cret" })
	if code, _ := get(t, front.URL+"/debug/state"); code != 401 {
		t.Errorf("no token sent: %d", code)
	}
	if code, _ := get(t, front.URL+"/debug/state", "Authorization", "Bearer wrong"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	code, body := get(t, front.URL+"/debug/state", "Authorization", "Bearer s3cret")
	if code != 200 {
		t.Fatalf("with the token: %d %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	for _, k := range []string{"time", "exporter", "targets", "mail_monitor"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%q is missing", k)
		}
	}
	if strings.Contains(body, "s3cret") || strings.Contains(body, easmockserver.Password) {
		t.Error("the answer shows a secret")
	}
	// it is not part of /metrics
	if _, metrics := get(t, front.URL+"/metrics"); strings.Contains(metrics, "samples") || strings.Contains(metrics, "debug") {
		t.Error("debug values in /metrics")
	}
}

func TestDebugStateShowsTheValuesOfTheMailMonitor(t *testing.T) {
	srv, front, _ := newTestServer(t, func(c *Config) {
		c.AuthToken = "s3cret"
		c.MailMonitorPollSeconds = 1
		t := c.Targets["lab"]
		t.User = mailUser
		c.Targets["lab"] = t
		c.MailMonitor = map[string]MailMonitorTarget{
			"loop": {To: mailUser, Account: "lab", IntervalSeconds: 3600, TimeoutSeconds: 60},
		}
	})
	srv.DebugSection("log", func() any { return map[string]string{"level": "info"} })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.RunMailMonitor(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var got struct {
		Log         map[string]string `json:"log"`
		MailMonitor struct {
			Targets map[string]struct {
				Received int64 `json:"received_total"`
				Samples  []struct {
					SentUnixNano     string  `json:"sent_unix_nano"`
					ReceivedUnixNano string  `json:"received_unix_nano"`
					SentTime         string  `json:"sent_time"`
					ReceivedTime     string  `json:"received_time"`
					RTTSeconds       float64 `json:"rtt_seconds"`
				} `json:"samples"`
				Windows []struct {
					Window string `json:"window"`
					Count  int    `json:"count"`
				} `json:"windows"`
			} `json:"targets"`
			Accounts map[string]struct {
				Open bool `json:"open"`
			} `json:"accounts"`
		} `json:"mail_monitor"`
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		_, body := get(t, front.URL+"/debug/state", "Authorization", "Bearer s3cret")
		got.MailMonitor.Targets = nil
		_ = json.Unmarshal([]byte(body), &got)
		if got.MailMonitor.Targets["loop"].Received == 1 {
			break
		}
	}
	m := got.MailMonitor.Targets["loop"]
	if m.Received != 1 || len(m.Samples) != 1 || m.Samples[0].ReceivedTime == "" {
		t.Fatalf("no sample in the debug state: %+v", m)
	}
	// every time is there in both forms: Unix nanoseconds and a readable time, and they say the same
	s := m.Samples[0]
	sent, err1 := strconv.ParseInt(s.SentUnixNano, 10, 64)
	recv, err2 := strconv.ParseInt(s.ReceivedUnixNano, 10, 64)
	if err1 != nil || err2 != nil || recv < sent || time.Duration(recv-sent) > time.Minute {
		t.Errorf("nanosecond times: sent %q received %q", s.SentUnixNano, s.ReceivedUnixNano)
	}
	if at, err := time.Parse(time.RFC3339Nano, s.SentTime); err != nil || at.UnixNano() != sent {
		t.Errorf("sent_time %q is not %d: %v", s.SentTime, sent, err)
	}
	if at, err := time.Parse(time.RFC3339Nano, s.ReceivedTime); err != nil || at.UnixNano() != recv {
		t.Errorf("received_time %q is not %d: %v", s.ReceivedTime, recv, err)
	}
	if len(m.Windows) != 4 || m.Windows[0].Window != "5m" || m.Windows[0].Count != 1 {
		t.Errorf("windows: %+v", m.Windows)
	}
	if !got.MailMonitor.Accounts["lab"].Open {
		t.Errorf("accounts: %+v", got.MailMonitor.Accounts)
	}
	if got.Log["level"] != "info" {
		t.Errorf("the section of the command is missing: %+v", got.Log)
	}
}
