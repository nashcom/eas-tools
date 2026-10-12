// Package exporter is the HTTP front end that turns the probe of eas-tools into a
// Prometheus blackbox-style exporter: Prometheus scrapes /probe?target=NAME
// and gets the metrics of one ActiveSync probe of that configured account.
package exporter

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/internal/listenaddr"
	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/probe"
	"log/slog"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config is the exporter configuration file (JSON).
type Config struct {
	// Listen is the main listen address. Default: ":9117" with TLS (the default: the certificate of tls_cert, or the one in /run/secrets/tls), and
	// "127.0.0.1:9116" with allow_http: unencrypted is loopback only unless an address that is not loopback is named.
	Listen string `json:"listen"`
	// AllowFrom restricts the listener to these addresses: single addresses, networks (192.168.0.0/16, 2001:db8::/32), ranges
	// (10.0.0.5-10.0.0.20) and the name private (RFC 1918 and fc00::/7). It guards every endpoint. The loopback interface is always
	// allowed. Empty means no restriction. A reload applies a change. The address is the one of the connection: behind a proxy it is
	// the proxy.
	AllowFrom []string `json:"allow_from"`
	// TLSCert and TLSKey make the listener serve HTTPS (both, or none). The files are watched: a renewed pair is loaded without a restart,
	// every TLSCheckSeconds (default 60) and on SIGHUP. A pair that does not fit or is expired is not taken; the one in use stays. A change
	// of the two file names needs a restart.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
	// TLSGenerate makes a small CA and a certificate when the two files are not there (never overwrites them); TLSNames are the names of
	// that certificate (default: localhost, the loopback addresses and the host name). The clients have to trust the CA, ca.crt next to it.
	TLSGenerate     bool     `json:"tls_generate"`
	TLSNames        []string `json:"tls_names"`
	TLSCheckSeconds int      `json:"tls_check_interval_seconds"`
	// TLSUpdateFrom is the host and port ("certmgr.example.com:443") that is asked for a renewed certificate: the program connects with
	// TLSUpdateName (default: the host name of this machine, never a name from a certificate) as SNI, and takes the certificate it gets when it is for the
	// key in use, is valid and is newer. It writes tls_cert; the key is never touched. It is asked at the start, every
	// TLSUpdateSeconds (default 3600), and when the configuration is checked on request or on SIGHUP.
	TLSUpdateFrom    string `json:"tls_update_from"`
	TLSUpdateName    string `json:"tls_update_name"`
	TLSUpdateSeconds int    `json:"tls_update_interval_seconds"`
	// TLSValidDays is how long a certificate that the micro CA makes is valid (default 365, from 1 to 3650). It is made again at every start. It does
	// not apply to a certificate of your own.
	TLSValidDays int `json:"tls_valid_days"`
	// TLSAuto is set when the certificate was not named: the default files are used and made when they are not there (and made again at
	// every start when they are ours). It is not a setting.
	TLSAuto bool `json:"-"`
	// ListenMetrics is the metrics port: a plain HTTP listener of its own that serves /metrics and /health and nothing else, always on, with
	// TLS or without. /metrics is also served by the main listener, Listen. Default: :9100, on all interfaces: it is open, and AllowFromMetrics
	// restricts it. A scraper on another host or container reaches it there; the peers that may connect are named in AllowFromMetrics (the
	// address of Prometheus). It is a list like AllowFrom, and applies to this listener alone. A reload applies a changed list; a changed
	// address needs a restart.
	ListenMetrics    string   `json:"listen_metrics"`
	AllowFromMetrics []string `json:"allow_from_metrics"`
	// AllowHTTP is the choice to serve everything over plain HTTP, which is only for a proxy that terminates TLS. Without a certificate the
	// exporter refuses to start on an address that is not loopback unless this is true; with a certificate it is an error: a listener that
	// has TLS speaks nothing else.
	AllowHTTP bool `json:"allow_http"`

	// LogLevel is warn (problems only), info (the default), debug or trace. The command line (-verbose, -debug) wins over it. A
	// reload applies a changed level at once.
	LogLevel string `json:"log_level"`
	// LogFormat is text (the default) or json: one object per line with the names of OpenTelemetry's JSON encoding. The command
	// line (-log-format) wins over it, and a reload applies a changed format at once.
	LogFormat string `json:"log_format"`

	// AuthTokenFile / AuthTokenEnv give the bearer token that protects
	// /probe. If neither is set, /probe is open. /health and /metrics are
	// always open.
	AuthTokenFile string `json:"auth_token_file"`
	AuthTokenEnv  string `json:"auth_token_env"`
	// AuthToken is the token itself (the simplest way, like "password" of a target; the config file then has to be
	// readable only by its owner). AuthTokenName is the optional name of the header that carries it: by default
	// "Authorization: Bearer <token>", for another name just "<name>: <token>", for a server where Authorization is taken.
	AuthToken     string `json:"auth_token"`
	AuthTokenName string `json:"auth_token_name"`

	// StateFile holds policy keys and sync keys of all targets so that a
	// restart does not register the devices again. Empty = memory only.
	StateFile string `json:"state_file"`
	// AutoSaveSeconds is the state snapshot interval (default 30).
	AutoSaveSeconds int `json:"autosave_seconds"`

	// MinIntervalSeconds is the shortest time between two real probes of one
	// target (default 60). A scrape inside the window gets the cached result,
	// so a short scrape interval cannot hammer the server.
	MinIntervalSeconds int `json:"min_interval_seconds"`
	// AuthBackoffSeconds is how long a target is left alone after a failed
	// login (default 600), so that a wrong password is not retried every
	// minute until the account or the client address gets blocked.
	AuthBackoffSeconds int `json:"auth_failure_backoff_seconds"`
	// MaxConcurrent limits probes running at the same time (default 10).
	MaxConcurrent int `json:"max_concurrent"`
	// MaxPingSeconds caps the ?ping= parameter (default 120).
	MaxPingSeconds int `json:"max_ping_seconds"`
	// DefaultTimeoutSeconds is used when Prometheus sends no scrape timeout
	// header (default 30).
	DefaultTimeoutSeconds int `json:"default_timeout_seconds"`

	// Insecure / CAFile apply to all targets unless a target overrides them.
	Insecure bool   `json:"insecure"`
	CAFile   string `json:"ca_file"`

	Targets TargetList `json:"targets"`

	// MailMonitor are the targets of the mail monitor: mails sent from the account of a target to an address outside,
	// which something there answers. The round-trip time is in /metrics. Empty = the probe is off.
	MailMonitor MailMonitorList `json:"mail_monitor"`
	// MailMonitorPollSeconds is the time between two looks at the Inbox while a mail waits for its answer (default 2).
	MailMonitorPollSeconds int `json:"mail_monitor_poll_seconds"`

	// Warnings are problems that do not stop the start, such as an unknown EASEXPORTER_ variable; the command logs them.
	Warnings []string `json:"-"`
}

