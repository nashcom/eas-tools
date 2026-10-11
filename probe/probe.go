// Package probe runs a read-only ActiveSync conversation against one
// account and reports what happened: which server answered, which protocol
// version was negotiated, how long every step took and why a step failed.
//
// The package is deliberately free of CLI and output concerns so that a
// blackbox-exporter style HTTP front end can call Run directly later.
package probe

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/nashcom/eas-tools/internal/logging"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hstern/go-activesync/eas"
)

const (
	// DefaultEndpointPath is the path every EAS server answers on.
	DefaultEndpointPath = "/Microsoft-Server-ActiveSync"

	// ProvisionAuto provisions only when no policy key is stored yet.
	ProvisionAuto = "auto"
	// ProvisionAlways runs the Provision handshake on every probe.
	ProvisionAlways = "always"
	// ProvisionNever skips Provision and relies on the 449 recovery.
	ProvisionNever = "never"

	// Step names, also used as the "step" label in the metrics.
	StepAutodiscover = "autodiscover"
	StepOptions      = "options"
	StepProvision    = "provision"
	StepSettings     = "settings"
	StepDirectory    = "directory"
	StepSendMail     = "sendmail"
	StepMail         = "mail"
	StepFolderSync   = "foldersync"
	StepSync         = "sync"
	StepPing         = "ping"
)

// Config describes one account to probe.
type Config struct {
	// ServerURL is the EAS endpoint. A bare host name or a URL without a
	// path is accepted; the standard endpoint path is added. When empty,
	// the endpoint is found via Autodiscover from Email.
	ServerURL string
	// Email is used for Autodiscover (needs a domain part).
	Email string
	// Username is the login. Defaults to Email.
	Username string
	Password string

	// DeviceID is the 32 hex character device identifier. When empty a
	// stable one is derived from the user name and server.
	DeviceID string
	// Directory controls the directory (GAL) lookup of the account itself:
	// "auto" (default) does it when the server offers Search, "on" always,
	// "off" never. DirectoryQuery overrides the query (default: the user
	// name, an e-mail address in the lab).
	Directory      string
	DirectoryQuery string
	// Mail adds a step that sends a mail to the account itself, waits for it to
	// come back in the Inbox and deletes it again: a round trip through the
	// server's mail routing. Off by default (it creates a mail).
	Mail bool
	// MailWait is how long to wait for the mail (default 60 s); MailArrival is
	// "poll" (default, a Sync every second) or "push" (a held Ping; unreliable on one device, see the guide); MailKeep leaves the mail and a
	// copy in Sent Items; MailSizeKB is the body size (default 2).
	MailWait    time.Duration
	MailArrival string
	MailKeep    bool
	MailSizeKB  float64
	// DeviceNumber is the number of this device among the devices of one user
	// (1-based; 0 and 1 both mean the first). Each number is its own device
	// in the server's device list, also for the same profile.
	DeviceNumber int
	// Profile selects how the probe presents itself (see profile.go);
	// DeviceType and UserAgent override the profile's values.
	Profile    string
	DeviceType string
	UserAgent  string

	// ASVersion forces a protocol version; empty means detect via OPTIONS.
	ASVersion string

	// Store holds the per-device state (policy key, sync keys, folders).
	// The caller owns it and decides when it is flushed to disk. When nil,
	// Run uses a throw-away in-memory store, so every run registers and
	// syncs from scratch.
	Store *Store
	// Fresh discards the stored state of this device before the run.
	Fresh bool

	// Provision is ProvisionAuto (default), ProvisionAlways or ProvisionNever.
	Provision string

	// PingSeconds, when > 0, adds a Ping step that holds the connection for
	// up to this many seconds. Off by default because it blocks that long.
	PingSeconds int
	// SyncWindow is the number of mail items requested (default 5).
	SyncWindow int

	// Insecure skips TLS certificate verification. CACertFile adds extra
	// trusted roots (PEM).
	Insecure   bool
	CACertFile string

	// StepTimeout limits each step (default 30s; Ping adds its heartbeat).
	StepTimeout time.Duration

	// Headers are extra request headers added to every request (see ParseHeaders);
	// for example X-Mock-Sim to tell the mock server how to behave.
	Headers http.Header

	// OnExchange, when set, is called for every HTTP round trip (used by the
	// load worker to collect timings without keeping every exchange).
	OnExchange func(Exchange)

	// Trace, when set, receives every request and response with the WBXML
	// bodies decoded (see Tracer). One Tracer can be shared by many sessions.
	Trace *Tracer

	Logger *slog.Logger
}

