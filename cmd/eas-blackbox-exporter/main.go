// eas-blackbox-exporter is a Prometheus blackbox-style exporter for ActiveSync
// (HCL Traveler, Exchange, Z-Push, ...). Prometheus scrapes
// /probe?target=NAME; the targets and their credentials come from a JSON
// config file.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nashcom/eas-tools/exporter"
	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/listenaddr"
	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/probe"
)

// logState is the level and the format of the log; a reload can change both. cmdVerbose, cmdDebug and cmdLogFormat are the
// command line, which wins over log_level and log_format.
var (
	logState             = logging.NewState(slog.LevelInfo)
	cmdVerbose, cmdDebug bool
	cmdLogFormat         string
)

// applyLogging sets the level and the format from the configuration (log_level, log_format), unless the command line decided.
func applyLogging(cfg *exporter.Config) {
	if lvl, err := logging.Pick(cfg.LogLevel, cmdVerbose, cmdDebug, slog.LevelInfo); err == nil {
		logState.SetLevel(lvl)
	}
	format := cfg.LogFormat
	if cmdLogFormat != "" {
		format = cmdLogFormat
	}
	_ = logState.SetFormat(format)
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		configPath  = flag.String("config", env("EASEXPORTER_CONFIG", "/etc/eas-tools/config.json"), "path of the JSON config file, or an https:// location to fetch it from (EASEXPORTER_CONFIG)")
		healthCheck = flag.Bool("health-check", false, "query the local /health endpoint and exit 0 or 1 (for container health checks)")
		verbose     = flag.Bool("verbose", false, "log at level debug: also the routine activity (the default is info; log_level sets the level)")
		debug       = flag.Bool("debug", false, "log at level trace: the details (the default is info; log_level sets the level)")
		logFormat   = flag.String("log-format", "", "format of the log: text (the default) or json, one OpenTelemetry-style object per line (log_format)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("eas-blackbox-exporter", probe.Version)
		return 0
	}

	// The command line wins over log_level (EASEXPORTER_LOG_LEVEL or the file). The environment is read here already, so
	// that the loading of the configuration is logged at the right level; the file and a reload set it again.
	cmdVerbose, cmdDebug, cmdLogFormat = *verbose, *debug, *logFormat
	early, _ := logging.Pick(os.Getenv("EASEXPORTER_LOG_LEVEL"), cmdVerbose, cmdDebug, slog.LevelInfo)
	logState.SetLevel(early)
	earlyFormat := os.Getenv("EASEXPORTER_LOG_FORMAT")
	if cmdLogFormat != "" {
		earlyFormat = cmdLogFormat
	}
	if err := logState.SetFormat(earlyFormat); err != nil {
		fmt.Fprintln(os.Stderr, "eas-blackbox-exporter:", err)
		return 2
	}
	log := logging.New(os.Stderr, logState, "eas-blackbox-exporter", probe.Version)

	if *healthCheck {
		return checkHealth(healthCheckTarget(*configPath))
	}

	cs, err := startConfig(*configPath, log)
	if err != nil {
		return 2
	}

	cfg := cs.cfg
	if err := cfg.CheckListen(); err != nil {
		log.Error("listen", "error", err)
		return 2
	}
	applyLogging(cfg)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	store, err := probe.OpenStore(cfg.StateFile)
	if err != nil {
		log.Error("state", "error", err)
		return 2
	}
	if w := store.Recovered(); w != "" {
		log.Warn(w)
	}
	stopSave := store.AutoSave(time.Duration(cfg.AutoSaveSeconds)*time.Second, func(err error) {
		log.Error("saving state", "error", err)
	})

	srv, err := exporter.NewServer(cfg, store, log)
	if err != nil {
		log.Error("exporter", "error", err)
		return 2
	}

	srv.DebugSection("log", func() any {
		return map[string]any{"level": logging.Name(logState.Level()), "format": logState.Format()}
	})

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TLS: the pair is held by a store that loads a changed pair while the listener runs
	var certs *certreload.Store
	if cfg.TLSCert != "" {
		if cfg.MakesCertificate() {
			o := certreload.GenerateOptions{CertFile: cfg.TLSCert, KeyFile: cfg.TLSKey, Names: cfg.TLSNames, LeafDays: cfg.TLSValidDays}
			if err := certreload.Prepare(o, cfg.TLSAuto, log); err != nil {
				log.Error("tls", "error", err)
				return 2
			}
		}
		if certs, err = certreload.New(cfg.TLSCert, cfg.TLSKey, log); err != nil {
			log.Error("tls", "error", err)
			return 2
		}
		srv.SetTLS(certs)
		httpSrv.TLSConfig = certs.TLSConfig()
		go certs.Watch(ctx, time.Duration(cfg.TLSCheckSeconds)*time.Second)
	}

	go srv.RunMailMonitor(ctx)

	if cs.source != nil {
		if !cs.fresh {
			srv.ConfigFailing(errors.New("the configuration source could not be read at the start, the saved copy is in use"))
		}
		if cs.fresh && cs.checkEvery > 0 {
			srv.ConfigChecked() // the configuration was read at the start, not taken from the saved copy
		}
		go reloadLoop(ctx, cs, srv, certs, *configPath, log) // also without an interval: it answers the requests
	}

	// POST /config/reload and SIGHUP check the source now
	srv.SetReloader(func(rctx context.Context) (any, int) {
		res, ok := cs.reloadNow(rctx)
		switch {
		case !ok:
			return map[string]string{"result": "failed", "error": "the configuration does not come from a file or an https:// location, there is nothing to check"}, http.StatusConflict
		case res.Result == "failed":
			return res, http.StatusInternalServerError
		}
		return res, http.StatusOK
	})
	go onSIGHUP(ctx, cs, certs, log)

	// The listeners are bound first, so that a port that is taken stops the start with a clear message, and the log can say what the
	// system made of the addresses. The metrics port: a plain HTTP listener of its own with /metrics and /health, always, with TLS or
	// without. It is open (the interfaces of listen) unless allow_from_metrics restricts it; /metrics is on the main listener too.
	metricsSrv := &http.Server{Addr: cfg.ListenMetrics, Handler: srv.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	mainLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listener", "name", "main", "error", err)
		return 2
	}
	metricsLn, err := net.Listen("tcp", cfg.ListenMetrics)
	if err != nil {
		log.Error("listener", "name", "metrics", "error", err, "hint", "the metrics port is 9100 unless listen_metrics says another")
		return 2
	}

	mainNote := "plain HTTP because the address is loopback: it never leaves the machine"
	switch {
	case certs != nil:
		mainNote = "HTTPS only: there is no plain HTTP on this port"
	case cfg.AllowHTTP:
		mainNote = "plain HTTP chosen with allow_http: that is for a proxy that terminates TLS, and allow_from should name it"
		log.Warn("everything is served over plain HTTP (allow_http): the token of /probe crosses the network in clear text unless a proxy in front encrypts it",
			"listen", cfg.Listen, "allow_from", cfg.AllowFrom)
	}
	metricsNote := "carries no secret and needs no token; allow_from_metrics restricts who may connect"
	if len(cfg.AllowFromMetrics) > 0 {
		metricsNote = "carries no secret and needs no token; restricted by allow_from_metrics"
	}
	listenaddr.Listener{Name: "main", Configured: cfg.Listen, Bound: mainLn.Addr(), TLS: certs != nil, Allow: cfg.AllowFrom,
		Endpoints: cfg.Endpoints(false), Note: mainNote, PlainWhy: listenaddr.PlainWhy(cfg.Listen, cfg.AllowHTTP)}.Log(log)
	listenaddr.Listener{Name: "metrics", Configured: cfg.ListenMetrics, Bound: metricsLn.Addr(), Allow: cfg.AllowFromMetrics,
		Endpoints: cfg.Endpoints(true), Note: metricsNote}.Log(log)

	go func() { errc <- metricsSrv.Serve(metricsLn) }()
	go func() {
		if certs != nil {
			errc <- httpSrv.ServeTLS(mainLn, "", "") // the certificate comes from the store
		} else {
			errc <- httpSrv.Serve(mainLn)
		}
	}()

	log.Info("started", "version", probe.Version, "targets", srv.TargetNames(), "mail_monitor", len(cfg.MailMonitor),
		"min_interval", cfg.MinInterval(), "state", cfg.StateFile, "devices", store.Len(), "config", describeSource(*configPath, cs))

	code := 0
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener", "error", err)
			code = 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	_ = metricsSrv.Shutdown(shutdownCtx)

	// Final snapshot so the registered devices survive the restart.
	if err := stopSave(); err != nil {
		log.Error("saving state", "error", err)
		code = 1
	}
	return code
}