// Target is one monitored ActiveSync account.
type Target struct {
	Server   string `json:"server"` // host or URL; empty = autodiscover from Email
	Email    string `json:"email"`
	User     string `json:"user"` // login, default Email
	Profile  string `json:"profile"`
	DeviceID string `json:"device_id"`

	// The password is best kept in this config file ("password", the simplest way), or comes from a separate file (first
	// line; it can be a Docker secret) or from an environment variable whose name is given. A file wins over the password,
	// and the password over the variable. A config file with a password has to be readable only by its owner and must
	// not be committed.
	PasswordFile string `json:"password_file"`
	Password     string `json:"password"`
	PasswordEnv  string `json:"password_env"`
	Provision    string `json:"provision"` // auto | always | never
	Directory    string `json:"directory"` // auto | on | off: look for the own user in the directory
	// Mail allows /probe?target=NAME&mail=1: a probe that sends a mail to the
	// account itself, waits for it to come back (push) and deletes it. It
	// creates mail, so it is off unless enabled here; scrape it rarely
	// (every 10 to 15 minutes) with its own job.
	Mail            bool   `json:"mail"`
	MailWaitSeconds int    `json:"mail_wait_seconds"` // default 60
	DirectoryQuery  string `json:"directory_query"`   // default: the user name
	Insecure        *bool  `json:"insecure"`
	CAFile          string `json:"ca_file"`
	// Headers are extra request headers for every request of this target, for
	// example {"X-Mock-Sim": "cmd=Sync;status=3;count=1"} to test against the mock
	// server. Authorization and Cookie are not allowed.
	Headers map[string]string `json:"headers"`
}

