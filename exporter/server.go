package exporter

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/mailmonitor"
	"github.com/nashcom/eas-tools/probe"
)

const promContentType = "text/plain; version=0.0.4; charset=utf-8"

// Server serves /probe, /metrics and /health.
type Server struct {
	store *probe.Store
	log   *slog.Logger

	rm *mailmonitor.Runner // the mail monitor; runs with RunMailMonitor

	debugMu       sync.Mutex // the extra sections of /debug/state
	debugSections map[string]func() any

	reloaderMu  sync.Mutex // POST /config/reload
	reloader    ReloadFunc
	reloadAsked atomic.Int64 // when the last request was accepted, Unix nanoseconds

	// cur is the configuration that is in use. A reload (see Reload) swaps it as a whole, so a request that started
	// with the old one finishes with it.
	cur     atomic.Pointer[live]
	started time.Time

	probes, probeErrors, rejected, cacheHits atomic.Int64
	probeNanos                               atomic.Int64

	reloadMu              sync.Mutex // one Reload at a time
	reloads, reloadErrors atomic.Int64
	denied                atomic.Int64                     // requests that allow_from refused
	tls                   atomic.Pointer[certreload.Store] // the certificate of the listener, when it serves HTTPS
	configErr             atomic.Pointer[configError]      // why the last check of the configuration failed; nil while it works
	lastCheck             atomic.Int64                     // unix time of the last successful fetch of a remote configuration, 0 = never
}

// live is one loaded configuration with what depends on it: the token, the limit of concurrent probes and the state
// of each target.
type live struct {
	cfg        *Config
	token      string
	acl        *ipacl.List   // the addresses that may connect; empty: all
	aclMetrics *ipacl.List   // the same for the metrics port
	sem        chan struct{} // limits concurrent probes
	targets    map[string]*targetState
	loadedAt   time.Time
}

type targetState struct {
	lock  chan struct{} // size 1; a channel so waiting honors the scrape timeout
	mu    sync.Mutex    // guards the fields below
	cache map[probeKind]cached

	// After an authentication failure the target is not probed again for
	// the backoff time: a wrong password probed every minute would only
	// trigger lockouts or ban rules on the server.
	blockedUntil time.Time
	blockedBody  []byte
	blockedAt    time.Time
}

// probeKind is what a scrape asks for. Every kind has its own cached result: a
// plain probe must never be answered with (or answer) a push or mail probe.
type probeKind struct {
	ping int  // heartbeat seconds of the Ping step, 0 = none
	mail bool // with the mail round trip
}

type cached struct {
	at   time.Time
	body []byte
}

// NewServer builds a Server. The store is shared by all targets.
func NewServer(cfg *Config, store *probe.Store, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	l, err := newLive(cfg, nil)
	if err != nil {
		return nil, err
	}
	s := &Server{store: store, log: log, started: time.Now()}
	s.cur.Store(l)
	s.rm = mailmonitor.New(s.openMailMonitor, log)
	s.rm.SetSettings(mailMonitorSettings(cfg))
	return s, nil
}