// checkHealth asks the running exporter on loopback for /health. The address is the one of the metrics port, which is plain HTTP
// with TLS and without, so no certificate is involved.
func checkHealth(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "health-check:", err)
		return 1
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "health-check:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health-check: HTTP", resp.Status)
		return 1
	}
	return 0
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// configStart is what the start found: the configuration, and where to look for a newer one while running.
type configStart struct {
	cfg        *exporter.Config
	source     exporter.Source    // checked every reload seconds; nil when there is nothing to check
	remote     *exporter.Remote   // set for an https:// location: it keeps the saved copy
	sum        [sha256.Size]byte  // hash of the document the configuration was made from (not the document itself)
	fresh      bool               // false when the saved copy was used because the location was down
	from       string             // "file", "environment", "remote" or "saved copy": where the configuration in use came from
	checkEvery int                // seconds between two checks, 0 = only on request
	trigger    chan reloadRequest // a check on request (POST /config/reload, SIGHUP), answered by reloadLoop
}

// defaultFileCheck is how often a configuration file is checked; the check is a stat of the file, so it is cheap.
const defaultFileCheck = 60

// startConfig loads the configuration and says in the log where it comes from and whether that worked.
func startConfig(location string, log *slog.Logger) (*configStart, error) {
	source := safeLocation(location)
	log.Info("loading the configuration", "source", source)

	cs, err := loadStart(location, log)
	if err != nil {
		log.Error("the configuration could not be loaded", "source", source, "error", err)
		return nil, err
	}

	attrs := []any{"source", source, "from", cs.from, "targets", len(cs.cfg.Targets)}
	if cs.source != nil && cs.checkEvery > 0 {
		attrs = append(attrs, "check_every", fmt.Sprintf("%ds", cs.checkEvery))
	}
	if cs.from == "saved copy" {
		log.Warn("configuration loaded from the saved copy, the source was not usable", attrs...)
	} else {
		log.Info("configuration loaded", attrs...)
	}
	return cs, nil
}