var targetName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// LoadConfig reads the config file, applies the EASEXPORTER_ environment variables over it (see env.go), defaults and
// validates the configuration. The file may be missing when the environment defines the target.
func LoadConfig(path string) (*Config, error) {
	return LoadConfigEnv(path, os.Environ())
}

// LoadConfigEnv is LoadConfig with the environment given as "NAME=value" lines.
func LoadConfigEnv(path string, environ []string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || !hasEnvTarget(environ) {
			return nil, err
		}
		raw = []byte("{}")
	}
	return LoadConfigBytes(raw, path, environ)
}

// LoadConfigBytes parses a configuration document (the name is only for messages), applies the environment variables
// over it, defaults and validates it. The environment wins, so a configuration from a remote location can be changed
// or completed locally, for example with the password of a target.
func LoadConfigBytes(raw []byte, name string, environ []string) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	warnings, err := applyEnv(&c, environ)
	if err != nil {
		return nil, err
	}
	c.Warnings = warnings

	// TLS is the default: without a certificate named, and without the choice of plain HTTP, and unless the listen address is loopback (which
	// never leaves the machine), the certificate is looked for in /run/secrets/tls, and made there when it is not
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("tls_cert and tls_key go together")
	}
	if c.TLSCert == "" && !c.AllowHTTP && !(c.Listen != "" && listenaddr.Loopback(c.Listen)) {
		c.TLSCert, c.TLSKey = certreload.DefaultFiles()
		c.TLSAuto = true
	}
	if c.Listen == "" { // the well-known ports: 9117 for HTTPS, 9116 for plain HTTP
		c.Listen = DefaultListenPlain
		if c.TLSCert != "" {
			c.Listen = DefaultListenTLS
		}
	}
	if c.ListenMetrics == "" {
		c.ListenMetrics = DefaultListenMetrics
	}
	if c.AutoSaveSeconds <= 0 {
		c.AutoSaveSeconds = 30
	}
	if c.MinIntervalSeconds <= 0 {
		c.MinIntervalSeconds = 60
	}
	if c.AuthBackoffSeconds <= 0 {
		c.AuthBackoffSeconds = 600
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 10
	}
	if c.MaxPingSeconds <= 0 {
		c.MaxPingSeconds = 120
	}
	if c.DefaultTimeoutSeconds <= 0 {
		c.DefaultTimeoutSeconds = 30
	}

	if _, err := logging.Parse(c.LogLevel, slog.LevelInfo); err != nil {
		return nil, err
	}
	if err := logging.ParseFormat(c.LogFormat); err != nil {
		return nil, err
	}

	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, errors.New("tls_cert and tls_key go together")
	}
	if c.TLSGenerate && c.TLSCert == "" {
		return nil, errors.New("tls_generate makes the certificate of a listener that has TLS: it excludes allow_http and a loopback listen address, where there is none")
	}
	if c.TLSCheckSeconds <= 0 {
		c.TLSCheckSeconds = 60
	}
	if c.TLSUpdateSeconds <= 0 {
		c.TLSUpdateSeconds = int(certreload.DefaultUpdateEvery / time.Second)
	}
	if c.TLSUpdateFrom != "" {
		if host, port, err := net.SplitHostPort(c.TLSUpdateFrom); err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("tls_update_from: %q is not host:port", c.TLSUpdateFrom)
		}
		if c.AllowHTTP {
			return nil, errors.New("tls_update_from renews the certificate of a listener that has TLS: it excludes allow_http")
		}
	}
	if c.TLSValidDays == 0 {
		c.TLSValidDays = certreload.DefaultLeafDays
	}
	if c.TLSValidDays < 1 || c.TLSValidDays > certreload.MaxLeafDays {
		return nil, fmt.Errorf("tls_valid_days: %d, use a number from 1 to %d (default %d)", c.TLSValidDays, certreload.MaxLeafDays, certreload.DefaultLeafDays)
	}
	if _, err := ipacl.Parse(c.AllowFrom); err != nil {
		return nil, fmt.Errorf("allow_from: %v", err)
	}
	if _, err := ipacl.Parse(c.AllowFromMetrics); err != nil {
		return nil, fmt.Errorf("allow_from_metrics: %v", err)
	}
	if c.TLSCert != "" && c.AllowHTTP {
		return nil, errors.New("allow_http and tls_cert exclude each other: a listener that has TLS speaks nothing else")
	}
	metrics, err := listenaddr.Second(c.Listen, c.ListenMetrics, c.AllowFromMetrics, "allow_from_metrics", true)
	if err != nil {
		return nil, fmt.Errorf("listen_metrics: %v", err)
	}
	c.ListenMetrics = metrics

	if _, err := tokenHeader(c.AuthTokenName); err != nil {
		return nil, fmt.Errorf("auth_token_name: %v", err)
	}

	if len(c.Targets) == 0 {
		return nil, errors.New("no targets configured")
	}
	for name, t := range c.Targets {
		if !targetName.MatchString(name) {
			return nil, fmt.Errorf("target %q: name may only contain letters, digits, '.', '_', '-'", name)
		}
		if t.Server == "" && !strings.Contains(t.Email, "@") {
			return nil, fmt.Errorf("target %q: needs \"server\" or an \"email\" for autodiscover", name)
		}
		if t.User == "" && t.Email == "" {
			return nil, fmt.Errorf("target %q: needs \"user\" or \"email\"", name)
		}
		if t.MailWaitSeconds < 0 || t.MailWaitSeconds > 600 {
			return nil, fmt.Errorf("target %q: mail_wait_seconds must be 0..600", name)
		}
		if t.Mail && !strings.Contains(t.Email+t.User, "@") {
			return nil, fmt.Errorf("target %q: mail needs the e-mail address as login (\"user\" or \"email\" with an @)", name)
		}
		if t.PasswordFile == "" && t.Password == "" && t.PasswordEnv == "" {
			return nil, fmt.Errorf("target %q: needs \"password_file\", \"password\" or \"password_env\"", name)
		}
		if _, err := probe.ParseHeaders(t.headerLines()); err != nil {
			return nil, fmt.Errorf("target %q: %v", name, err)
		}
	}
	if err := c.validateMailMonitor(); err != nil {
		return nil, err
	}
	return &c, nil
}