// Step is the outcome of one stage of the probe.
type Step struct {
	Name       string            `json:"name"`
	OK         bool              `json:"ok"`
	Skipped    bool              `json:"skipped,omitempty"`
	Duration   time.Duration     `json:"duration_ns"`
	Error      string            `json:"error,omitempty"`
	ErrorClass string            `json:"error_class,omitempty"`
	HTTPStatus int               `json:"http_status,omitempty"`
	Info       map[string]string `json:"info,omitempty"`
}

// ServerInfo is everything the probe could learn about the server side
// without being told.
type ServerInfo struct {
	URL             string            `json:"url"`
	Product         string            `json:"product,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Versions        []string          `json:"versions,omitempty"`
	Commands        []string          `json:"commands,omitempty"`
	ProtocolVersion string            `json:"protocol_version,omitempty"`
	TLSVersion      string            `json:"tls_version,omitempty"`
	Policy          string            `json:"policy,omitempty"`
}

// Result is the complete record of one probe run.
type Result struct {
	User       string        `json:"user"`
	DeviceID   string        `json:"device_id"`
	DeviceType string        `json:"device_type"`
	Started    time.Time     `json:"started"`
	Duration   time.Duration `json:"duration_ns"`
	OK         bool          `json:"ok"`
	Server     ServerInfo    `json:"server"`
	Steps      []Step        `json:"steps"`
	Exchanges  []Exchange    `json:"exchanges"`
	StatePath  string        `json:"state_path,omitempty"`
}

// Step returns the named step or nil.
func (r *Result) Step(name string) *Step {
	for i := range r.Steps {
		if r.Steps[i].Name == name {
			return &r.Steps[i]
		}
	}
	return nil
}

// preferred is the order in which protocol versions are chosen. 14.1 is the
// most widely implemented version the EAS library speaks fully.
var preferred = []string{"14.1", "14.0", "16.1", "16.0", "12.1", "12.0"}

// NormalizeServerURL turns "mail.example.com", "https://mail.example.com"
// or a full URL into the full EAS endpoint URL.
func NormalizeServerURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty server")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", s)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = DefaultEndpointPath
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// DeriveDeviceID builds a stable 32 hex character device id from the
// account and server, so repeated probes show up as one device on the server.
func DeriveDeviceID(user, serverURL string) string {
	sum := sha256.Sum256([]byte("easprobe:" + strings.ToLower(user) + "@" + strings.ToLower(serverURL)))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}

// DefaultStatePath returns the default location of the state file.
func DefaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "easprobe", "state.json")
}

// runner holds the mutable state of a single probe run.
type runner struct {
	cfg     Config
	profile Profile
	log     *slog.Logger
	rec     *recorder
	hc      *http.Client
	res     *Result
	sess    *Session
}

// Run executes the probe. It always returns a Result; the error is only
// non-nil for problems that prevented the probe from starting at all (bad
// configuration, unreadable state file). Failures of individual steps are
// reported inside the Result.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	profile, err := prepare(&cfg)
	if err != nil {
		return nil, err
	}

	hc, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	rec := &recorder{log: cfg.Logger, keep: true, hook: cfg.OnExchange, hdr: newHeaderSet(cfg.Headers)}
	hc.Transport = newTransport(hc.Transport, rec, cfg)

	r := &runner{
		cfg:     cfg,
		profile: profile,
		log:     cfg.Logger,
		rec:     rec,
		hc:      hc,
		res: &Result{
			User:       cfg.Username,
			DeviceType: cfg.DeviceType,
			Started:    time.Now(),
		},
	}

	start := time.Now()
	r.execute(ctx)
	r.res.Duration = time.Since(start)
	r.res.Exchanges = r.rec.all()
	r.finish()
	return r.res, nil
}

func newHTTPClient(cfg Config) (*http.Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()

	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		tc.InsecureSkipVerify = true
	}
	if cfg.CACertFile != "" {
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", cfg.CACertFile)
		}
		tc.RootCAs = pool
	}
	base.TLSClientConfig = tc

	// No client-wide timeout: Ping legitimately holds the request open.
	hc := &http.Client{Transport: base}
	addJar(hc)
	return hc, nil
}

// execute runs the steps in order; a failed step skips the ones that depend on it.
func (r *runner) execute(ctx context.Context) {
	failed := false

	if r.cfg.ServerURL == "" {
		if !r.run(ctx, StepAutodiscover, r.cfg.StepTimeout, r.autodiscover) {
			return
		}
	}

	r.sess = newSession(r.cfg, r.profile, r.hc, r.rec)
	r.res.Server.URL = r.cfg.ServerURL
	r.res.DeviceID = r.sess.DeviceID()
	r.res.StatePath = r.cfg.Store.Path()

	// Two probes of one device at the same time would corrupt each
	// other's sync keys.
	dev := r.sess.Device()
	if !dev.TryLock() {
		r.res.Steps = append(r.res.Steps, Step{
			Name:       "state",
			Error:      "another probe of device " + r.sess.DeviceID() + " is still running",
			ErrorClass: "busy",
		})
		return
	}
	defer dev.Unlock()

	if r.cfg.Fresh {
		dev.Reset()
	}

	steps := []struct {
		name    string
		timeout time.Duration
		fn      func(context.Context, *Step) error
		enabled bool
		// optional steps are reported when they fail, but do not stop the
		// steps after them.
		optional bool
	}{
		{StepOptions, r.cfg.StepTimeout, r.sess.Options, true, false},
		{StepProvision, r.cfg.StepTimeout, r.sess.Provision, true, false},
		{StepSettings, r.cfg.StepTimeout, r.sess.DeviceInfo, r.sess.HasDeviceInfo(), true},
		{StepFolderSync, r.cfg.StepTimeout, r.sess.FolderSync, true, false},
		{StepSync, r.cfg.StepTimeout, r.syncStep, true, false},
		{StepDirectory, r.cfg.StepTimeout, r.directoryStep, r.cfg.Directory != "off", true},
		{StepMail, r.cfg.MailWait + 2*r.cfg.StepTimeout, r.mailStep, r.cfg.Mail, true},
		{StepPing, time.Duration(r.cfg.PingSeconds)*time.Second + r.cfg.StepTimeout, r.pingStep, r.cfg.PingSeconds > 0, false},
	}

	for _, s := range steps {
		if !s.enabled {
			continue
		}
		if failed {
			r.res.Steps = append(r.res.Steps, Step{Name: s.name, Skipped: true})
			continue
		}
		if !r.run(ctx, s.name, s.timeout, s.fn) && !s.optional {
			failed = true
		}
	}
}

func (r *runner) syncStep(ctx context.Context, st *Step) error {
	_, err := r.sess.Sync(ctx, st)
	return err
}

// directoryStep looks for the account itself in the directory. In auto mode
// a server that does not offer Search skips the step.
func (r *runner) directoryStep(ctx context.Context, st *Step) error {
	if !r.sess.SupportsDirectory() {
		if r.cfg.Directory == "on" {
			return errors.New("server does not advertise the Search command")
		}
		st.Skipped = true
		put(st, "reason", "server does not advertise Search")
		return nil
	}
	query := r.cfg.DirectoryQuery
	if query == "" {
		query = r.cfg.Username
	}
	res, err := r.sess.Directory(ctx, st, query, true)
	if err != nil {
		return err
	}
	if res.Entries == 0 {
		return fmt.Errorf("directory search for %q returned no entries", res.Query)
	}
	if res.SelfChecked && !res.SelfFound {
		return fmt.Errorf("own user %s not found in the directory (%d entries for %q)", r.sess.selfAddress(), res.Entries, res.Query)
	}
	return nil
}

// mailStep sends a mail to the account itself and waits for it to come back.
func (r *runner) mailStep(ctx context.Context, st *Step) error {
	_, err := r.sess.MailRoundTrip(ctx, st, MailTripOptions{
		Wait:     r.cfg.MailWait,
		Arrival:  r.cfg.MailArrival,
		Keep:     r.cfg.MailKeep,
		SaveSent: r.cfg.MailKeep,
		SizeKB:   r.cfg.MailSizeKB,
	})
	return err
}

func (r *runner) pingStep(ctx context.Context, st *Step) error {
	_, err := r.sess.Ping(ctx, r.cfg.PingSeconds, st)
	return err
}

// run executes one step, timing it and classifying any error.
func (r *runner) run(ctx context.Context, name string, timeout time.Duration, fn func(context.Context, *Step) error) bool {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	logging.Trace(r.log, "step start", "name", name)
	r.rec.setStep(name)
	step := Step{Name: name, Info: map[string]string{}}
	t0 := time.Now()
	err := fn(sctx, &step)
	step.Duration = time.Since(t0)

	if err != nil {
		step.Error = ErrorText(err)
		step.ErrorClass = Classify(sctx, err)
		step.OK = false
	} else {
		step.OK = true
	}
	if len(step.Info) == 0 {
		step.Info = nil
	}

	// Last HTTP status seen during this step.
	for _, x := range r.rec.all() {
		if x.Step == name && x.Status != 0 {
			step.HTTPStatus = x.Status
		}
	}

	logging.Trace(r.log, "step done", "name", name, "ok", step.OK, "skipped", step.Skipped,
		"duration", step.Duration.Round(time.Millisecond), "http_status", step.HTTPStatus,
		"info", fmt.Sprint(step.Info), "err", step.Error)
	r.res.Steps = append(r.res.Steps, step)
	return step.OK
}

func (r *runner) finish() {
	ok := len(r.res.Steps) > 0
	for _, s := range r.res.Steps {
		if !s.OK && !s.Skipped {
			ok = false
		}
	}
	r.res.OK = ok

	if r.sess != nil {
		url := r.res.Server.URL
		r.res.Server = r.sess.Server()
		r.res.Server.URL = url
	}
	for _, x := range r.res.Exchanges {
		if x.TLSVersion != "" {
			r.res.Server.TLSVersion = x.TLSVersion
		}
	}
}

func (r *runner) autodiscover(ctx context.Context, s *Step) error {
	res, err := eas.Autodiscover(ctx, r.cfg.Email, r.cfg.Password, eas.AutodiscoverOptions{
		HTTPClient: r.hc,
		Logger:     r.log,
		UserAgent:  r.cfg.UserAgent,
	})
	if err != nil {
		return err
	}
	u, err := NormalizeServerURL(res.URL)
	if err != nil {
		return fmt.Errorf("autodiscover returned unusable URL %q: %w", res.URL, err)
	}
	r.cfg.ServerURL = u
	s.Info["url"] = u
	if res.DisplayName != "" {
		s.Info["display_name"] = res.DisplayName
	}
	return nil
}

// Classify maps an error to a short, stable class used as a metric label.
func Classify(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, ErrMailLate) {
		return "mail_late"
	}
	if errors.Is(err, ErrMailLost) {
		return "mail_lost"
	}

	var he *eas.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusUnauthorized:
			return "auth"
		case he.StatusCode == http.StatusForbidden:
			return "forbidden"
		case he.StatusCode == http.StatusNotFound:
			return "not_found"
		case he.StatusCode == 449:
			return "provision_required"
		case he.StatusCode >= 500:
			return "server_error"
		}
		return "http"
	}

	var se *eas.StatusError
	if errors.As(err, &se) {
		return "eas_status"
	}

	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &ci) {
		return "tls"
	}

	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "timeout"
		}
		return "network"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	return "error"
}

// SortedKeys is a small helper for stable rendering of Info maps.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RunDevices probes the account once per device: the first device with the
// first profile, the second with the next one (the profiles are used in turn),
// each its own device in the server's device list. It returns one Result per
// device, in device order.
//
// workers is how many devices may be probed at the same time (1: one after the
// other). The first device always runs alone, as the pilot: if the server
// refuses its login nothing else is started, so a wrong password costs one
// failed login, not one per worker. After that the other devices run with up to
// `workers` at a time; a refused login among them stops the devices that have
// not started yet.
func RunDevices(ctx context.Context, cfg Config, devices int, profiles []string, workers int) ([]*Result, error) {
	if devices < 1 {
		devices = 1
	}
	if workers < 1 {
		workers = 1
	}
	if len(profiles) == 0 {
		profiles = []string{cfg.Profile}
	}
	cfgFor := func(n int) Config {
		c := cfg
		c.Profile = profiles[(n-1)%len(profiles)]
		c.DeviceNumber = n
		if n > 1 {
			c.DeviceID = "" // each device number gets its own id
		}
		return c
	}

	// the pilot
	first, err := Run(ctx, cfgFor(1))
	if err != nil {
		return nil, err
	}
	results := []*Result{first}
	if devices == 1 || authFailed(first) || ctx.Err() != nil {
		return results, nil
	}

	if workers == 1 {
		for n := 2; n <= devices; n++ {
			res, err := Run(ctx, cfgFor(n))
			if err != nil {
				return results, err
			}
			results = append(results, res)
			if authFailed(res) || ctx.Err() != nil {
				break
			}
		}
		return results, nil
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		stop     bool
		firstErr error
	)
	rest := make([]*Result, devices-1)
	sem := make(chan struct{}, workers)
	for n := 2; n <= devices; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			mu.Lock()
			skip := stop || ctx.Err() != nil
			mu.Unlock()
			if skip {
				return
			}

			res, err := Run(ctx, cfgFor(n))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			rest[n-2] = res
			if authFailed(res) {
				stop = true
			}
		}(n)
	}
	wg.Wait()

	for _, r := range rest {
		if r != nil {
			results = append(results, r)
		}
	}
	return results, firstErr
}

// authFailed reports whether a step failed because the server refused the
// login (401) or the device/user (403).
func authFailed(res *Result) bool {
	for _, st := range res.Steps {
		if !st.OK && !st.Skipped && (st.ErrorClass == "auth" || st.ErrorClass == "forbidden") {
			return true
		}
	}
	return false
}