// newLive prepares a configuration for use. A target that is unchanged since prev keeps its state (cached results and
// the pause after a failed login); the limit of concurrent probes is kept when it did not change.
func newLive(cfg *Config, prev *live) (*live, error) {
	token, err := cfg.authToken()
	if err != nil {
		return nil, fmt.Errorf("auth token: %w", err)
	}
	acl, err := ipacl.Parse(cfg.AllowFrom)
	if err != nil {
		return nil, fmt.Errorf("allow_from: %w", err)
	}
	aclMetrics, err := ipacl.Parse(cfg.AllowFromMetrics)
	if err != nil {
		return nil, fmt.Errorf("allow_from_metrics: %w", err)
	}
	l := &live{cfg: cfg, token: token, acl: acl, aclMetrics: aclMetrics, targets: map[string]*targetState{}, loadedAt: time.Now()}
	if prev != nil && cap(prev.sem) == cfg.MaxConcurrent {
		l.sem = prev.sem
	} else {
		l.sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	for name, t := range cfg.Targets {
		if prev != nil {
			if old, ok := prev.targets[name]; ok && reflect.DeepEqual(prev.cfg.Targets[name], t) &&
				prev.cfg.Insecure == cfg.Insecure && prev.cfg.CAFile == cfg.CAFile {
				l.targets[name] = old
				continue
			}
		}
		l.targets[name] = &targetState{lock: make(chan struct{}, 1), cache: map[probeKind]cached{}}
	}
	return l, nil
}

// Reload replaces the configuration while the server runs. It returns what changed that only takes effect at the next
// start (the listen address, the state file and its save interval); everything else applies at once.
func (s *Server) Reload(cfg *Config) (restartNeeded []string, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	prev := s.cur.Load()
	l, err := newLive(cfg, prev)
	if err != nil {
		s.ReloadFailed(err)
		return nil, err
	}
	if prev.cfg.TLSCert != cfg.TLSCert || prev.cfg.TLSKey != cfg.TLSKey {
		restartNeeded = append(restartNeeded, "tls_cert/tls_key")
	}
	if prev.cfg.Listen != cfg.Listen {
		restartNeeded = append(restartNeeded, "listen")
	}
	if prev.cfg.ListenMetrics != cfg.ListenMetrics {
		restartNeeded = append(restartNeeded, "listen_metrics")
	}
	if prev.cfg.StateFile != cfg.StateFile {
		restartNeeded = append(restartNeeded, "state_file")
	}
	if prev.cfg.AutoSaveSeconds != cfg.AutoSaveSeconds {
		restartNeeded = append(restartNeeded, "autosave_seconds")
	}
	s.cur.Store(l)
	s.reloads.Add(1)
	s.rm.SetSettings(mailMonitorSettings(cfg))
	return restartNeeded, nil
}

// configError is the reason the last check of the configuration failed, and when.
type configError struct {
	text string
	at   time.Time
}

// ConfigChecked records that the configuration source was read and found valid, whether it changed or not: the time since
// then is what to alert on when the location is unreachable or the document is broken, and the status goes back to 0.
func (s *Server) ConfigChecked() {
	s.lastCheck.Store(time.Now().Unix())
	s.configErr.Store(nil)
}

// ReloadFailed counts a reload that did not happen because the new configuration could not be fetched or was invalid. It
// also sets the status to 1 until the next check that works; the count itself is never reset.
func (s *Server) ReloadFailed(err error) {
	s.reloadErrors.Add(1)
	s.ConfigFailing(err)
}

// ConfigFailing sets the status to 1 without counting a reload: the exporter runs on a configuration that is not the one of
// its source, for example the saved copy after the location was down at the start.
func (s *Server) ConfigFailing(err error) {
	s.configErr.Store(&configError{text: err.Error(), at: time.Now()})
}

// Handler returns the HTTP handler with all routes, guarded by allow_from. It is the one of the main listener (HTTPS when the
// exporter has a certificate).
func (s *Server) Handler() http.Handler { return s.handler(false) }

// MetricsHandler is the handler of the metrics port, guarded by allow_from_metrics. It has /metrics and /health and nothing else: the
// metrics carry no secret and no token (a scrape needs no encryption and no overhead of it), while /probe, /debug/state and
// /config/reload are on the main listener only. The main listener serves /metrics as well.
func (s *Server) MetricsHandler() http.Handler { return s.handler(true) }

// MainRoutes are the paths of the main listener, MetricsRoutes those of the metrics port. The handlers are built from these lists and the
// start writes them to the log, so what the log says is what is there.
var (
	MainRoutes    = []string{"/probe", "/metrics", "/health", "/debug/state", "/config/reload"}
	MetricsRoutes = []string{"/metrics", "/health"}
)

func (s *Server) handler(plain bool) http.Handler {
	handlers := map[string]http.HandlerFunc{"/probe": s.handleProbe, "/metrics": s.handleMetrics, "/health": s.handleHealth,
		"/debug/state": s.handleDebugState, "/config/reload": s.handleConfigReload}
	mux := http.NewServeMux()
	paths := MainRoutes
	if plain {
		paths = MetricsRoutes
	}
	for _, p := range paths {
		mux.HandleFunc(p, handlers[p])
	}

	// allow_from guards every endpoint, before the router and before a token. It is read from the configuration in use for every
	// request, so a reload applies it.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.cur.Load()
		acl, name := l.acl, "allow_from"
		if plain {
			acl, name = l.aclMetrics, "allow_from_metrics"
		}
		if acl.Empty() {
			mux.ServeHTTP(w, r)
			return
		}
		acl.Wrap(mux, func(remote string, r *http.Request) {
			s.denied.Add(1)
			s.log.Debug("request refused by "+name, "remote", remote, "method", r.Method, "path", r.URL.Path)
		}).ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OK\n"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	counter := func(name, help string, v any) {
		fmt.Fprintf(&b, "# HELP eas_probe_%s %s\n# TYPE eas_probe_%s counter\neas_probe_%s %v\n", name, help, name, name, v)
	}
	gauge := func(name, help string, v any) {
		fmt.Fprintf(&b, "# HELP eas_probe_%s %s\n# TYPE eas_probe_%s gauge\neas_probe_%s %v\n", name, help, name, name, v)
	}

	counter("probes_total", "Total number of probes that were actually run", s.probes.Load())
	counter("probe_errors_total", "Total number of probes where a step failed", s.probeErrors.Load())
	counter("probe_duration_seconds_total", "Total accumulated probe duration in seconds since startup",
		strconv.FormatFloat(time.Duration(s.probeNanos.Load()).Seconds(), 'f', 3, 64))
	counter("queue_rejected_total", "Total number of probe requests rejected because too many probes were running", s.rejected.Load())
	counter("denied_requests_total", "Total number of requests refused because the address of the peer is not in allow_from", s.denied.Load())
	if st := s.tls.Load(); st != nil {
		in := st.Info()
		gauge("tls_certificate_not_after_timestamp_seconds", "Unix time when the TLS certificate of the listener expires", in.NotAfter.Unix())
		counter("tls_reloads_total", "Total number of times a new TLS certificate was loaded while the listener ran", in.Reloads)
		counter("tls_reload_errors_total", "Total number of times a changed TLS certificate could not be loaded; the one in use stays", in.Failures)
	}
	counter("cache_hits_total", "Total number of probe requests answered from the minimum-interval cache", s.cacheHits.Load())
	l := s.cur.Load()
	gauge("targets", "Number of configured targets", len(l.targets))
	gauge("config_loaded_timestamp_seconds", "Unix time when the configuration in use was loaded", l.loadedAt.Unix())
	gauge("config_last_check_timestamp_seconds", "Unix time of the last successful check for a changed configuration (0 = never, or the reload is off)", s.lastCheck.Load())
	counter("config_reloads_total", "Total number of configuration reloads that were applied", s.reloads.Load())
	counter("config_reload_errors_total", "Total number of configuration reloads that failed; the exporter keeps the last good configuration", s.reloadErrors.Load())
	status := 0
	if s.configErr.Load() != nil {
		status = 1
	}
	gauge("config_status", "Status of the configuration source: 0 = the last check worked, 1 = the last check failed and the exporter runs on the last good configuration", status)
	gauge("devices", "Number of devices in the state store", s.store.Len())
	gauge("start_timestamp_seconds", "Start time of the exporter", s.started.Unix())
	gauge("uptime_seconds", "Seconds since the exporter started", int64(time.Since(s.started).Seconds()))

	s.rm.WritePrometheus(&b)

	w.Header().Set("Content-Type", promContentType)
	_, _ = w.Write(b.Bytes())
}