// AuthBackoff returns the pause after a failed login.
func (c *Config) AuthBackoff() time.Duration {
	return time.Duration(c.AuthBackoffSeconds) * time.Second
}

// MinInterval returns the probe cache window.
func (c *Config) MinInterval() time.Duration {
	return time.Duration(c.MinIntervalSeconds) * time.Second
}

// readFirstLine returns the first line of a secret file.
func readFirstLine(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	return strings.TrimRight(line, "\r"), nil
}

// password resolves the password of a target at probe time, so rotating the
// file or variable takes effect without a restart.
// headerLines returns the target headers as "Name: value" lines.
func (t Target) headerLines() []string {
	var out []string
	for k, v := range t.Headers {
		out = append(out, k+": "+v)
	}
	return out
}

func (t Target) password() (string, error) {
	if t.PasswordFile != "" {
		p, err := readFirstLine(t.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("password file: %w", err)
		}
		if p == "" {
			return "", errors.New("password file is empty")
		}
		return p, nil
	}
	if t.Password != "" {
		return t.Password, nil
	}
	p := os.Getenv(t.PasswordEnv)
	if p == "" {
		return "", fmt.Errorf("environment variable %s is empty", t.PasswordEnv)
	}
	return p, nil
}

// authToken resolves the optional /probe token: from a file, else the token itself, else a variable whose name is given.
func (c *Config) authToken() (string, error) {
	switch {
	case c.AuthTokenFile != "":
		return readFirstLine(c.AuthTokenFile)
	case c.AuthToken != "":
		return c.AuthToken, nil
	case c.AuthTokenEnv != "":
		return os.Getenv(c.AuthTokenEnv), nil
	}
	return "", nil
}

