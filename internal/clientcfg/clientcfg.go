// Package clientcfg is the configuration that the programs which work on the mailboxes have in common (eas-client and eas-mcp): the
// options, the config file with its list of accounts, the environment (EASCLIENT_...), the log, and the manager of the accounts that is made
// from them. A program defines the options with Define, adds its own, and lets Resolve read the file and the environment.
package clientcfg

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/internal/settings"
	"github.com/nashcom/eas-tools/probe"
)

// Prefix of the environment variables: EASCLIENT_SERVER for -server.
const Prefix = "EASCLIENT_"

// StringList is an option that may be given several times and holds comma separated values.
type StringList []string

func (l *StringList) String() string { return strings.Join(*l, ",") }

func (l *StringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// Flags are the options in common. The pointers are filled when the flag set is parsed.
type Flags struct {
	Account, Server, Email, User, PwEnv, PwFile, DeviceID, Profile, Provision, CAFile, StatePath, Timezone, TraceFile, LogFormat *string
	PwStdin, Insecure, NoState, Write, Send, Verbose, Debug, ShowVersion, HealthCheck                                            *bool
	Timeout                                                                                                                      *time.Duration
	// SendDomains limits the domains that mail is sent to (-send-domains, send_domains in the file).
	SendDomains StringList

	fileAccounts []client.Account
}

// Define adds the options in common to the flag set.
func Define(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	_ = fs.String("config", "", "JSON config file: the options (keys are the option names, \"-\" written as \"_\") and \"accounts\", a list of accounts with a \"name\"")
	f.Account = fs.String("account", "", "the account to use (default: the only one)")
	f.Server = fs.String("server", "", "EAS endpoint (host or URL) of a single account given on the command line")
	f.Email = fs.String("email", "", "e-mail address of that account")
	f.User = fs.String("user", "", "login name if different from -email")
	f.PwEnv = fs.String("password-env", "EASCLIENT_PASSWORD", "name of the environment variable holding the password")
	f.PwFile = fs.String("password-file", "", "read the password from the first line of this file")
	f.PwStdin = fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	f.DeviceID = fs.String("device-id", "", "device id (default: derived from user and server, for this tool alone)")
	f.Profile = fs.String("profile", "", "how to present the device (see eas-probe -h)")
	f.Provision = fs.String("provision", "auto", "auto | always | never")
	f.Insecure = fs.Bool("insecure", false, "do not verify the server TLS certificate")
	f.CAFile = fs.String("ca-file", "", "PEM file with additional trusted CA certificates")
	f.StatePath = fs.String("state-file", "", "state file (default: client-state.json in the easprobe directory of the user config dir)")
	f.NoState = fs.Bool("no-state", false, "keep no state on disk")
	f.Write = fs.Bool("write", false, "allow read, unread, move and delete (reading is always allowed)")
	f.Send = fs.Bool("send", false, "allow send: sending a mail is a switch of its own, -write does not include it")
	fs.Var(&f.SendDomains, "send-domains", "send, reply and forward only to these domains: example.com is that domain, *.example.com every domain below it; repeatable or comma separated; default: no restriction")
	f.Timeout = fs.Duration("timeout", time.Minute, "time limit of one command or one request")
	f.Timezone = fs.String("timezone", "", "time zone of times without an offset and of new calendar entries, a name such as Europe/Berlin (default: the zone of the machine: TZ, /etc/localtime, Windows)")
	f.TraceFile = fs.String("trace", "", "append every request and response, WBXML decoded, to this file (no password or cookies)")
	f.Verbose = fs.Bool("verbose", false, "log at level debug")
	f.Debug = fs.Bool("debug", false, "log at level trace: the details and the library output")
	f.LogFormat = fs.String("log-format", "", "format of the log (on stderr): text (the default) or json")
	f.ShowVersion = fs.Bool("version", false, "print version and exit")
	f.HealthCheck = fs.Bool("health-check", false, "ask the running program on this machine for /health and exit with 0 (healthy) or 1: for the health check of a container, with the same options as the program that runs (-listen, -allow-http, -tls-cert, -listen-health)")
	return f
}

// Resolve reads the config file and the environment into the options that were not given on the command line (the order is: command line,
// environment, file, default). The accounts of the file are kept for Accounts. A program that has options of its own that have to be
// known to the file lists them in extra.
func (f *Flags) Resolve(fs *flag.FlagSet, environ func() []string, readFile func(string) ([]byte, error), extra ...string) (settings.Result, error) {
	return settings.Resolve(fs, settings.Options{
		Prefix: Prefix, ConfigFlag: "config", Skip: []string{"version", "password-stdin", "health-check"},
		Extra: append([]string{"password", "log_level"}, extra...), Environ: environ, ReadFile: f.accountsReader(readFile),
	})
}

// accountsReader reads the config file and takes the list "accounts" out of it: the option settings do not take a list. The rest of the
// file goes on.
func (f *Flags) accountsReader(readFile func(string) ([]byte, error)) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		raw, err := readFile(p)
		if err != nil {
			return nil, err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		if a, ok := doc["accounts"]; ok {
			dec := json.NewDecoder(bytes.NewReader(a))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&f.fileAccounts); err != nil {
				return nil, fmt.Errorf("accounts: %w", err)
			}
			delete(doc, "accounts")
		}
		return json.Marshal(doc)
	}
}