func (s *Server) authorized(l *live, r *http.Request) bool {
	if l.token == "" {
		return true
	}
	got := r.URL.Query().Get("auth")
	if h := getToken(r, l.cfg.AuthTokenName); h != "" {
		got = h
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(l.token)) == 1
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Only GET is supported\n", http.StatusMethodNotAllowed)
		return
	}
	l := s.cur.Load()
	if !s.authorized(l, r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="eas-probe"`)
		http.Error(w, "Unauthorized\n", http.StatusUnauthorized)
		return
	}

	name := r.URL.Query().Get("target")
	target, ok := l.cfg.Targets[name]
	ts := l.targets[name]
	if name == "" || !ok {
		http.Error(w, "Unknown or missing target\n", http.StatusNotFound)
		return
	}

	mailTest := false
	if v := r.URL.Query().Get("mail"); v != "" {
		switch v {
		case "1", "true", "yes":
			if !target.Mail {
				http.Error(w, "mail probes are not enabled for this target (set \"mail\": true in its configuration)\n", http.StatusBadRequest)
				return
			}
			mailTest = true
		case "0", "false", "no":
		default:
			http.Error(w, "mail must be 1 or 0\n", http.StatusBadRequest)
			return
		}
	}
	kind := probeKind{mail: mailTest}

	pingSeconds := 0
	if v := r.URL.Query().Get("ping"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > l.cfg.MaxPingSeconds {
			http.Error(w, fmt.Sprintf("ping must be 0..%d\n", l.cfg.MaxPingSeconds), http.StatusBadRequest)
			return
		}
		pingSeconds = n
	}
	kind.ping = pingSeconds

	// Honor Prometheus' scrape timeout, like the blackbox exporter does.
	timeout := time.Duration(l.cfg.DefaultTimeoutSeconds)*time.Second + time.Duration(pingSeconds)*time.Second
	if mailTest {
		timeout += target.mailWait()
	}
	if v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 1 {
			timeout = time.Duration((secs - 0.5) * float64(time.Second))
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// One probe per target at a time; later scrapes wait for it and then
	// usually get its cached result.
	select {
	case ts.lock <- struct{}{}:
		defer func() { <-ts.lock }()
	case <-ctx.Done():
		http.Error(w, "Timed out waiting for the running probe of this target\n", http.StatusServiceUnavailable)
		return
	}

	ts.mu.Lock()
	c, hit := ts.cache[kind]
	blockedUntil, blockedBody, blockedAt := ts.blockedUntil, ts.blockedBody, ts.blockedAt
	ts.mu.Unlock()
	if time.Now().Before(blockedUntil) {
		s.cacheHits.Add(1)
		s.writeProbe(w, blockedBody, blockedAt, blockedUntil, true)
		return
	}
	if hit && time.Since(c.at) < l.cfg.MinInterval() {
		s.cacheHits.Add(1)
		s.writeProbe(w, c.body, c.at, c.at.Add(l.cfg.MinInterval()), false)
		return
	}

	select {
	case l.sem <- struct{}{}:
		defer func() { <-l.sem }()
	default:
		s.rejected.Add(1)
		http.Error(w, "Too many probes running\n", http.StatusServiceUnavailable)
		return
	}

	res := s.runProbe(ctx, l, name, target, kind)

	var b bytes.Buffer
	probe.WritePrometheus(&b, res)
	body := b.Bytes()

	now := time.Now()
	next := now.Add(l.cfg.MinInterval())
	backoff := authFailed(res)
	ts.mu.Lock()
	ts.cache[kind] = cached{at: now, body: body}
	if backoff {
		ts.blockedUntil = now.Add(l.cfg.AuthBackoff())
		ts.blockedBody, ts.blockedAt = body, now
		next = ts.blockedUntil
	}
	ts.mu.Unlock()
	if backoff {
		s.log.Warn("authentication failed; not probing this target again for a while",
			"target", name, "backoff", l.cfg.AuthBackoff())
	}

	s.writeProbe(w, body, now, next, backoff)
}

// authFailed reports whether a step failed because the server refused the
// login (401) or the device/user (403).
func authFailed(res *probe.Result) bool {
	for _, st := range res.Steps {
		if !st.OK && !st.Skipped && (st.ErrorClass == "auth" || st.ErrorClass == "forbidden") {
			return true
		}
	}
	return false
}

// writeProbe sends a probe result with the timing metrics: when it ran, how
// old it is, when the next real probe may happen and whether the target is in
// authentication backoff.
func (s *Server) writeProbe(w http.ResponseWriter, body []byte, at, next time.Time, backoff bool) {
	var b bytes.Buffer
	b.Write(body)
	fmt.Fprintln(&b, "# HELP eas_probe_timestamp_seconds Unix time when the probe ran.")
	fmt.Fprintln(&b, "# TYPE eas_probe_timestamp_seconds gauge")
	fmt.Fprintf(&b, "eas_probe_timestamp_seconds %d\n", at.Unix())
	fmt.Fprintln(&b, "# HELP eas_probe_cache_age_seconds Age of the result; 0 for a probe that just ran.")
	fmt.Fprintln(&b, "# TYPE eas_probe_cache_age_seconds gauge")
	fmt.Fprintf(&b, "eas_probe_cache_age_seconds %d\n", int64(time.Since(at).Seconds()))
	fmt.Fprintln(&b, "# HELP eas_probe_next_probe_seconds Seconds until the target may be probed for real again.")
	fmt.Fprintln(&b, "# TYPE eas_probe_next_probe_seconds gauge")
	wait := int64(time.Until(next).Seconds())
	if wait < 0 {
		wait = 0
	}
	fmt.Fprintf(&b, "eas_probe_next_probe_seconds %d\n", wait)
	fmt.Fprintln(&b, "# HELP eas_probe_auth_backoff 1 while probing is paused after a failed login, to protect the account from lockout.")
	fmt.Fprintln(&b, "# TYPE eas_probe_auth_backoff gauge")
	if backoff {
		fmt.Fprintln(&b, "eas_probe_auth_backoff 1")
	} else {
		fmt.Fprintln(&b, "eas_probe_auth_backoff 0")
	}

	w.Header().Set("Content-Type", promContentType)
	_, _ = w.Write(b.Bytes())
}

// runProbe runs one probe of the target and updates the counters. A broken
// target configuration (e.g. unreadable password file) is reported as a
// failed probe with error class "config", so it shows up in alerting.
func (s *Server) runProbe(ctx context.Context, l *live, name string, t Target, kind probeKind) *probe.Result {
	start := time.Now()

	var res *probe.Result

	password, err := t.password()
	if err != nil {
		s.log.Error("target configuration", "target", name, "error", err)
		res = configFailure(t, err)
	} else {
		insecure := l.cfg.Insecure
		if t.Insecure != nil {
			insecure = *t.Insecure
		}
		ca := l.cfg.CAFile
		if t.CAFile != "" {
			ca = t.CAFile
		}

		headers, _ := probe.ParseHeaders(t.headerLines()) // checked when the config was loaded

		res, err = probe.Run(ctx, probe.Config{
			ServerURL:      t.Server,
			Email:          t.Email,
			Username:       t.User,
			Password:       password,
			DeviceID:       strings.ToUpper(t.DeviceID),
			Profile:        t.Profile,
			Provision:      t.Provision,
			Directory:      t.Directory,
			DirectoryQuery: t.DirectoryQuery,
			PingSeconds:    kind.ping,
			Mail:           kind.mail,
			MailWait:       t.mailWait(),
			Store:          s.store,
			Insecure:       insecure,
			CACertFile:     ca,
			StepTimeout:    30 * time.Second,
			Headers:        headers,
			Logger:         s.log,
		})
		if err != nil {
			s.log.Error("target configuration", "target", name, "error", err)
			res = configFailure(t, err)
		}
	}

	s.probes.Add(1)
	s.probeNanos.Add(int64(time.Since(start)))
	if !res.OK {
		s.probeErrors.Add(1)
	}

	attrs := []any{"target", name, "ok", res.OK, "duration", res.Duration.Round(time.Millisecond)}
	for _, st := range res.Steps {
		if !st.OK && !st.Skipped {
			attrs = append(attrs, "failed_step", st.Name, "error_class", st.ErrorClass, "error", st.Error)
			break
		}
	}
	s.log.Debug("probe", attrs...)
	return res
}

func configFailure(t Target, err error) *probe.Result {
	return &probe.Result{
		User: t.User,
		OK:   false,
		Steps: []probe.Step{{
			Name:       "config",
			Error:      err.Error(),
			ErrorClass: "config",
		}},
	}
}

// TargetNames returns the configured target names, sorted.
func (s *Server) TargetNames() []string {
	l := s.cur.Load()
	names := make([]string, 0, len(l.targets))
	for n := range l.targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SameConfig reports whether the configuration in use has exactly these settings. The warnings are not settings. It is
// how a reload tells a document that only looks different (spaces, the order of keys) from one that changes something.
func (s *Server) SameConfig(cfg *Config) bool {
	a, b := *s.cur.Load().cfg, *cfg
	a.Warnings, b.Warnings = nil, nil
	return reflect.DeepEqual(a, b)
}

// MailMonitorNames returns the names of the configured mail monitor entries, sorted.
func (s *Server) MailMonitorNames() []string {
	c := s.cur.Load().cfg
	names := make([]string, 0, len(c.MailMonitor))
	for n := range c.MailMonitor {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SetTLS tells the server the store of the certificate of its listener, for the metrics and the debug state.
func (s *Server) SetTLS(st *certreload.Store) { s.tls.Store(st) }
