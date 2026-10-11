package probe

import (
	"context"
	"errors"
	"fmt"
	"github.com/nashcom/eas-tools/internal/logging"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
)

// Session is one simulated or monitored device talking to one server: its
// own HTTP connection(s) and cookie jar, its EAS client and its state. It
// offers the ActiveSync steps as methods. Run uses it for the one-shot probe
// and the load worker keeps one Session per simulated device.
//
// A Session is used by one goroutine at a time.
//
// Every step method takes an optional *Step (may be nil). When given, the
// method records details of what it did in step.Info.
type Session struct {
	cfg     Config
	profile Profile
	log     *slog.Logger
	rec     *recorder
	hc      *http.Client
	state   *Device
	client  eas.Client
	inbox   string
	server  ServerInfo

	// windowOverride replaces cfg.SyncWindow while a mail test runs (see window).
	windowOverride int
	// watch counts the Inbox Syncs of the running mail test (nil when none runs).
	watch *mailWatch
}

// NewSession prepares a session. cfg.ServerURL is required (endpoint
// discovery is a step of Run, not of a session); defaults and validation are
// the same as for Run. cfg.OnExchange, when set, is called for every HTTP
// round trip, which is how the load worker collects its numbers without
// keeping them all in memory.
func NewSession(cfg Config) (*Session, error) {
	profile, err := prepare(&cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ServerURL == "" {
		return nil, errors.New("server URL is required")
	}

	hc, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	rec := &recorder{log: cfg.Logger, hook: cfg.OnExchange, hdr: newHeaderSet(cfg.Headers)}
	hc.Transport = newTransport(hc.Transport, rec, cfg)

	return newSession(cfg, profile, hc, rec), nil
}

func newSession(cfg Config, profile Profile, hc *http.Client, rec *recorder) *Session {
	if cfg.DeviceID == "" {
		cfg.DeviceID = deviceIDFor(cfg)
	}
	return &Session{
		cfg:     cfg,
		profile: profile,
		log:     cfg.Logger,
		rec:     rec,
		hc:      hc,
		state:   cfg.Store.Device(cfg.DeviceID),
	}
}

// deviceIDFor derives the stable device id. An imitated device type must
// not reuse the record of another type, so the type is part of the id (the
// plain "easprobe" type keeps the plain derivation).
func deviceIDFor(cfg Config) string {
	seed := cfg.ServerURL
	if !strings.EqualFold(cfg.DeviceType, "easprobe") {
		seed += "#" + strings.ToLower(cfg.DeviceType)
	}
	// The first device keeps the plain derivation, so adding more devices to
	// a user later does not change the id (and registration) of the first.
	if cfg.DeviceNumber > 1 {
		seed += "#device" + strconv.Itoa(cfg.DeviceNumber)
	}
	return DeriveDeviceID(cfg.Username, seed)
}

// prepare validates the config and fills in the defaults.
func prepare(cfg *Config) (Profile, error) {
	if cfg.Username == "" {
		cfg.Username = cfg.Email
	}
	if cfg.Username == "" {
		return Profile{}, errors.New("user name is required")
	}
	if cfg.Password == "" {
		return Profile{}, errors.New("password is required")
	}
	if cfg.ServerURL == "" && !strings.Contains(cfg.Email, "@") {
		return Profile{}, errors.New("either a server or an e-mail address (for autodiscover) is required")
	}
	profile, ok := LookupProfile(cfg.Profile)
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q (available: %s)", cfg.Profile, strings.Join(ProfileNames(), ", "))
	}
	if cfg.DeviceType == "" {
		cfg.DeviceType = profile.DeviceType
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = profile.UserAgent
	}
	if cfg.Provision == "" {
		cfg.Provision = ProvisionAuto
	}
	if cfg.MailWait <= 0 {
		cfg.MailWait = 60 * time.Second
	}
	if cfg.MailArrival == "" {
		cfg.MailArrival = "poll"
	}
	if cfg.MailArrival != "push" && cfg.MailArrival != "poll" {
		return Profile{}, fmt.Errorf("invalid mail arrival mode %q", cfg.MailArrival)
	}
	if cfg.MailSizeKB <= 0 {
		cfg.MailSizeKB = 2
	}
	if cfg.Directory == "" {
		cfg.Directory = "auto"
	}
	if cfg.Directory != "auto" && cfg.Directory != "on" && cfg.Directory != "off" {
		return Profile{}, fmt.Errorf("invalid directory mode %q", cfg.Directory)
	}
	if cfg.Provision != ProvisionAuto && cfg.Provision != ProvisionAlways && cfg.Provision != ProvisionNever {
		return Profile{}, fmt.Errorf("invalid provision mode %q", cfg.Provision)
	}
	if cfg.SyncWindow <= 0 {
		cfg.SyncWindow = 5
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Store == nil {
		cfg.Store, _ = OpenStore("")
	}

	if cfg.ServerURL != "" {
		u, err := NormalizeServerURL(cfg.ServerURL)
		if err != nil {
			return Profile{}, fmt.Errorf("server: %w", err)
		}
		cfg.ServerURL = u
	}

	// Never log the password, but its shape explains most 401s that curl
	// does not show (shell quoting, trailing blanks).
	logging.Trace(cfg.Logger, "credentials", "user", cfg.Username, "password_length", len(cfg.Password),
		"surrounding_whitespace", cfg.Password != strings.TrimSpace(cfg.Password))
	return profile, nil
}

// put records a detail in the step, if there is one.
func put(st *Step, key, value string) {
	if st == nil {
		return
	}
	if st.Info == nil {
		st.Info = map[string]string{}
	}
	st.Info[key] = value
}

// DeviceID returns the device id of this session.
func (s *Session) DeviceID() string { return s.cfg.DeviceID }

// Device returns the stored state of this session's device.
func (s *Session) Device() *Device { return s.state }

// Inbox returns the Inbox folder id, or "" while it is unknown.
func (s *Session) Inbox() string {
	if s.inbox != "" {
		return s.inbox
	}
	return s.state.InboxID()
}

// Server returns what the session detected about the server so far.
func (s *Session) Server() ServerInfo { return s.server }

// Provisioned reports whether the device has a stored policy key.
func (s *Session) Provisioned() bool { return s.state.HasPolicyKey() }

// Close releases the idle connections of the session.
func (s *Session) Close() {
	s.hc.CloseIdleConnections()
}

func (s *Session) newClient(version string) (eas.Client, error) {
	return eas.NewClient(eas.Config{
		ServerURL:    s.cfg.ServerURL,
		Username:     s.cfg.Username,
		Password:     s.cfg.Password,
		DeviceID:     s.cfg.DeviceID,
		DeviceType:   s.cfg.DeviceType,
		ASVersion:    version,
		UserAgent:    s.cfg.UserAgent,
		HTTPClient:   s.hc,
		Logger:       logging.DebugAsTrace(s.log), // the library logs every request and response as debug: that is trace here
		State:        s.state,
		GzipRequests: false,
	})
}

// ensureClient creates the EAS client with the configured (or default)
// protocol version when Options has not been run.
func (s *Session) ensureClient() error {
	if s.client != nil {
		return nil
	}
	version := s.cfg.ASVersion
	if version == "" {
		version = "14.1"
	}
	c, err := s.newClient(version)
	if err != nil {
		return err
	}
	s.client = c
	return nil
}

// Options discovers the protocol versions and commands, picks the protocol
// version and detects the server software.
func (s *Session) Options(ctx context.Context, st *Step) error {
	s.rec.setStep(StepOptions)

	c, err := s.newClient("14.1")
	if err != nil {
		return err
	}
	opts, err := c.Options(ctx)
	if err != nil {
		return err
	}

	s.server.Versions = opts.ProtocolVersions
	s.server.Commands = opts.Commands
	put(st, "versions", strings.Join(opts.ProtocolVersions, ","))
	put(st, "commands", fmt.Sprintf("%d", len(opts.Commands)))

	s.captureHeaders()

	version := s.cfg.ASVersion
	if version == "" {
		for _, v := range preferred {
			if opts.Supports(v) {
				version = v
				break
			}
		}
		if version == "" {
			return fmt.Errorf("server offers protocol versions %v; none supported by the client (%v)", opts.ProtocolVersions, preferred)
		}
	} else if !opts.Supports(version) {
		return fmt.Errorf("server does not offer requested protocol version %s (offers %v)", version, opts.ProtocolVersions)
	}
	s.server.ProtocolVersion = version
	put(st, "selected", version)

	for _, need := range []string{"Provision", "FolderSync", "Sync"} {
		if !opts.HasCommand(need) {
			return fmt.Errorf("server does not advertise required command %s", need)
		}
	}

	s.client, err = s.newClient(version)
	return err
}

// captureHeaders records server identifying headers from the OPTIONS reply.
func (s *Session) captureHeaders() {
	s.rec.mu.Lock()
	h := s.rec.optionsHdr
	s.rec.mu.Unlock()
	if h == nil {
		return
	}
	keep := []string{"Server", "X-Powered-By", "MS-Server-ActiveSync", "X-AspNet-Version", "Via",
		"X-IBM-TRAVELER-HTTP-TIME"}
	out := map[string]string{}
	for _, k := range keep {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	s.server.Headers = out
	s.server.Product = DetectProduct(h)
}

// DetectProduct makes a best-effort guess of the server software from the
// headers of an OPTIONS reply.
func DetectProduct(h http.Header) string {
	all := strings.ToLower(h.Get("Server") + " " + h.Get("X-Powered-By") + " " + h.Get("MS-Server-ActiveSync") + " " + h.Get("Via"))
	switch {
	case h.Get("X-IBM-TRAVELER-COMMANDS") != "" || h.Get("X-IBM-TRAVELER-HTTP-TIME") != "":
		// Traveler marks its replies even behind a reverse proxy.
		return "HCL Traveler (MS-Server-ActiveSync " + h.Get("MS-Server-ActiveSync") + ")"
	case strings.Contains(all, "domino") || strings.Contains(all, "traveler") || strings.Contains(all, "lotus"):
		return "HCL Domino / Traveler"
	case strings.Contains(all, "z-push"):
		return "Z-Push"
	case strings.Contains(all, "grommunio"):
		return "grommunio-sync"
	case strings.Contains(all, "sogo"):
		return "SOGo"
	case h.Get("MS-Server-ActiveSync") != "":
		return "Microsoft Exchange (MS-Server-ActiveSync " + h.Get("MS-Server-ActiveSync") + ")"
	}
	return ""
}

// Provision registers the device. A server that has no policy for the
// device (policy status 2) is a valid outcome and is reported as such. When
// the mode is auto and the device is already provisioned, nothing is sent
// and st.Skipped is set.
func (s *Session) Provision(ctx context.Context, st *Step) error {
	s.rec.setStep(StepProvision)
	if err := s.ensureClient(); err != nil {
		return err
	}

	had := s.state.HasPolicyKey()
	if s.cfg.Provision == ProvisionNever || (s.cfg.Provision == ProvisionAuto && had) {
		reason := "already provisioned"
		if s.cfg.Provision == ProvisionNever {
			reason = "disabled"
		}
		if st != nil {
			st.Skipped = true
		}
		put(st, "reason", reason)
		put(st, "policy_key_stored", fmt.Sprintf("%t", had))
		s.server.Policy = "stored"
		return nil
	}

	err := s.client.Provision(ctx)
	if err != nil {
		var se *eas.StatusError
		if errors.As(err, &se) && se.Command == "Provision/Policy" && se.Code == 2 {
			put(st, "policy", "none required")
			s.server.Policy = "none"
			return nil
		}
		return err
	}

	s.server.Policy = "provisioned"
	put(st, "new_registration", fmt.Sprintf("%t", !had))
	if p := s.client.LastPolicy(); p != nil {
		put(st, "policy_received", "true")
	}
	return nil
}

// HasDeviceInfo reports whether the profile sends DeviceInformation.
func (s *Session) HasDeviceInfo() bool { return s.profile.Info != nil }

// DeviceInfo sends the DeviceInformation a real phone sends after Provision,
// so Traveler shows a proper model and OS version for the device.
func (s *Session) DeviceInfo(ctx context.Context, st *Step) error {
	s.rec.setStep(StepSettings)
	if s.profile.Info == nil {
		return errors.New("profile has no device information")
	}
	if err := s.ensureClient(); err != nil {
		return err
	}

	info := *s.profile.Info
	info.UserAgent = s.cfg.UserAgent
	if err := s.client.SettingsDeviceInformation(ctx, info); err != nil {
		return err
	}
	put(st, "model", info.Model)
	put(st, "os", info.OS)
	return nil
}

// FolderSync fetches the folder hierarchy and remembers the Inbox.
func (s *Session) FolderSync(ctx context.Context, st *Step) error {
	s.rec.setStep(StepFolderSync)
	if err := s.ensureClient(); err != nil {
		return err
	}

	before, _ := s.state.SyncKey(ctx, eas.FolderRootID)

	res, err := s.client.FolderSync(ctx)
	if statusIs(err, "FolderSync", 9) {
		// Status 9: the server does not accept our synchronization key (its state
		// was lost or recreated). MS-ASCMD: return to sync key 0. The keys of all
		// folders belong to the old hierarchy, so they go as well.
		s.log.Warn("FolderSync status 9 (stale sync key): rebuilding the folder hierarchy from sync key 0")
		s.state.ResetHierarchy()
		put(st, "resync", "hierarchy rebuilt (FolderSync status 9: stale sync keys)")
		res, err = s.client.FolderSync(ctx)
	}
	if err != nil {
		return err
	}
	s.state.MergeFolders(res)

	// An incremental FolderSync does not repeat folders we have forgotten
	// (state loss); start the hierarchy over once in that case.
	if s.state.InboxID() == "" && before != "0" {
		if err := s.state.SetSyncKey(ctx, eas.FolderRootID, "0"); err != nil {
			return err
		}
		res, err = s.client.FolderSync(ctx)
		if err != nil {
			return err
		}
		s.state.MergeFolders(res)
		put(st, "resync", "hierarchy rebuilt")
	}
	s.rec.setStep(StepFolderSync)

	s.inbox = s.state.InboxID()
	put(st, "folders", fmt.Sprintf("%d", s.state.FolderCount()))
	put(st, "added", fmt.Sprintf("%d", len(res.Added)))
	put(st, "updated", fmt.Sprintf("%d", len(res.Updated)))
	put(st, "deleted", fmt.Sprintf("%d", len(res.Deleted)))
	put(st, "by_type", s.state.TypeSummary())

	if s.inbox == "" {
		return errors.New("server returned no Inbox folder")
	}
	put(st, "inbox", s.inbox)
	return nil
}

// SyncResult is the outcome of one Sync round trip of the Inbox.
type SyncResult struct {
	FirstSync     bool // the stored sync key was "0" before this call
	Added         int
	Changed       int
	Deleted       int
	MoreAvailable bool // the server has more items; call Sync again to continue
	SyncKey       string
	Mails         []MailRef // the new and changed mails of this round trip
}

// MailRef identifies a mail seen by Sync.
type MailRef struct {
	ServerID string
	Subject  string
	Received time.Time // when the server says it received the mail (DateReceived); zero when it did not say
}

// Sync runs one incremental Sync of the Inbox. The very first call of a
// device also does the bootstrap round trip that the EAS library performs.
func (s *Session) Sync(ctx context.Context, st *Step) (*SyncResult, error) {
	s.rec.setStep(StepSync)
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	inbox := s.Inbox()
	if inbox == "" {
		return nil, errors.New("Inbox unknown: run FolderSync first")
	}

	key, _ := s.state.SyncKey(ctx, inbox)
	out := &SyncResult{FirstSync: key == "0"}
	put(st, "first_sync", fmt.Sprintf("%t", out.FirstSync))

	syncOnce := func(folder string) (*eas.EmailSyncResult, error) {
		return s.client.SyncEmail(ctx, folder, eas.EmailSyncOptions{
			WindowSize:         s.window(),
			BodyType:           eas.BodyTypePlain,
			BodyTruncationSize: 256,
		})
	}
	res, err := syncOnce(inbox)
	if statusIs(err, "Sync", 12) {
		// Status 12: the folder hierarchy changed. MS-ASCMD: run FolderSync, then retry.
		s.log.Warn("Sync status 12 (folder hierarchy changed): running FolderSync and retrying")
		if ferr := s.FolderSync(ctx, nil); ferr != nil {
			return nil, ferr
		}
		s.rec.setStep(StepSync)
		put(st, "resync", "folder hierarchy refreshed (Sync status 12)")
		inbox = s.Inbox()
		res, err = syncOnce(inbox)
	}
	if err != nil {
		return nil, err
	}
	out.Added = len(res.Added)
	out.Changed = len(res.Changed)
	out.Deleted = len(res.Deleted)
	out.MoreAvailable = res.MoreAvailable
	out.SyncKey = res.SyncKey
	for _, m := range res.Added {
		out.Mails = append(out.Mails, MailRef{ServerID: m.ServerID, Subject: m.Subject, Received: m.DateReceived})
	}
	for _, m := range res.Changed {
		out.Mails = append(out.Mails, MailRef{ServerID: m.ServerID, Subject: m.Subject, Received: m.DateReceived})
	}

	put(st, "added", fmt.Sprintf("%d", out.Added))
	put(st, "changed", fmt.Sprintf("%d", out.Changed))
	put(st, "deleted", fmt.Sprintf("%d", out.Deleted))
	put(st, "more_available", fmt.Sprintf("%t", out.MoreAvailable))
	put(st, "sync_key", out.SyncKey)
	return out, nil
}

// PingResult is the outcome of a Ping.
type PingResult struct {
	Held            time.Duration
	Changes         bool // status 2: the server reports changes
	ChangedFolders  []string
	HeartbeatUsed   int
	HeartbeatChange bool // the server's limit made us change the heartbeat
}

// Ping holds a Ping request open. A server that honors the heartbeat answers
// with status 1 after about that many seconds. Per MS-ASCMD, status 5 means the
// heartbeat is outside the server's range and the response carries the limit,
// which is adopted once (automatic detection); status 7 means the folder
// hierarchy is out of date: FolderSync, then Ping again.
func (s *Session) Ping(ctx context.Context, heartbeatSeconds int, st *Step) (*PingResult, error) {
	s.rec.setStep(StepPing)
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	if s.Inbox() == "" {
		return nil, errors.New("Inbox unknown: run FolderSync first")
	}

	hb := heartbeatSeconds
	out := &PingResult{}
	adjusted, resynced := false, false

	for {
		folders := []eas.PingFolder{{ID: s.Inbox(), Class: "Email"}}
		t0 := time.Now()
		res, err := s.client.Ping(ctx, hb, folders)
		// The library returns the parsed result together with an error for every
		// status except 1 and 2: the status (and the limit it may carry) is what
		// we need, so only a missing result is a real failure.
		if res == nil || (err != nil && res.Status == 0) {
			return nil, err
		}
		out.Held = time.Since(t0)
		out.HeartbeatUsed = hb

		put(st, "heartbeat_requested", fmt.Sprintf("%d", hb))
		put(st, "held_seconds", fmt.Sprintf("%.1f", out.Held.Seconds()))
		put(st, "ping_status", fmt.Sprintf("%d", res.Status))

		switch res.Status {
		case 1:
			put(st, "result", "no changes")
			return out, nil
		case 2:
			out.Changes = true
			out.ChangedFolders = res.ChangedFolders
			put(st, "result", "changes available")
			put(st, "changed_folders", strings.Join(res.ChangedFolders, ","))
			return out, nil
		case 5:
			if res.HeartbeatInterval <= 0 || adjusted {
				return nil, fmt.Errorf("ping: heartbeat %ds refused (status 5: %s)", hb, StatusName("Ping", 5))
			}
			put(st, "heartbeat_adjusted_from", fmt.Sprintf("%d", hb))
			hb = res.HeartbeatInterval
			adjusted = true
			out.HeartbeatChange = true
			put(st, "heartbeat_adjusted_to", fmt.Sprintf("%d", hb))
		case 7:
			if resynced {
				return nil, errors.New("ping: folder hierarchy still out of date after FolderSync (status 7)")
			}
			s.log.Warn("Ping status 7 (folder hierarchy out of date): running FolderSync and retrying")
			if ferr := s.FolderSync(ctx, nil); ferr != nil {
				return nil, ferr
			}
			s.rec.setStep(StepPing)
			resynced = true
			put(st, "resync", "folder hierarchy refreshed (Ping status 7)")
		default:
			return nil, fmt.Errorf("ping: status %d (%s)", res.Status, StatusName("Ping", res.Status))
		}
	}
}

// newHTTPClientJar adds a cookie jar: Traveler sets a SessionID cookie and
// real devices send it back, which saves a full authentication per request.
func addJar(hc *http.Client) {
	if jar, err := cookiejar.New(nil); err == nil {
		hc.Jar = jar
	}
}

// DirectoryResult is the outcome of a directory (GAL) lookup.
type DirectoryResult struct {
	Query       string // the query that produced the result
	Entries     int
	Total       int
	SelfChecked bool // the lookup looked for the account's own address
	SelfFound   bool
	First       string   // display name of the first entry, for diagnostics
	Addresses   []string // e-mail addresses of the entries
}

// selfAddress is the e-mail address of the account, if known: the Email of
// the config, else the user name when it looks like an address.
func (s *Session) selfAddress() string {
	if strings.Contains(s.cfg.Email, "@") {
		return s.cfg.Email
	}
	if strings.Contains(s.cfg.Username, "@") {
		return s.cfg.Username
	}
	return ""
}

// SupportsDirectory reports whether the server advertised the Search command
// (false only when OPTIONS ran and did not list it).
func (s *Session) SupportsDirectory() bool {
	if len(s.server.Commands) == 0 {
		return true // unknown: try
	}
	for _, c := range s.server.Commands {
		if c == "Search" {
			return true
		}
	}
	return false
}

// Directory searches the server's directory (the Global Address List) for
// query. With checkSelf the result tells whether the account's own address
// is among the entries; if the full query finds nothing and is an address,
// the part before the @ is tried as well (some directories do not match on
// the domain).
func (s *Session) Directory(ctx context.Context, st *Step, query string, checkSelf bool) (*DirectoryResult, error) {
	s.rec.setStep(StepDirectory)
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("directory query is empty")
	}

	queries := []string{query}
	if local, _, ok := strings.Cut(query, "@"); ok && local != "" {
		queries = append(queries, local)
	}

	out := &DirectoryResult{}
	self := s.selfAddress()
	for _, q := range queries {
		res, err := s.client.GALSearch(ctx, q, 25)
		if err != nil {
			return nil, err
		}
		out.Query, out.Entries, out.Total = q, len(res.Entries), res.Total
		out.Addresses = out.Addresses[:0]
		for _, e := range res.Entries {
			if e.EmailAddress != "" {
				out.Addresses = append(out.Addresses, e.EmailAddress)
			}
		}
		if len(res.Entries) > 0 {
			out.First = res.Entries[0].DisplayName
		}
		if checkSelf && self != "" {
			out.SelfChecked = true
			for _, e := range res.Entries {
				if strings.EqualFold(e.EmailAddress, self) {
					out.SelfFound = true
					break
				}
			}
		}
		if len(res.Entries) > 0 && (!out.SelfChecked || out.SelfFound) {
			break
		}
	}

	put(st, "query", out.Query)
	put(st, "results", fmt.Sprintf("%d", out.Entries))
	if out.First != "" {
		put(st, "first", out.First)
	}
	if out.SelfChecked {
		put(st, "self_found", fmt.Sprintf("%t", out.SelfFound))
	}
	return out, nil
}

// Username returns the login name of this session.
func (s *Session) Username() string { return s.cfg.Username }

// window is the number of Inbox items asked for per Sync. A mail test uses a
// larger window, so that an Inbox with many other mails needs few round trips
// to reach the test mail.
func (s *Session) window() int {
	if s.windowOverride > 0 {
		return s.windowOverride
	}
	return s.cfg.SyncWindow
}