// safeLocation is the location for the log: a URL without its query string and fragment, which could hold a secret.
func safeLocation(location string) string {
	if !exporter.IsRemote(location) {
		return location
	}
	u, err := url.Parse(location)
	if err != nil {
		return "remote location"
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path)
}

// startConfig loads the configuration from a file or, for an https:// location, from there. When the location cannot be
// reached at start, the last good document that was saved is used, so a restart does not depend on it.
func loadStart(location string, log *slog.Logger) (*configStart, error) {
	cs := &configStart{fresh: true, trigger: make(chan reloadRequest)}

	if !exporter.IsRemote(location) {
		fs := exporter.NewFileSource(location)
		fs.Prime() // before reading, so that a change in between is found by the first check

		raw, err := os.ReadFile(location)
		switch {
		case err == nil:
			if cs.cfg, err = exporter.LoadConfigBytes(raw, location, os.Environ()); err != nil {
				return nil, err
			}
			cs.sum, cs.from = sha256.Sum256(raw), "file"
			cs.source = fs
			if cs.checkEvery, err = exporter.ConfigCheckSeconds(os.Environ(), defaultFileCheck); err != nil {
				return nil, err
			}
		default:
			// no file: a configuration that comes from the environment alone has nothing to check
			cs.from = "environment"
			if cs.cfg, err = exporter.LoadConfig(location); err != nil {
				return nil, err
			}
		}
		return cs, nil
	}

	remote, err := exporter.NewRemote(location, os.Environ())
	if err != nil {
		return nil, err
	}
	cs.remote, cs.source, cs.checkEvery = remote, remote, remote.CheckSeconds

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := remote.Fetch(ctx)
	if err == nil {
		var cfg *exporter.Config
		if cfg, err = exporter.LoadConfigBytes(raw, location, os.Environ()); err == nil {
			remote.Accept()
			if serr := remote.SaveCache(raw); serr != nil {
				log.Warn("the configuration could not be cached, a restart needs the remote location", "error", serr)
			}
			cs.cfg, cs.sum, cs.from = cfg, sha256.Sum256(raw), "remote"
			return cs, nil
		}
	}

	log.Warn("the remote configuration is not usable, trying the saved copy", "error", err)
	raw, cerr := remote.LoadCache()
	if cerr != nil {
		return nil, fmt.Errorf("%v; and there is no saved copy: %v", err, cerr)
	}
	cfg, err := exporter.LoadConfigBytes(raw, "the saved copy", os.Environ())
	if err != nil {
		return nil, fmt.Errorf("the saved copy: %v", err)
	}
	cs.cfg, cs.sum, cs.fresh, cs.from = cfg, sha256.Sum256(raw), false, "saved copy"
	return cs, nil
}