// mailWait is how long a mail probe waits for the mail to come back.
func (t Target) mailWait() time.Duration {
	if t.MailWaitSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(t.MailWaitSeconds) * time.Second
}

// MailMonitorTarget is one address outside that the mail monitor sends to.
//
// The account that sends the mail and receives the answer is, in this order: the one given inline (the fields of
// MailMonitorAccount), the target that "account" names, or the only target when there is exactly one. An entry with its
// own account is not a target of /probe, so nothing can scrape it by accident.
type MailMonitorTarget struct {
	// To is the internet address the mail is sent to.
	To string `json:"to"`
	// Account is the name of a target (in "targets") whose ActiveSync account is used. Not together with account fields.
	Account string `json:"account"`
	MailMonitorAccount
	// IntervalSeconds is the time between two mails (default 600, at least 60).
	IntervalSeconds int `json:"interval_seconds"`
	// TimeoutSeconds is how long an answer is waited for before the mail counts as lost (default 300).
	TimeoutSeconds int `json:"timeout_seconds"`
}

// MailMonitorAccount is an account of its own for a mail monitor target, with the fields of a target that matter for it.
// The login has to be an e-mail address, and the server has to be given (autodiscover is not used).
type MailMonitorAccount struct {
	Server       string            `json:"server"`
	Email        string            `json:"email"`
	User         string            `json:"user"`
	Profile      string            `json:"profile"`
	Provision    string            `json:"provision"`
	Password     string            `json:"password"`
	PasswordFile string            `json:"password_file"`
	PasswordEnv  string            `json:"password_env"`
	Insecure     *bool             `json:"insecure"`
	CAFile       string            `json:"ca_file"`
	Headers      map[string]string `json:"headers"`
}

func (a MailMonitorAccount) isSet() bool { return !reflect.DeepEqual(a, MailMonitorAccount{}) }

// target is the account as a Target.
func (a MailMonitorAccount) target() Target {
	return Target{Server: a.Server, Email: a.Email, User: a.User, Profile: a.Profile, Provision: a.Provision,
		Password: a.Password, PasswordFile: a.PasswordFile, PasswordEnv: a.PasswordEnv,
		Insecure: a.Insecure, CAFile: a.CAFile, Headers: a.Headers}
}

// monitorAccount returns the account of a mail monitor entry: a key that is the same for entries that share an account
// (the name of the target, or "monitor:NAME" for an own account, which no target name can have) and the account itself.
func (c *Config) monitorAccount(name string, m MailMonitorTarget) (string, Target) {
	if m.Account != "" {
		return m.Account, c.Targets[m.Account]
	}
	return "monitor:" + name, m.MailMonitorAccount.target()
}

const (
	mailMonitorDefaultInterval = 600
	mailMonitorMinInterval     = 60
	mailMonitorDefaultTimeout  = 300
)

