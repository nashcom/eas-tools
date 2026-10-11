// eas-probe performs a read-only ActiveSync conversation with one account and
// reports the result as text, JSON or Prometheus metrics.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/internal/settings"
	"github.com/nashcom/eas-tools/probe"
)

const version = probe.Version

func main() {
	os.Exit(run())
}

func run() int {
	var (
		server      = flag.String("server", "", "EAS endpoint: host name or URL (default: autodiscover from -email)")
		email       = flag.String("email", "", "e-mail address (autodiscover input, default login)")
		user        = flag.String("user", "", "login name if different from -email")
		pwEnv       = flag.String("password-env", "EASPROBE_PASSWORD", "name of the environment variable holding the password")
		pwStdin     = flag.Bool("password-stdin", false, "read the password from the first line of stdin")
		pwFile      = flag.String("password-file", "", "read the password from the first line of this file")
		deviceID    = flag.String("device-id", "", "32 hex character device id (default: derived from user and server)")
		profile     = flag.String("profile", probe.DefaultProfile, "how to present the device, comma separated for several devices, used in turn: "+strings.Join(probe.ProfileNames(), " | "))
		workers     = flag.Int("workers", 1, "probe up to this many devices at the same time (1 = one after the other); the first device always runs alone as the login pilot")
		devices     = flag.Int("devices", 1, "probe this many devices of the account, one after the other (each is its own device in the server's list)")
		deviceType  = flag.String("device-type", "", "DeviceType sent to the server (default: from the profile)")
		userAgent   = flag.String("user-agent", "", "HTTP User-Agent (default: from the profile)")
		asVersion   = flag.String("as-version", "", "force an ActiveSync protocol version (default: detect)")
		statePath   = flag.String("state-file", "", "state file (default: <user config dir>/easprobe/state.json)")
		noState     = flag.Bool("no-state", false, "keep no state on disk; every run registers and syncs from scratch")
		fresh       = flag.Bool("fresh", false, "discard stored state before the run")
		provision   = flag.String("provision", probe.ProvisionAuto, "auto | always | never")
		directory   = flag.String("directory", "auto", "look for the own user in the directory (Search/GAL): auto (if the server offers it) | on | off")
		dirQuery    = flag.String("directory-query", "", "directory search text (default: the user name)")
		pingSeconds = flag.Int("ping", 0, "also run Ping with this heartbeat in seconds (0 = off)")
		mailTest    = flag.Bool("mail", false, "also send a mail to the account itself, wait for it to come back and delete it (creates a mail!)")
		mailWait    = flag.Int("mail-wait-seconds", 60, "seconds to wait for the mail to come back")
		mailArrival = flag.String("mail-arrival", "poll", "how the arrival is awaited: poll (Sync every second) or push (held Ping; experimental, unreliable with one device)")
		mailKeep    = flag.Bool("mail-keep", false, "keep the mail and a copy in Sent Items instead of deleting it")
		mailSize    = flag.Float64("mail-size", 2, "approximate mail size in KB")
		window      = flag.Int("window", 5, "number of mail items to request in the Inbox sync")
		insecure    = flag.Bool("insecure", false, "do not verify the server TLS certificate")
		caFile      = flag.String("ca-file", "", "PEM file with additional trusted CA certificates")
		stepTimeout = flag.Duration("step-timeout", 30*time.Second, "timeout of each step")
		timeout     = flag.Duration("timeout", 3*time.Minute, "timeout of the whole run")
		asJSON      = flag.Bool("json", false, "print the result as JSON")
		asProm      = flag.Bool("prom", false, "print the result as Prometheus text metrics")
		verbose     = flag.Bool("verbose", false, "log at level debug: also the routine activity (the default is info; log_level sets the level)")
		debug       = flag.Bool("debug", false, "log at level trace: the details and the library output")
		logFormat   = flag.String("log-format", "", "format of the log: text (the default) or json, one OpenTelemetry-style object per line")
		traceFile   = flag.String("trace", "", "append every request and response, WBXML decoded, to this file (no password or cookies)")
		headerLines probe.HeaderFlags
		showVersion = flag.Bool("version", false, "print version and exit")
		_           = flag.String("config", "", "JSON config file with the options (keys are the option names, \"-\" written as \"_\"); the command line and EASPROBE_<OPTION> variables win over it")
	)
	flag.Usage = usage
	flag.Var(&headerLines, "header", "extra request header \"Name: value\" for every request (repeatable), for example X-Mock-Sim for the mock server")
	flag.Parse()

	// Options not given on the command line come from EASPROBE_<OPTION> variables, then from the config file.
	settingsResult, err := settings.Resolve(flag.CommandLine, settings.Options{
		Prefix: "EASPROBE_", ConfigFlag: "config", Skip: []string{"version"}, Extra: []string{"password", "log_level"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}
	for _, w := range settingsResult.Warnings {
		fmt.Fprintln(os.Stderr, "eas-probe: warning:", w)
	}

	if *showVersion {
		fmt.Println("eas-probe", version)
		return 0
	}

	password, err := readPassword(*pwEnv, *pwStdin, *pwFile, settingsResult.Values["password"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}

	headers, err := probe.ParseHeaders(headerLines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}

	level, err := logging.Pick(settingsResult.Values["log_level"], *verbose, *debug, slog.LevelInfo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}
	logState := logging.NewState(level)
	if err := logState.SetFormat(*logFormat); err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}
	logger := logging.New(os.Stderr, logState, "eas-probe", version)

	cfg := probe.Config{
		ServerURL:      *server,
		Email:          *email,
		Username:       *user,
		Password:       password,
		DeviceID:       strings.ToUpper(*deviceID),
		Profile:        *profile,
		DeviceType:     *deviceType,
		UserAgent:      *userAgent,
		ASVersion:      *asVersion,
		Fresh:          *fresh,
		Provision:      *provision,
		Directory:      *directory,
		DirectoryQuery: *dirQuery,
		PingSeconds:    *pingSeconds,
		Mail:           *mailTest,
		MailWait:       time.Duration(*mailWait) * time.Second,
		MailArrival:    *mailArrival,
		MailKeep:       *mailKeep,
		MailSizeKB:     *mailSize,
		SyncWindow:     *window,
		Insecure:       *insecure,
		CACertFile:     *caFile,
		StepTimeout:    *stepTimeout,
		Headers:        headers,
		Logger:         logger,
	}
	if cfg.Username == "" {
		cfg.Username = cfg.Email
	}
	if *traceFile != "" {
		tr, err := probe.OpenTracer(*traceFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eas-probe:", err)
			return 2
		}
		defer tr.Close()
		cfg.Trace = tr
	}

	path := *statePath
	if path == "" && !*noState {
		path = probe.DefaultStatePath()
	}
	if *noState {
		path = ""
	}
	store, err := probe.OpenStore(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}
	if w := store.Recovered(); w != "" {
		fmt.Fprintln(os.Stderr, "eas-probe: warning:", w)
	}
	cfg.Store = store

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	profiles := splitList(*profile)
	if len(profiles) == 0 {
		profiles = []string{probe.DefaultProfile}
	}
	cfg.Profile = profiles[0]
	if *devices < 1 {
		fmt.Fprintln(os.Stderr, "eas-probe: -devices must be at least 1")
		return 2
	}
	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "eas-probe: -workers must be at least 1")
		return 2
	}
	if *devices > 1 && *deviceID != "" {
		fmt.Fprintln(os.Stderr, "eas-probe: -device-id names one device; use it with -devices 1")
		return 2
	}
	if *devices > 1 && *asProm {
		fmt.Fprintln(os.Stderr, "eas-probe: -prom reports one device; use -devices 1")
		return 2
	}

	results, err := probe.RunDevices(ctx, cfg, *devices, profiles, *workers)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe:", err)
		return 2
	}
	if err := store.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "eas-probe: saving state:", err)
	}

	allOK := len(results) == *devices
	for _, r := range results {
		if !r.OK {
			allOK = false
		}
	}

	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if *devices == 1 {
			_ = enc.Encode(results[0])
		} else {
			_ = enc.Encode(results)
		}
	case *asProm:
		probe.WritePrometheus(os.Stdout, results[0])
	default:
		for i, r := range results {
			if *devices > 1 {
				fmt.Printf("\n===== device %d of %d: %s =====\n", i+1, *devices, r.DeviceType)
			}
			probe.WriteText(os.Stdout, r)
		}
		if *devices > 1 {
			ok := 0
			for _, r := range results {
				if r.OK {
					ok++
				}
			}
			fmt.Printf("\n%d of %d devices ok", ok, *devices)
			if len(results) < *devices {
				fmt.Printf(" (stopped after %d: a refused login is not repeated)", len(results))
			}
			fmt.Println()
		}
	}

	if !allOK {
		return 1
	}
	return 0
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func readPassword(envName string, fromStdin bool, file, plain string) (string, error) {
	switch {
	case fromStdin:
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	case file != "":
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer f.Close()
		line, err := bufio.NewReader(f).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password file: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if plain != "" {
		return plain, nil
	}
	pw := os.Getenv(envName)
	if pw == "" {
		return "", fmt.Errorf("no password: set $%s, or use -password-stdin / -password-file", envName)
	}
	return pw, nil
}

func usage() {
	fmt.Fprintf(os.Stderr, `eas-probe %s - ActiveSync probe (read-only, except -mail)

Usage:
  EASPROBE_PASSWORD=... eas-probe -server mail.example.com -user alice
  EASPROBE_PASSWORD=... eas-probe -email alice@example.com        (autodiscover)

Steps: autodiscover (if no -server), options, provision, settings, foldersync, sync,
       directory (own user lookup, -directory), mail (round trip to self, -mail),
       ping (-ping N).
Several devices of the account: -devices 3 -profile iphone,ipad, one after the other,
or at the same time with -workers N (the first device always runs alone, as the login pilot).
Exit code: 0 all steps ok, 1 a step failed, 2 usage or configuration error.

Settings from the environment and a file:
  Every option below can also be set as an environment variable EASPROBE_<OPTION> (upper case, "-" written as "_":
  EASPROBE_SERVER, EASPROBE_PASSWORD_FILE, EASPROBE_MAIL, ...) and as a key of the JSON file given with -config or
  EASPROBE_CONFIG (keys written with "_": password_file; "header" takes a list). The command line wins over the
  environment, the environment over the file, the file over the default. The password: the key "password" (or EASPROBE_PASSWORD),
  or password_file for a separate file (never a command line option: others on the machine can read it).

Options:
`, version)
	flag.PrintDefaults()
}