// reloadResult is what one check of the configuration source found.
type reloadResult struct {
	Result        string   `json:"result"` // applied, unchanged or failed
	Why           string   `json:"why,omitempty"`
	Error         string   `json:"error,omitempty"`
	Source        string   `json:"source"`
	Targets       []string `json:"targets,omitempty"`
	MailMonitor   []string `json:"mail_monitor,omitempty"`
	RestartNeeded []string `json:"restart_needed,omitempty"`
}

// reloadRequest asks the loop for a check now; the loop answers on reply.
type reloadRequest struct{ reply chan reloadResult }

// reloadNow checks the configuration source now, the way the timer does, and says what it found. The check runs in the
// loop, so a manual and a timed check never overlap. The second value is false when there is no source to check.
func (cs *configStart) reloadNow(ctx context.Context) (reloadResult, bool) {
	if cs.source == nil {
		return reloadResult{}, false
	}
	req := reloadRequest{reply: make(chan reloadResult, 1)}
	select {
	case cs.trigger <- req:
	case <-ctx.Done():
		return reloadResult{Result: "failed", Error: "the check did not start: " + ctx.Err().Error()}, true
	}
	select {
	case res := <-req.reply:
		return res, true
	case <-ctx.Done():
		return reloadResult{Result: "failed", Error: "the check did not finish: " + ctx.Err().Error()}, true
	}
}

// reloadLoop checks the source (and, when the listener serves TLS, the certificate files) every interval, and when it is asked to (POST /config/reload, SIGHUP), and applies the
// configuration when its settings changed. The source says whether the document changed (the time and size of a file, the
// ETag and Last-Modified of a URL), which saves the read. What it returns is first compared by a hash with the last accepted
// document (the same bytes: nothing to do; only the hash is kept, not the document), and then, when the bytes differ, by its
// parsed settings, so a document that only looks different (spaces, key order) changes nothing. A source that cannot be read
// and a document that fails leave the running configuration alone and are counted.
func reloadLoop(ctx context.Context, cs *configStart, srv *exporter.Server, certs *certreload.Store, location string, log *slog.Logger) {
	var tick <-chan time.Time // without an interval the source is only checked on request
	if cs.checkEvery > 0 {
		t := time.NewTicker(time.Duration(cs.checkEvery) * time.Second)
		defer t.Stop()
		tick = t.C
	}
	lastSum := cs.sum
	source := safeLocation(location)
	failing := false

	// checked records a successful check; the first one after failures is worth an entry at the normal level. At the trace
	// level every check says what it found and why nothing changed.
	checked := func(result, why string, extra ...any) reloadResult {
		srv.ConfigChecked()
		if failing {
			log.Info("the configuration source works again", "source", source, "result", result)
			failing = false
		} else {
			logging.Trace(log, "configuration checked", append([]any{"source", source, "result", result, "why", why}, extra...)...)
		}
		return reloadResult{Result: "unchanged", Why: result + ": " + why, Source: source}
	}
	short := func(sum [32]byte) string { return hex.EncodeToString(sum[:])[:12] }
	notModified := "the time and size of the file are those of the file in use"
	if cs.remote != nil {
		notModified = "the server answered 304 Not Modified: its ETag or Last-Modified is that of the document in use"
	}
	failed := func(msg string, err error) reloadResult {
		srv.ReloadFailed(err)
		failing = true
		log.Warn(msg, "source", source, "error", err)
		return reloadResult{Result: "failed", Error: msg + ": " + err.Error(), Source: source}
	}

	check := func() reloadResult {
		if c, ok := cs.source.(interface{ Compared() string }); ok {
			logging.Trace(log, "checking the configuration", "source", source, "compared_with", c.Compared())
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		raw, err := cs.source.Fetch(fctx)
		cancel()
		if errors.Is(err, exporter.ErrNotModified) {
			return checked("not modified", notModified)
		}
		if err != nil {
			return failed("configuration reload failed, keeping the current configuration", err)
		}

		// the same bytes as the last accepted document: nothing to parse, nothing to compare
		sum := sha256.Sum256(raw)
		if sum == lastSum {
			cs.source.Accept()
			return checked("same content", "the document was read again and its hash is that of the document in use", "sha256", short(sum))
		}

		cfg, err := exporter.LoadConfigBytes(raw, location, os.Environ())
		if err != nil {
			return failed("the new configuration is invalid, keeping the current configuration", err)
		}
		if srv.SameConfig(cfg) {
			cs.source.Accept()
			res := checked("the document changed, the settings did not", "the bytes differ, but the settings are the same: only spaces, the order of keys or similar changed", "sha256", short(lastSum)+" -> "+short(sum))
			lastSum = sum
			return res
		}
		restart, err := srv.Reload(cfg)
		if err != nil {
			failing = true
			log.Warn("the new configuration could not be applied, keeping the current configuration", "source", source, "error", err)
			return reloadResult{Result: "failed", Error: "the new configuration could not be applied: " + err.Error(), Source: source}
		}
		cs.source.Accept()
		lastSum = sum
		failing = false
		applyLogging(cfg)
		srv.ConfigChecked()
		if cs.remote != nil {
			if serr := cs.remote.SaveCache(raw); serr != nil {
				log.Warn("the configuration could not be cached", "error", serr)
			}
		}
		for _, w := range cfg.Warnings {
			log.Warn(w)
		}
		log.Info("configuration reloaded", "source", source, "targets", srv.TargetNames(), "mail_monitor", srv.MailMonitorNames())
		if len(restart) > 0 {
			log.Warn("these settings changed and apply at the next start", "settings", restart)
		}
		return reloadResult{Result: "applied", Why: "the settings changed", Source: source, Targets: srv.TargetNames(),
			MailMonitor: srv.MailMonitorNames(), RestartNeeded: restart}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			check()
			certs.Check(ctx) // every check of the configuration looks at the certificate files as well
		case req := <-cs.trigger:
			log.Info("the configuration is checked on request", "source", source)
			req.reply <- check()
			certs.Check(ctx)
		}
	}
}