// validateMailMonitor checks the mail monitor targets and fills in the defaults.
func (c *Config) validateMailMonitor() error {
	if c.MailMonitorPollSeconds < 0 || c.MailMonitorPollSeconds > 60 {
		return errors.New("mail_monitor_poll_seconds must be 1..60")
	}
	if c.MailMonitorPollSeconds == 0 {
		c.MailMonitorPollSeconds = 2
	}
	for name, m := range c.MailMonitor {
		if !targetName.MatchString(name) {
			return fmt.Errorf("mail_monitor %q: name may only contain letters, digits, '.', '_', '-'", name)
		}
		if i := strings.Index(m.To, "@"); i < 1 || i == len(m.To)-1 || strings.ContainsAny(m.To, " ,;<>\"") {
			return fmt.Errorf("mail_monitor %q: \"to\" must be one internet address", name)
		}

		own := m.MailMonitorAccount.isSet()
		var t Target
		switch {
		case own && m.Account != "":
			return fmt.Errorf("mail_monitor %q: give either \"account\" (a target) or the fields of an account of its own, not both", name)
		case own:
			t = m.MailMonitorAccount.target()
			if t.PasswordFile == "" && t.Password == "" && t.PasswordEnv == "" {
				return fmt.Errorf("mail_monitor %q: the account needs \"password_file\", \"password\" or \"password_env\"", name)
			}
			if _, err := probe.ParseHeaders(t.headerLines()); err != nil {
				return fmt.Errorf("mail_monitor %q: %v", name, err)
			}
		default:
			if m.Account == "" && len(c.Targets) == 1 {
				for only := range c.Targets {
					m.Account = only
				}
			}
			var ok bool
			if t, ok = c.Targets[m.Account]; !ok {
				return fmt.Errorf("mail_monitor %q: \"account\" must name one of the targets (or the entry needs an account of its own)", name)
			}
		}
		if t.Server == "" {
			return fmt.Errorf("mail_monitor %q: the account needs \"server\" (autodiscover is not used here)", name)
		}
		if !strings.Contains(t.Email+t.User, "@") {
			return fmt.Errorf("mail_monitor %q: the account needs the e-mail address as login (\"user\" or \"email\" with an @)", name)
		}
		if m.IntervalSeconds == 0 {
			m.IntervalSeconds = mailMonitorDefaultInterval
		}
		if m.IntervalSeconds < mailMonitorMinInterval || m.IntervalSeconds > 86400 {
			return fmt.Errorf("mail_monitor %q: interval_seconds must be %d..86400", name, mailMonitorMinInterval)
		}
		if m.TimeoutSeconds == 0 {
			m.TimeoutSeconds = mailMonitorDefaultTimeout
		}
		if m.TimeoutSeconds < 10 || m.TimeoutSeconds > 3600 {
			return errors.New("mail_monitor " + strconv.Quote(name) + ": timeout_seconds must be 10..3600")
		}
		c.MailMonitor[name] = m
	}
	return nil
}

// CheckListen is the check of the start (not of the loading of a configuration): without a certificate the listener serves everything
// over plain HTTP, and that has to be chosen (allow_http) unless the address is loopback.
func (c *Config) CheckListen() error {
	return listenaddr.NeedsHTTPSwitch(c.Listen, c.TLSCert != "", c.AllowHTTP)
}

// The well-known ports of the exporter: HTTPS, plain HTTP, and the metrics port (the port of the node exporter's convention for a
// metrics service of its own; mapped to another port at the container, if it has to be).
const (
	DefaultListenTLS     = ":9117"
	DefaultListenPlain   = "127.0.0.1:9116"
	DefaultListenMetrics = ":9100"
)

// MakesCertificate says whether the start makes the certificate when it is not there, and renews one that it made: when the certificate was
// not named (the default place, TLSAuto), or tls_generate is set.
func (c *Config) MakesCertificate() bool { return c.TLSCert != "" && (c.TLSAuto || c.TLSGenerate) }
