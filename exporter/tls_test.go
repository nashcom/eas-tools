package exporter

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/internal/certreload"
)

func TestTLSSettingsOfTheConfig(t *testing.T) {
	doc := `{"tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", "tls_generate": true, "tls_names": ["a.example.com", "10.0.0.1"], "tls_check_interval_seconds": 5,
	 "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`
	cfg, err := LoadConfigEnv(writeConfig(t, doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSCert != "/c/tls.crt" || cfg.TLSKey != "/c/tls.key" || !cfg.TLSGenerate || strings.Join(cfg.TLSNames, ",") != "a.example.com,10.0.0.1" || cfg.TLSCheckSeconds != 5 {
		t.Errorf("config = %+v", cfg)
	}

	// the environment: the same keys, lists with commas
	cfg, err = LoadConfigEnv(writeConfig(t, strings.Replace(doc, `"tls_check_interval_seconds": 5,`, "", 1)),
		[]string{"EASEXPORTER_TLS_NAMES=b.example.com, 10.9.9.9", "EASEXPORTER_TLS_CHECK_INTERVAL_SECONDS=120"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.TLSNames, ",") != "b.example.com,10.9.9.9" || cfg.TLSCheckSeconds != 120 {
		t.Errorf("from the environment: %v %d", cfg.TLSNames, cfg.TLSCheckSeconds)
	}
	// the default of the interval
	cfg, _ = LoadConfigEnv(writeConfig(t, strings.Replace(doc, `"tls_check_interval_seconds": 5,`, "", 1)), nil)
	if cfg.TLSCheckSeconds != 60 {
		t.Errorf("the default interval is %d, want 60", cfg.TLSCheckSeconds)
	}

	for name, bad := range map[string]string{
		"cert only":            `{"tls_cert": "/c/tls.crt", "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`,
		"key only":             `{"tls_key": "/c/tls.key", "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`,
		"generate without TLS": `{"tls_generate": true, "allow_http": true, "targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]}`,
	} {
		if _, err := LoadConfigEnv(writeConfig(t, bad), nil); err == nil || !strings.Contains(err.Error(), "tls_") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMetricsPortSettings(t *testing.T) {
	targets := `"targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]`
	tls := `"tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", `
	load := func(doc string, env ...string) (*Config, error) { return LoadConfigEnv(writeConfig(t, doc), env) }

	// the metrics port is there always, with TLS and without: open (all interfaces) and on 9100
	for name, c := range map[string][2]string{
		"tls":             {`{` + tls + `"listen": ":9443", ` + targets + `}`, ":9100"}, // fixed, not the port after listen
		"tls, default":    {`{` + tls + targets + `}`, ":9100"},
		"no tls":          {`{"listen": "127.0.0.1:9000", ` + targets + `}`, ":9100"},
		"no tls, default": {`{` + targets + `}`, ":9100"},
		"named":           {`{"listen_metrics": "127.0.0.1:9500", ` + targets + `}`, "127.0.0.1:9500"},
		"named, no list":  {`{"listen_metrics": "0.0.0.0:9500", ` + targets + `}`, "0.0.0.0:9500"}, // open by default, the list restricts it
	} {
		if cfg, err := load(c[0]); err != nil || cfg.ListenMetrics != c[1] {
			t.Errorf("%s: %q %v, want %q", name, cfg.ListenMetrics, err, c[1])
		}
	}
	for name, doc := range map[string]string{
		"the port of the main one": `{"listen": ":9443", "listen_metrics": ":9443", ` + targets + `}`,
		"a wrong entry in a list":  `{"allow_from_metrics": ["nonsense"], ` + targets + `}`,
		"an address without port":  `{"listen_metrics": "nonsense", ` + targets + `}`,
		"TLS and plain HTTP":       `{` + tls + `"allow_http": true, ` + targets + `}`,
	} {
		if _, err := load(doc); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// the list restricts the metrics port; from the environment as well
	cfg, err := load(`{`+targets+`}`, "EASEXPORTER_LISTEN_METRICS=0.0.0.0:9117", "EASEXPORTER_ALLOW_FROM_METRICS=10.1.2.0/24, 10.1.1.1", "EASEXPORTER_ALLOW_HTTP=true")
	if err != nil || cfg.ListenMetrics != "0.0.0.0:9117" || strings.Join(cfg.AllowFromMetrics, ",") != "10.1.2.0/24,10.1.1.1" || !cfg.AllowHTTP {
		t.Errorf("from the environment: %+v %v", cfg, err)
	}
}

// Plain HTTP for everything is a choice, except on the loopback interface: this is the check of the start, not of the loading.
func TestPlainHTTPIsAChoice(t *testing.T) {
	for _, c := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{Listen: ":9116"}, false},
		{Config{Listen: ":9116", AllowHTTP: true}, true},
		{Config{Listen: ":9116", TLSCert: "c", TLSKey: "k"}, true},
		{Config{Listen: "127.0.0.1:9116"}, true},
	} {
		if err := c.cfg.CheckListen(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.cfg, err)
		}
	}
}

// The metrics port is for the metrics: no probe (the token would cross the network in clear text), no debug state, no reload.
func TestMetricsHandlerServesOnlyMetricsAndHealth(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	get := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		srv.MetricsHandler().ServeHTTP(rec, req)
		return rec.Code
	}
	for _, path := range []string{"/health", "/metrics"} {
		if code := get("GET", path); code != 200 {
			t.Errorf("GET %s on the plain listener = %d", path, code)
		}
	}
	for _, c := range [][2]string{{"GET", "/probe?target=lab"}, {"GET", "/debug/state"}, {"POST", "/config/reload"}, {"GET", "/config/reload"}} {
		if code := get(c[0], c[1]); code != 404 {
			t.Errorf("%s %s on the plain listener = %d, want 404", c[0], c[1], code)
		}
		// the main listener has the route (whatever it answers, it is not the 404 of a route that does not exist)
		req := httptest.NewRequest(c[0], c[1], nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == 404 {
			t.Errorf("%s %s on the main listener = 404", c[0], c[1])
		}
	}
}

// The metrics port has a list of its own: Prometheus may use it and not the main port, and the other way round.
func TestMetricsHandlerHasItsOwnList(t *testing.T) {
	srv, _, _ := newTestServer(t, func(c *Config) {
		c.AllowFrom = []string{"192.168.0.0/16"}
		c.AllowFromMetrics = []string{"10.1.2.0/24"}
	})
	get := func(h http.Handler, remote string) int {
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	main, plain := srv.Handler(), srv.MetricsHandler()
	for _, c := range []struct {
		h      http.Handler
		remote string
		want   int
	}{
		{main, "192.168.1.1:1", 200}, {main, "10.1.2.5:1", 403},
		{plain, "10.1.2.5:1", 200}, {plain, "192.168.1.1:1", 403}, {plain, "203.0.113.9:1", 403},
		{main, "127.0.0.1:1", 200}, {plain, "127.0.0.1:1", 200}, // loopback: the health check
	} {
		if got := get(c.h, c.remote); got != c.want {
			t.Errorf("%s: %d, want %d", c.remote, got, c.want)
		}
	}
}

func TestMetricsAndStateShowTheCertificate(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	store, err := certreload.New(o.CertFile, o.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, _ := newTestServer(t, nil)

	if m := request(srv, "GET", "/metrics", "127.0.0.1:1").Body.String(); strings.Contains(m, "tls_certificate_not_after") {
		t.Error("TLS metrics without a certificate")
	}
	srv.SetTLS(store)

	m := request(srv, "GET", "/metrics", "127.0.0.1:1").Body.String()
	for _, want := range []string{"eas_probe_tls_certificate_not_after_timestamp_seconds ", "eas_probe_tls_reloads_total 0\n", "eas_probe_tls_reload_errors_total 0\n"} {
		if !strings.Contains(m, want) {
			t.Errorf("the metrics lack %q:\n%s", want, m)
		}
	}
	state := request(srv, "GET", "/debug/state", "127.0.0.1:1").Body.String()
	for _, want := range []string{`"tls"`, "eas.example.com", `"not_after"`, `"sha256"`, `"reload_errors": 0`} {
		if !strings.Contains(state, want) {
			t.Errorf("the debug state lacks %s:\n%s", want, state)
		}
	}
	if strings.Contains(state, "PRIVATE KEY") {
		t.Error("the debug state shows a key")
	}
}

// The start writes MainRoutes and MetricsRoutes to the log: they are the routes that are there, and everything else is a 404.
func TestTheRoutesOfTheLogAreTheRoutesThatAreThere(t *testing.T) {
	srv, _, _ := newTestServer(t, nil)
	status := func(h http.Handler, path string) int {
		req := httptest.NewRequest("POST", path, nil) // /probe answers 405 to it, where without a target it would say 404
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	listed := func(paths []string, p string) bool {
		for _, x := range paths {
			if x == p {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		name  string
		h     http.Handler
		paths []string
	}{{"main", srv.Handler(), MainRoutes}, {"metrics", srv.MetricsHandler(), MetricsRoutes}} {
		for _, p := range MainRoutes {
			if got := status(c.h, p); (got != 404) != listed(c.paths, p) {
				t.Errorf("%s: %s answers %d but the list says %v", c.name, p, got, listed(c.paths, p))
			}
		}
	}
}

func TestEndpointsSayWhatIsNeededAndNeverTheToken(t *testing.T) {
	protected := []string{"/probe", "/debug/state", "/config/reload"}
	// word and detail by path: the word is on the line of the start, the detail at debug level
	by := func(c *Config, metricsPort bool) (map[string]string, map[string]string) {
		word, detail := map[string]string{}, map[string]string{}
		for _, e := range c.Endpoints(metricsPort) {
			word[e.Path], detail[e.Path] = e.Auth, e.Detail
		}
		return word, detail
	}
	word, detail := by(&Config{}, false)
	if len(word) != len(MainRoutes) || detail["/probe"] == "" || !strings.Contains(detail["/probe"], "no token") {
		t.Errorf("no token: %v %v", word, detail)
	}
	for _, p := range MainRoutes {
		if word[p] != "none" {
			t.Errorf("no token: %s is %q", p, word[p])
		}
	}

	word, detail = by(&Config{AuthToken: "s3cret-value"}, false)
	for _, p := range protected {
		if word[p] != "token" || !strings.Contains(detail[p], "Authorization: Bearer") || strings.Contains(detail[p], "s3cret") {
			t.Errorf("%s with a token: %q %q", p, word[p], detail[p])
		}
	}
	if word["/metrics"] != "none" || word["/health"] != "none" {
		t.Errorf("the metrics and the health check need no token: %v", word)
	}

	if word, _ := by(&Config{AuthToken: "s3cret-value"}, true); len(word) != len(MetricsRoutes) || word["/probe"] != "" || word["/metrics"] != "none" {
		t.Errorf("the metrics port has its own two endpoints: %v", word)
	}
	if w, d := by(&Config{AuthToken: "s3cret-value", AuthTokenName: "X-Probe-Token"}, false); w["/probe"] != "token" ||
		!strings.Contains(d["/probe"], "X-Probe-Token: <token>") || strings.Contains(d["/probe"], "s3cret") {
		t.Errorf("a token with a name: %q %q", w["/probe"], d["/probe"])
	}
	if w, d := by(&Config{AuthTokenFile: "/nonexistent/token"}, false); w["/probe"] != "token-error" || !strings.Contains(d["/probe"], "cannot be read") {
		t.Errorf("a file that is not there: %q %q", w["/probe"], d["/probe"])
	}
}

// The main port follows the mode, with the well-known ports: 9117 for HTTPS, 9116 for plain HTTP.
func TestTheDefaultPortOfTheMainListenerFollowsTheMode(t *testing.T) {
	targets := `"targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]`
	for name, c := range map[string][2]string{
		"certificate":       {`{"tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", ` + targets + `}`, ":9117"},
		"nothing named":     {`{` + targets + `}`, ":9117"},                              // TLS is the default
		"allow_http":        {`{"allow_http": true, ` + targets + `}`, "127.0.0.1:9116"}, // unencrypted is loopback unless named
		"allow_http, named": {`{"allow_http": true, "listen": ":9116", ` + targets + `}`, ":9116"},
		"named, whatever":   {`{"listen": "127.0.0.1:9000", "tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", ` + targets + `}`, "127.0.0.1:9000"},
		"named, no cert":    {`{"listen": ":8080", ` + targets + `}`, ":8080"},
	} {
		cfg, err := LoadConfigEnv(writeConfig(t, c[0]), nil)
		if err != nil || cfg.Listen != c[1] {
			t.Errorf("%s: %q %v, want %q", name, cfg.Listen, err, c[1])
		}
	}
	// the metrics port is its own: the main listener cannot take it
	if _, err := LoadConfigEnv(writeConfig(t, `{"listen": ":9100", `+targets+`}`), nil); err == nil {
		t.Error("the main listener on the metrics port was accepted")
	}
}

// TLS is the default: without a certificate named, without the choice of plain HTTP and off the loopback, the certificate is looked for in
// /run/secrets/tls (tls.crt, tls.key) and the start makes it when it is not there.
func TestTLSIsTheDefault(t *testing.T) {
	targets := `"targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]`
	cert, key := certreload.DefaultFiles()
	for name, c := range map[string]struct {
		doc           string
		auto, tls, mk bool
	}{
		"nothing named":         {`{` + targets + `}`, true, true, true},
		"a network address":     {`{"listen": ":8443", ` + targets + `}`, true, true, true},
		"plain HTTP chosen":     {`{"allow_http": true, ` + targets + `}`, false, false, false},
		"loopback":              {`{"listen": "127.0.0.1:9116", ` + targets + `}`, false, false, false},
		"named files":           {`{"tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", ` + targets + `}`, false, true, false},
		"named files, generate": {`{"tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", "tls_generate": true, ` + targets + `}`, false, true, true},
	} {
		cfg, err := LoadConfigEnv(writeConfig(t, c.doc), nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg.TLSAuto != c.auto || (cfg.TLSCert != "") != c.tls || cfg.MakesCertificate() != c.mk {
			t.Errorf("%s: auto %v, tls %v, makes %v; want %v %v %v", name, cfg.TLSAuto, cfg.TLSCert != "", cfg.MakesCertificate(), c.auto, c.tls, c.mk)
		}
		if c.auto && (cfg.TLSCert != cert || cfg.TLSKey != key) {
			t.Errorf("%s: the files are %q %q, want the default ones %q %q", name, cfg.TLSCert, cfg.TLSKey, cert, key)
		}
	}
	// only one of the two files named is a mistake, the default does not fill the other
	if _, err := LoadConfigEnv(writeConfig(t, `{"tls_cert": "/c/tls.crt", `+targets+`}`), nil); err == nil {
		t.Error("a certificate without a key")
	}
	// from the environment: the same
	cfg, err := LoadConfigEnv(writeConfig(t, `{`+targets+`}`), []string{"EASEXPORTER_ALLOW_HTTP=true"})
	if err != nil || cfg.TLSAuto || cfg.Listen != "127.0.0.1:9116" {
		t.Errorf("allow_http from the environment: %+v %v", cfg.Listen, err)
	}
}

func TestTLSValidDays(t *testing.T) {
	targets := `"targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]`
	load := func(doc string, env ...string) (*Config, error) { return LoadConfigEnv(writeConfig(t, doc), env) }
	if cfg, err := load(`{` + targets + `}`); err != nil || cfg.TLSValidDays != 365 {
		t.Errorf("the default: %v %v", cfg.TLSValidDays, err)
	}
	if cfg, err := load(`{"tls_valid_days": 90, ` + targets + `}`); err != nil || cfg.TLSValidDays != 90 {
		t.Errorf("90 days: %v %v", cfg.TLSValidDays, err)
	}
	if cfg, err := load(`{`+targets+`}`, "EASEXPORTER_TLS_VALID_DAYS=30"); err != nil || cfg.TLSValidDays != 30 {
		t.Errorf("from the environment: %v %v", cfg.TLSValidDays, err)
	}
	for _, bad := range []string{"-1", "3651", "100000"} {
		if _, err := load(`{"tls_valid_days": ` + bad + `, ` + targets + `}`); err == nil || !strings.Contains(err.Error(), "tls_valid_days") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