// healthCheckTarget returns the address of the running exporter for the health check: the metrics port, which is plain HTTP with TLS
// and without, so that no certificate is involved. For a remote configuration the saved copy is
// used, so that the check does not fetch from the network every few seconds.
func healthCheckTarget(location string) string {
	fromConfig := func(cfg *exporter.Config) string { return cfg.ListenMetrics }
	if !exporter.IsRemote(location) {
		if cfg, err := exporter.LoadConfig(location); err == nil {
			return fromConfig(cfg)
		}
		return fromEnv()
	}
	if remote, err := exporter.NewRemote(location, os.Environ()); err == nil {
		if raw, err := remote.LoadCache(); err == nil {
			if cfg, err := exporter.LoadConfigBytes(raw, "the saved copy", os.Environ()); err == nil {
				return fromConfig(cfg)
			}
		}
	}
	return fromEnv()
}

// fromEnv is the address when the configuration cannot be read: what the environment says, and the default.
func fromEnv() string {
	if metrics := os.Getenv("EASEXPORTER_LISTEN_METRICS"); metrics != "" {
		return metrics
	}
	return exporter.DefaultListenMetrics
}

// describeSource says where the configuration came from, without the query string of a URL, which could hold a secret.
func describeSource(location string, cs *configStart) string {
	check := ""
	if cs.source != nil && cs.checkEvery > 0 {
		check = fmt.Sprintf(" (checked every %ds)", cs.checkEvery)
	}
	if cs.remote == nil {
		return location + check
	}
	u, err := url.Parse(location)
	if err != nil {
		return "remote" + check
	}
	return fmt.Sprintf("%s://%s%s%s", u.Scheme, u.Host, u.Path, check)
}

// onSIGHUP checks the configuration source when the process gets SIGHUP, the usual way to ask a service to read its
// configuration again (docker kill -s HUP NAME). The result is in the log.
func onSIGHUP(ctx context.Context, cs *configStart, certs *certreload.Store, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if certs != nil { // the TLS certificate and key are looked at again as well
				log.Info("SIGHUP: reloading the TLS certificate")
				_, _ = certs.Reload()
			}
			rctx, cancel := context.WithTimeout(ctx, time.Minute)
			res, ok := cs.reloadNow(rctx)
			cancel()
			if !ok {
				log.Warn("SIGHUP: the configuration does not come from a file or an https:// location, there is nothing to check")
				continue
			}
			log.Info("SIGHUP: the configuration was checked", "result", res.Result)
		}
	}
}
