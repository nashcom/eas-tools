// eas-load is an nshtestherd worker that generates ActiveSync load: it
// registers N simulated devices with the coordinator, gets their accounts,
// and runs the jobs the coordinator asks for (eas-probe, eas-sync).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nashcom/eas-tools/herd"
	"github.com/nashcom/eas-tools/internal/logging"
	"github.com/nashcom/eas-tools/internal/settings"
	"github.com/nashcom/eas-tools/load"
	"github.com/nashcom/eas-tools/probe"
)

func main() {
	os.Exit(run())
}

func run() int {
	host, _ := os.Hostname()
	if host == "" {
		host = "eas-load"
	}

	var (
		herdURL     = flag.String("herd", env("NSHTEST_COORDINATOR", "http://127.0.0.1:8788"), "nshtestherd coordinator URL")
		clients     = flag.Int("clients", 1, "number of simulated devices in this process")
		name        = flag.String("name", host, "name of this worker; request keys are <name>-<n>. Keep it stable across restarts and different for workers running in parallel")
		server      = flag.String("server", env("EASLOAD_SERVER", ""), "ActiveSync server: host name or URL")
		login       = flag.String("login", "internetaddress", "which account name to log in with: internetaddress | shortname")
		devices     = flag.Int("devices", 1, "devices per account: each client runs this many devices of its user at the same time (phone, tablet, ...)")
		profile     = flag.String("profile", probe.DefaultProfile, "device profile(s), comma separated, used in turn for the devices: "+strings.Join(probe.ProfileNames(), " | "))
		provision   = flag.String("provision", probe.ProvisionAuto, "auto | always | never")
		insecure    = flag.Bool("insecure", false, "do not verify the server TLS certificate")
		caFile      = flag.String("ca-file", "", "PEM file with additional trusted CA certificates")
		statePath   = flag.String("state-file", "", "state file (default: <user config dir>/easprobe/load-<name>.json)")
		noState     = flag.Bool("no-state", false, "keep no state on disk; devices register again on every start")
		poll        = flag.Duration("poll", 2*time.Second, "status poll interval per client")
		ramp        = flag.Duration("ramp", 100*time.Millisecond, "delay between client starts")
		retry       = flag.Duration("retry", 5*time.Second, "wait when the coordinator has no account or is not reachable")
		stepTimeout = flag.Duration("step-timeout", 30*time.Second, "timeout of one ActiveSync request step")
		metrics     = flag.String("metrics", "", "listen address for /metrics and /health, e.g. :9117 (default: off)")
		report      = flag.Duration("report", 10*time.Second, "interval of the summary line (0 = off)")
		user        = flag.String("user", env("EASLOAD_USER", ""), "standalone mode: login of the one account (use -devices for several devices of it)")
		pwFile      = flag.String("password-file", "", "standalone mode: file with the password of -user in its first line")
		pwEnv       = flag.String("password-env", "EASLOAD_PASSWORD", "standalone mode: environment variable with the password of -user")
		usersFile   = flag.String("users", "", "standalone mode: CSV file with the accounts (nshtestherd format: FirstName,LastName,Password,Shortname,InternetAddress)")
		jobName     = flag.String("job", "", "standalone mode: the job to run: eas-probe | eas-sync | eas-gal | eas-mail")
		jobParams   = flag.String("params", "", "standalone mode: parameters of the job, as in a coordinator command (think=10-30&...)")
		duration    = flag.Duration("duration", 0, "standalone mode: stop the job after this time (0 = until it ends)")
		resultFile  = flag.String("result-file", "", "write the result of the run as a JSON file when it ends (numbers per command, job outcome)")
		headerLines probe.HeaderFlags
		verbose     = flag.Bool("verbose", false, "log at level debug: also the routine activity (the default is info; log_level sets the level)")
		debug       = flag.Bool("debug", false, "log at level trace: the details and the library output")
		logFormat   = flag.String("log-format", "", "format of the log: text (the default) or json, one OpenTelemetry-style object per line")
		showVersion = flag.Bool("version", false, "print version and exit")
		_           = flag.String("config", "", "JSON config file with the options (keys are the option names, \"-\" written as \"_\"); the command line and EASLOAD_<OPTION> variables win over it")
	)
	flag.Usage = usage
	flag.Var(&headerLines, "header", "extra request header \"Name: value\" for every request (repeatable), for example X-Mock-Sim for the mock server")
	flag.Parse()

	// Options not given on the command line come from EASLOAD_<OPTION> variables, then from the config file.
	settingsResult, err := settings.Resolve(flag.CommandLine, settings.Options{
		Prefix: "EASLOAD_", ConfigFlag: "config", Skip: []string{"version"}, Extra: []string{"password", "log_level"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		return 2
	}
	for _, w := range settingsResult.Warnings {
		fmt.Fprintln(os.Stderr, "eas-load: warning:", w)
	}

	if *showVersion {
		fmt.Println("eas-load", probe.Version)
		return 0
	}
	if *server == "" {
		fmt.Fprintln(os.Stderr, "eas-load: -server is required")
		return 2
	}
	headers, err := probe.ParseHeaders(headerLines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		return 2
	}

	// Standalone mode: the accounts and the job come from the command line and
	// the worker runs without a coordinator. Otherwise it follows nshtestherd.
	var coordinator load.Coordinator
	var local *load.Local
	nClients := *clients
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if *user != "" || *usersFile != "" {
		accounts, code := standaloneAccounts(*user, *pwFile, settingsResult.Values["password"], *pwEnv, *usersFile)
		if code != 0 {
			return code
		}
		switch {
		case *jobName == "":
			fmt.Fprintln(os.Stderr, "eas-load: -job is required with -user or -users")
			return 2
		case setFlags["herd"]:
			fmt.Fprintln(os.Stderr, "eas-load: -herd and -user/-users exclude each other: coordinator mode or standalone mode")
			return 2
		case *user != "" && *usersFile != "":
			fmt.Fprintln(os.Stderr, "eas-load: use either -user or -users")
			return 2
		case *user != "" && setFlags["clients"] && *clients != 1:
			fmt.Fprintln(os.Stderr, "eas-load: one user is one client; use -devices for several devices of it")
			return 2
		case *usersFile != "" && setFlags["clients"] && *clients > len(accounts):
			fmt.Fprintf(os.Stderr, "eas-load: -clients %d, but %s has only %d accounts\n", *clients, *usersFile, len(accounts))
			return 2
		}
		nClients = len(accounts)
		if *usersFile != "" && setFlags["clients"] {
			nClients = *clients
		}
		local = load.NewLocal(accounts, nClients, *jobName, *jobParams, *duration)
		coordinator = local
	} else {
		if *jobName != "" || *jobParams != "" || *duration != 0 {
			fmt.Fprintln(os.Stderr, "eas-load: -job, -params and -duration are for standalone mode (-user or -users)")
			return 2
		}
		coordinator = herd.New(*herdURL, os.Getenv("NSHTEST_TOKEN"))
	}

	// info is the default; the progress lines and the result of eas-load are info, so warn leaves it silent.
	level, err := logging.Pick(settingsResult.Values["log_level"], *verbose, *debug, slog.LevelInfo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		return 2
	}
	logState := logging.NewState(level)
	if err := logState.SetFormat(*logFormat); err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		return 2
	}
	log := logging.New(os.Stderr, logState, "eas-load", probe.Version)
	// The library's own output is only wanted at the debug level.
	sessionState := logging.NewState(slog.LevelWarn)
	_ = sessionState.SetFormat(*logFormat)
	sessionLog := logging.New(os.Stderr, sessionState, "eas-load", probe.Version)
	if level <= logging.LevelTrace {
		sessionLog = log
	}

	path := *statePath
	if path == "" && !*noState {
		dir, err := os.UserConfigDir()
		if err != nil || dir == "" {
			dir = "."
		}
		path = filepath.Join(dir, "easprobe", "load-"+safeName(*name)+".json")
	}
	if *noState {
		path = ""
	}
	store, err := probe.OpenStore(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		return 2
	}
	if w := store.Recovered(); w != "" {
		log.Warn(w)
	}
	stopSave := store.AutoSave(30*time.Second, func(err error) { log.Error("saving state", "error", err) })

	stats := load.NewStats()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metrics != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("OK\n")) })
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			stats.WritePrometheus(w)
		})
		srv := &http.Server{Addr: *metrics, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics listener", "error", err)
			}
		}()
		defer srv.Close()
	}

	if *report > 0 {
		go func() {
			t := time.NewTicker(*report)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					log.Info("status\n" + stats.Summary())
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	log.Info("started", "version", probe.Version, "coordinator", coordinatorName(*herdURL, local != nil), "server", *server,
		"clients", nClients, "devices", *devices, "name", *name, "profile", *profile, "state", path)

	runStart := time.Now()
	err = load.Run(ctx, load.Config{
		Herd:        coordinator,
		Name:        *name,
		Clients:     nClients,
		Server:      *server,
		Login:       *login,
		Devices:     *devices,
		Profiles:    splitList(*profile),
		Provision:   *provision,
		Insecure:    *insecure,
		CAFile:      *caFile,
		Headers:     headers,
		Store:       store,
		Poll:        *poll,
		Ramp:        *ramp,
		RetryDelay:  *retry,
		StepTimeout: *stepTimeout,
		Stats:       stats,
		Log:         log,
		SessionLog:  sessionLog,
	})

	code := 0
	if err != nil {
		fmt.Fprintln(os.Stderr, "eas-load:", err)
		code = 2
	}

	if err := stopSave(); err != nil {
		log.Error("saving state", "error", err)
		code = 1
	}
	log.Info("finished\n" + stats.Summary())
	if n := stats.LoginRefusals(); n > 0 {
		log.Warn("logins were refused during this run: check the passwords, account lockouts and block lists (nginx, CrowdSec, Domino) before running again", "refused_steps", n)
	}

	if local != nil {
		res := local.Result()
		log.Info("standalone result", "job", *jobName, "ok", res.OK, "stopped", res.Stopped, "failed", res.Failed, "no_result", res.Missing)
		if res.Failed > 0 || (res.Missing > 0 && ctx.Err() == nil) {
			code = 1
		}
	}

	if *resultFile != "" {
		r := load.Result{Tool: "eas-load", Version: probe.Version, Name: *name, Server: *server, Mode: "coordinator",
			Job: *jobName, Params: *jobParams, Profile: *profile, Clients: nClients, Devices: *devices}
		if local != nil {
			r.Mode = "standalone"
			res := local.Result()
			r.Jobs = load.JobsResult{OK: res.OK, Stopped: res.Stopped, Failed: res.Failed}
			if ctx.Err() == nil {
				r.Jobs.NoResult = res.Missing
			}
		}
		stats.Snapshot(&r)
		r.SetOutcome()
		r.SetTimes(runStart, time.Now())
		if err := r.Write(*resultFile); err != nil {
			log.Error("writing the result file", "file", *resultFile, "error", err)
			code = 1
		} else {
			log.Info("result written", "file", *resultFile, "outcome", r.Outcome)
		}
	}
	return code
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

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintf(os.Stderr, `eas-load %s - nshtestherd worker for ActiveSync load

Usage:
  export NSHTEST_TOKEN=...            (only when the coordinator has a token)
  eas-load -herd http://coordinator:8788 -server traveler.example.com -clients 50

Each account runs -devices devices at the same time (phone, tablet, ...), profiles from -profile in turn.
The coordinator (nshtestherd) hands out the accounts; control the load with
its commands, for example:
  curl -X POST -d 'target=all&command=run&job=eas-sync' --data-urlencode 'params=think=10-30&ping=60' http://coordinator:8788/command
  curl -X POST -d 'target=all&command=run&job=eas-probe' http://coordinator:8788/command
  curl -X POST -d 'target=all&command=stop' http://coordinator:8788/command

Standalone, without a coordinator: the accounts and the job come from the command line.
  one user (with -devices for several devices of it), the password from a file or EASLOAD_PASSWORD:
  eas-load -server traveler.example.com -user alice@example.com -password-file pw.txt -devices 2 \
           -job eas-mail -params 'to=self&think=30-120&count=10'
  many users from a CSV file (FirstName,LastName,Password,Shortname,InternetAddress, as nshtestherd reads it),
  the job stops after 10 minutes:
  eas-load -server traveler.example.com -users users.csv -job eas-sync -params 'think=10-30' -duration 10m
The exit code is 0 when the job ended ok (or was stopped by -duration), 1 when it failed, 2 for wrong options.
-result-file FILE writes the numbers and the outcome of the run as JSON when it ends; it holds no passwords.

Jobs:
  eas-probe   one full flow per device: options, provision, folders, sync
  eas-sync    keep syncing until told otherwise; params think=MIN-MAX (seconds),
              ping=N (push: hold a Ping of N seconds, sync on change),
              duration=N (end after N seconds, result ok)
  eas-gal     directory (address book) lookups of the own user, over and over;
              params mode=self|typeahead (typeahead = the growing prefixes, like typing),
              query=TEXT (fixed search text), think=MIN-MAX, duration=N
  eas-mail    send mail through ActiveSync, over and over; params to=self|others|a@x.de,b@y.de,
              think=MIN-MAX, size=MIN-MAX (KB, text), attach=MIN-MAX (KB of each attachment, default none),
              files=N (attachments per mail, default 1), attach_type=txt|bin (default txt),
              save_sent=yes|no (keep a copy in Sent Items, default no; otherwise the copies are deleted at the end),
              wait=N (seconds to wait for the mail to come back),
              arrival=push|poll, cleanup=yes|no, count=N, duration=N

Settings from the environment and a file:
  Every option below can also be set as an environment variable EASLOAD_<OPTION> (upper case, "-" written as "_":
  EASLOAD_SERVER, EASLOAD_PASSWORD_FILE, EASLOAD_DEVICES, EASLOAD_PARAMS, ...) and as a key of the JSON file given with
  -config or EASLOAD_CONFIG (keys written with "_": password_file; "header" takes a list). The command line wins over the
  environment, the environment over the file, the file over the default. The password: the key "password" (or EASLOAD_PASSWORD),
  or password_file for a separate file (never a command line option: others on the machine can read it).

Options:
`, probe.Version)
	flag.PrintDefaults()
}

func coordinatorName(url string, standalone bool) string {
	if standalone {
		return "none (standalone)"
	}
	return url
}

// standaloneAccounts returns the accounts of a standalone run: the one given
// user (the password from a file or the environment, never from an option) or
// the accounts of a CSV file.
func standaloneAccounts(user, pwFile, plain, pwEnv, usersFile string) ([]herd.Reply, int) {
	if usersFile != "" {
		f, err := os.Open(usersFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eas-load:", err)
			return nil, 2
		}
		defer f.Close()
		accounts, err := load.ParseAccounts(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "eas-load: %s: %v\n", usersFile, err)
			return nil, 2
		}
		return accounts, 0
	}

	password := ""
	switch {
	case pwFile != "":
		raw, err := os.ReadFile(pwFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "eas-load:", err)
			return nil, 2
		}
		password = strings.TrimRight(strings.SplitN(string(raw), "\n", 2)[0], "\r")
	case plain != "":
		password = plain
	case os.Getenv(pwEnv) != "":
		password = os.Getenv(pwEnv)
	}
	if password == "" {
		fmt.Fprintf(os.Stderr, "eas-load: no password: use -password-file or the environment variable %s\n", pwEnv)
		return nil, 2
	}

	short := user
	if i := strings.Index(user, "@"); i > 0 {
		short = user[:i]
	}
	return []herd.Reply{{Shortname: short, InternetAddress: user, Password: password}}, 0
}