// Logger makes the logger on w. def is the level when nothing says another: a command that prints JSON logs only what is wrong, a server
// logs what it does.
func (f *Flags) Logger(res settings.Result, def slog.Level, w io.Writer, program, version string) (*slog.Logger, error) {
	level, err := logging.Pick(res.Values["log_level"], *f.Verbose, *f.Debug, def)
	if err != nil {
		return nil, err
	}
	state := logging.NewState(level)
	if err := state.SetFormat(*f.LogFormat); err != nil {
		return nil, err
	}
	return logging.New(w, state, program, version), nil
}

// Accounts are the accounts of the config file, or the single account that -server, -email and -user describe. optional says that a
// program can run without one (the command "accounts" and "serve" of eas-client).
func (f *Flags) Accounts(res settings.Result, stdin io.Reader, optional bool) ([]client.Account, error) {
	accounts := f.fileAccounts
	single := *f.Server != "" || *f.Email != "" || *f.User != ""
	switch {
	case single && len(accounts) > 0:
		return nil, fmt.Errorf("accounts are given in the config file and as -server/-email/-user: use one of the two")
	case single:
		a := client.Account{Name: "default", Server: *f.Server, Email: *f.Email, User: *f.User, PasswordFile: *f.PwFile, PasswordEnv: *f.PwEnv,
			Password: res.Values["password"], DeviceID: strings.ToUpper(*f.DeviceID), Profile: *f.Profile, Provision: *f.Provision}
		if *f.Insecure {
			a.Insecure = f.Insecure
		}
		a.CAFile = *f.CAFile
		if *f.PwStdin {
			line, err := FirstLine(stdin)
			if err != nil || line == "" {
				return nil, fmt.Errorf("-password-stdin: no password on stdin")
			}
			a.Password, a.PasswordFile = line, ""
		}
		return []client.Account{a}, nil
	case optional || len(accounts) > 0:
		// an account of the file that names no password takes the variable of -password-env (EASCLIENT_PASSWORD), as the single account does
		out := make([]client.Account, len(accounts))
		for i, a := range accounts {
			if a.Password == "" && a.PasswordFile == "" && a.PasswordEnv == "" {
				a.PasswordEnv = *f.PwEnv
			}
			out[i] = a
		}
		return out, nil
	}
	return nil, fmt.Errorf("no account: give -server and -email (or -user), or accounts in the config file (-config)")
}

// FirstLine is the first line of what r gives, without the line end: a password or a token.
func FirstLine(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimRight(line, "\r"), nil
}

// Open opens the state of the device and the trace, and makes the manager of the accounts. The function that it returns saves the state and
// closes everything; it is called once, when the program ends.
func (f *Flags) Open(accounts []client.Account, logger *slog.Logger) (m *client.Manager, store *probe.Store, closeAll func(), err error) {
	sp := f.StateFilePath()
	store, err = probe.OpenStore(sp)
	if err != nil {
		return nil, nil, nil, err
	}
	if msg := store.Recovered(); msg != "" {
		logger.Warn(msg)
	}
	var tracer *probe.Tracer
	if *f.TraceFile != "" {
		if tracer, err = probe.OpenTracer(*f.TraceFile); err != nil {
			return nil, nil, nil, err
		}
	}
	m, err = client.NewManager(accounts, client.Options{Trace: tracer, AllowWrite: *f.Write, AllowSend: *f.Send, SendDomains: f.SendDomains, Store: store,
		Logger: logger, Insecure: *f.Insecure, CAFile: *f.CAFile, TimeZone: *f.Timezone})
	if err != nil {
		if tracer != nil {
			tracer.Close()
		}
		return nil, nil, nil, err
	}
	closeAll = func() {
		if err := store.Flush(); err != nil {
			logger.Warn("the state could not be saved", "error", err.Error())
		}
		m.Close()
		if tracer != nil {
			tracer.Close()
		}
	}
	return m, store, closeAll, nil
}

// StateFilePath is the file of the device state: -state-file, or client-state.json in the easprobe folder of the user config folder; empty with
// -no-state.
func (f *Flags) StateFilePath() string {
	if *f.NoState {
		return ""
	}
	if *f.StatePath != "" {
		return *f.StatePath
	}
	return filepath.Join(filepath.Dir(probe.DefaultStatePath()), "client-state.json")
}
