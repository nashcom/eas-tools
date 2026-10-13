// eas-client is an ActiveSync mail client for the command line and, with "serve", a REST API. Both print the same JSON.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/clientcfg"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/internal/listenaddr"
	"github.com/nashcom/eas-tools/probe"
)

const version = probe.Version

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ))
}

const usageText = `eas-client - ActiveSync mail client: command line and REST API, one JSON for both

Usage:
  eas-client [options] COMMAND [arguments]

Commands:
  accounts                         the accounts of the configuration
  info                             server, protocol version and device
  folders                          the folders of the mailbox
  list FOLDER [-limit N|all] [-preview BYTES] [-since 2w]
                                   the mails of a folder in a date window, newest first
  get [FOLDER] ID                  one mail with its whole body
  calendar list [FOLDER] [-from DATE] [-to DATE] [-since 2w] [-limit N|all]
                                   the events of the calendar, by start time; from now on by default
  calendar get [FOLDER] ID         one event with all details
  calendar ics [FOLDER] ID         the event as an iCalendar file (.ics): the raw text, not JSON
  calendar create [FOLDER] -subject S -start T [-end T | -duration D] [-all-day] [-location L] [-attendee A]...
                                   create an event (-write); with attendees it is a meeting: the invitation is mailed to them (-send as well)
  calendar delete [FOLDER] ID      delete an event (-write; a meeting with attendees needs -send too, and its cancellation is mailed)
  contacts list [FOLDER] [-search TEXT] [-limit N|all]
                                   the contacts of the address book, by name
  contacts get [FOLDER] ID         one contact with all details (JSON, no photo)
  contacts vcf [FOLDER] ID         one contact as a vCard file (.vcf): the raw text, with the photo
  contacts export [FOLDER] [-search TEXT]
                                   all contacts (or the search result) as one vCard file
  eml [FOLDER] ID                  the whole message as a mail file (.eml): the raw bytes, not JSON
  send -to ADDRESS [-cc ADDRESS] -subject TEXT TEXT
                                   send a mail; TEXT is -body TEXT, -body-file FILE or -body-stdin
  reply [FOLDER] ID [-all] TEXT   reply to a mail (SmartReply: the server adds the original); -all: to all
  forward [FOLDER] ID -to ADDRESS [-cc ADDRESS] TEXT
                                   forward a mail (SmartForward)
  read [FOLDER] ID | unread [FOLDER] ID     set the read flag
  move [FOLDER] ID DESTINATION     move a mail to another folder
  delete [FOLDER] ID               delete a mail (it goes to Deleted Items)
  serve                            the REST API on -listen (default :8443, HTTPS; 127.0.0.1:8080 with -allow-http), see docs/CLIENT.md

FOLDER is a folder id, or inbox, sent, drafts, deleted, or the name of the folder. It can be left out when the id of the
mail says it (4:10207 is in the folder 4).
read, unread, move and delete change the mailbox and need -write; send, reply and forward need -send (a switch of its own).
Options go before the command.
The answer is JSON on stdout; an error is a JSON object on stderr and the exit code 1.

Options:
`

// stringList is an option that may be given several times and holds comma separated values.
type stringList = clientcfg.StringList

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, environ func() []string) int {
	fs := flag.NewFlagSet("eas-client", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := clientcfg.Define(fs)
	account, timeout, showVer := cf.Account, cf.Timeout, cf.ShowVersion
	var (
		listen    = fs.String("listen", "", "serve: address of the REST API. Default: :8443 (HTTPS: TLS is the default), and 127.0.0.1:8080 with -allow-http (unencrypted is loopback unless an address is named)")
		tokenFile = fs.String("auth-token-file", "", "serve: file with the bearer token of the REST API (first line)")
		tlsCert   = fs.String("tls-cert", "", "serve: certificate file; with -tls-key the API is served over HTTPS")
		tlsKey    = fs.String("tls-key", "", "serve: private key file")
		tlsGen    = fs.Bool("tls-generate", false, "serve: make a small CA and a certificate for the listener when -tls-cert and -tls-key do not exist yet (they are never overwritten; the CA is kept next to the certificate as ca.crt and ca.key)")
		tlsDays   = fs.Int("tls-valid-days", certreload.DefaultLeafDays, "serve: how many days a certificate that the micro CA of the program makes is valid (1 to 3650); it is made again at every start, and a certificate of your own is not touched")
		tlsCheck  = fs.Duration("tls-check-interval", time.Minute, "serve: how often the certificate and key files are looked at; a changed pair is loaded without a restart (also on SIGHUP)")
		tlsFrom   = fs.String("tls-update-from", "", "serve: host:port of a certificate host (such as the Domino CertMgr) that is asked for a renewed certificate with the name as SNI: it is taken when it is for the key in use, valid and newer, and written to -tls-cert (the key is never touched). Asked at the start, every -tls-update-interval, and on SIGHUP")
		tlsName   = fs.String("tls-update-name", "", "serve: the name that the certificate host is asked for (SNI); default: the host name of this machine (never taken from a certificate: the certificate host answers with the best match, wildcards included)")
		tlsEvery  = fs.Duration("tls-update-interval", certreload.DefaultUpdateEvery, "serve: how often the certificate host is asked for a renewed certificate (it is asked at the start as well)")
		docs      = fs.Bool("docs", false, "serve: also the page /docs (Swagger UI) where the requests of the API can be tried, and /openapi.json is there always")
		docsUI    = fs.String("docs-ui", "", "serve: where the browser loads Swagger UI from for /docs: the URL of a directory with swagger-ui.css and swagger-ui-bundle.js (default: the CDN jsDelivr, in the version pinned in the program, with integrity hashes)")
	)
	allowHTTP := fs.Bool("allow-http", false, "serve: serve everything over plain HTTP although there is no certificate: for a proxy that terminates TLS. Without -tls-cert and without this the start is refused unless -listen is a loopback address")
	listenHealth := fs.String("listen-health", "", "serve: with -tls-cert a second listener, plain HTTP, with /health only (always on then), for the health check of a container; default: the loopback interface and the port after -listen. Another address needs -allow-from-health")
	var allowFrom, allowFromHealth, tlsNames stringList
	fs.Var(&allowFromHealth, "allow-from-health", "serve: the peers that may connect to the health listener -listen-health (same syntax as -allow-from); required when it is not on loopback; it applies to this listener alone")
	fs.Var(&tlsNames, "tls-names", "serve: the names (DNS names and IP addresses) of a certificate that -tls-generate makes; default: localhost, 127.0.0.1, ::1 and the host name")
	fs.Var(&allowFrom, "allow-from", "serve: only these peers may connect, every endpoint included: addresses, networks (192.168.0.0/16), ranges (10.0.0.5-10.0.0.20) and the name private; repeatable or comma separated; loopback is always allowed; default: no restriction")
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "eas-client: "+format+"\n", a...)
		return 2
	}
	res, err := cf.Resolve(fs, environ, os.ReadFile, "auth_token")
	if err != nil {
		return fail("%v", err)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "eas-client: warning:", w)
	}
	if *showVer {
		fmt.Fprintln(stdout, "eas-client", version)
		return 0
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	cmd, cmdArgs := rest[0], rest[1:]

	def := slog.LevelWarn // a command prints JSON: it logs only what is wrong, unless asked
	if cmd == "serve" {
		def = slog.LevelInfo
	}
	logger, err := cf.Logger(res, def, stderr, "eas-client", version)
	if err != nil {
		return fail("%v", err)
	}

	// the accounts and their state, shared with eas-mcp
	accounts, err := cf.Accounts(res, stdin, cmd == "accounts" || cmd == "serve")
	if err != nil {
		return fail("%v", err)
	}
	m, store, closeAll, err := cf.Open(accounts, logger)
	if err != nil {
		return fail("%v", err)
	}
	defer closeAll()

	if cmd == "serve" {
		token := res.Values["auth_token"]
		if *tokenFile != "" {
			t, err := bufioLine(strings.NewReader(readAll(*tokenFile)))
			if err != nil || t == "" {
				return fail("-auth-token-file: no token in %s", *tokenFile)
			}
			token = t
		}
		ui, err := client.ParseDocsUI(*docsUI)
		if err != nil {
			return fail("%v", err)
		}
		acl, err := ipacl.Parse(allowFrom)
		if err != nil {
			return fail("allow-from: %v", err)
		}
		aclHealth, err := ipacl.Parse(allowFromHealth)
		if err != nil {
			return fail("allow-from-health: %v", err)
		}
		if *tlsDays < 1 || *tlsDays > certreload.MaxLeafDays {
			return fail("-tls-valid-days: %d, use a number from 1 to %d (default %d)", *tlsDays, certreload.MaxLeafDays, certreload.DefaultLeafDays)
		}
		if *tlsCert != "" && *allowHTTP {
			return fail("-allow-http and -tls-cert exclude each other: a listener that has TLS speaks nothing else")
		}
		if *tlsFrom != "" {
			if host, port, err := net.SplitHostPort(*tlsFrom); err != nil || host == "" || port == "" {
				return fail("-tls-update-from: %q is not host:port", *tlsFrom)
			}
			if *allowHTTP {
				return fail("-tls-update-from renews the certificate of a listener that has TLS: it excludes -allow-http")
			}
		}
		return serve(m, store, logger, serveOptions{tlsUpdateFrom: *tlsFrom, tlsUpdateName: *tlsName, tlsUpdateEvery: *tlsEvery, addr: defaultListen(*listen, *allowHTTP), token: token, cert: *tlsCert, key: *tlsKey, timeout: *timeout, docs: *docs, docsUI: ui, allow: acl,
			tlsGenerate: *tlsGen, tlsNames: tlsNames, tlsCheck: *tlsCheck, tlsDays: *tlsDays, allowHTTP: *allowHTTP, healthAddr: *listenHealth, allowHealth: aclHealth}, stderr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	out, err := command(ctx, m, *account, cmd, cmdArgs, stdin, stderr)
	if err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintf(stderr, "eas-client: %v\n", u)
			return 2
		}
		_ = client.Encode(stderr, client.ErrorBody{Error: client.AsError(err)})
		return 1
	}
	if raw, ok := out.(*client.Raw); ok { // a mail file: the bytes, not JSON
		if _, err := stdout.Write(raw.Data); err != nil {
			return 1
		}
		return 0
	}
	_ = client.Encode(stdout, out)
	return 0
}

// usageError is a command that was not written right: no JSON, the exit code 2.
type usageError string

func (e usageError) Error() string { return string(e) }

func bufioLine(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimRight(line, "\r"), nil
}

func readAll(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// parseArgs parses the options of a command and returns its arguments, in any order: "list inbox -limit 5" and
// "list -limit 5 inbox" are the same.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError(err.Error())
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func need(pos []string, n int, what string) error {
	if len(pos) != n {
		return usageError(fmt.Sprintf("%s needs %d argument(s)", what, n))
	}
	return nil
}

// command runs one command and returns the value that is printed as JSON. The REST API calls the same operations.
func command(ctx context.Context, m *client.Manager, account, cmd string, args []string, stdin io.Reader, stderr io.Writer) (any, error) {
	newSet := func() *flag.FlagSet {
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		fs.SetOutput(stderr)
		return fs
	}
	switch cmd {
	case "accounts":
		return m.Accounts(), nil
	case "info":
		return m.Info(ctx, account)
	case "folders":
		return m.Folders(ctx, account)

	case "list":
		fs := newSet()
		limit := fs.String("limit", "20", "number of mails, or all")
		preview := fs.Int("preview", 0, "bytes of the body to show for each mail")
		since := fs.String("since", "", "date window: 1d, 3d, 1w, 2w (default), 1m, 3m or 6m; a list shows only the mail of that window")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		if err := need(pos, 1, "list"); err != nil {
			return nil, err
		}
		p := client.ListParams{Preview: *preview, Since: *since}
		if *limit == "all" {
			p.Limit = -1
		} else if _, err := fmt.Sscanf(*limit, "%d", &p.Limit); err != nil || p.Limit < 1 {
			return nil, usageError("-limit is a number of 1 or more, or all")
		}
		return m.List(ctx, account, pos[0], p)

	case "get":
		pos, err := parseArgs(newSet(), args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "get")
		if err != nil {
			return nil, err
		}
		return m.Get(ctx, account, folder, id)

	case "calendar":
		return calendarCommand(ctx, m, account, args, stdin, stderr)

	case "contacts":
		return contactsCommand(ctx, m, account, args, stderr)

	case "eml":
		pos, err := parseArgs(newSet(), args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "eml")
		if err != nil {
			return nil, err
		}
		return m.EML(ctx, account, folder, id)

	case "read", "unread":
		pos, err := parseArgs(newSet(), args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, cmd)
		if err != nil {
			return nil, err
		}
		return m.SetRead(ctx, account, folder, id, cmd == "read")

	case "move":
		pos, err := parseArgs(newSet(), args)
		if err != nil {
			return nil, err
		}
		folder, id, rest, err := folderAndID(pos, 1, "move")
		if err != nil {
			return nil, err
		}
		return m.Move(ctx, account, folder, id, rest[0])

	case "delete":
		pos, err := parseArgs(newSet(), args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "delete")
		if err != nil {
			return nil, err
		}
		return m.Delete(ctx, account, folder, id)

	case "send":
		fs := newSet()
		var to, cc stringList
		fs.Var(&to, "to", "recipient (repeatable, or comma separated)")
		fs.Var(&cc, "cc", "copy recipient (repeatable, or comma separated)")
		subject := fs.String("subject", "", "subject")
		tf := addTextFlags(fs)
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 0 {
			return nil, usageError("send takes options only")
		}
		text, err := tf.text(stdin)
		if err != nil {
			return nil, err
		}
		return m.Send(ctx, account, client.SendRequest{To: to, Cc: cc, Subject: *subject, Body: text, SaveInSent: tf.saveInSent()})

	case "reply":
		fs := newSet()
		all := fs.Bool("all", false, "reply to all: the other recipients of the mail too")
		tf := addTextFlags(fs)
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "reply")
		if err != nil {
			return nil, err
		}
		text, err := tf.text(stdin)
		if err != nil {
			return nil, err
		}
		return m.Reply(ctx, account, folder, id, client.ReplyRequest{Body: text, All: *all, SaveInSent: tf.saveInSent()})

	case "forward":
		fs := newSet()
		var to, cc stringList
		fs.Var(&to, "to", "recipient (repeatable, or comma separated)")
		fs.Var(&cc, "cc", "copy recipient (repeatable, or comma separated)")
		tf := addTextFlags(fs)
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "forward")
		if err != nil {
			return nil, err
		}
		text, err := tf.text(stdin)
		if err != nil {
			return nil, err
		}
		return m.Forward(ctx, account, folder, id, client.ForwardRequest{To: to, Cc: cc, Body: text, SaveInSent: tf.saveInSent()})
	}
	return nil, usageError(fmt.Sprintf("unknown command %q (accounts, info, folders, list, get, send, read, unread, move, delete, serve)", cmd))
}

type serveOptions struct {
	addr, token, cert, key string
	timeout                time.Duration
	docs                   bool
	docsUI                 string
	allow                  *ipacl.List
	tlsGenerate            bool
	tlsNames               []string
	tlsCheck               time.Duration
	tlsUpdateFrom          string // a certificate host that is asked for a renewed certificate
	tlsUpdateName          string
	tlsUpdateEvery         time.Duration
	tlsDays                int
	healthAddr             string      // with TLS: the listener of /health next to HTTPS; default loopback, the next port
	allowHTTP              bool        // everything over plain HTTP, chosen on purpose (a proxy that terminates TLS)
	allowHealth            *ipacl.List // the peers that may connect to it
}

func serve(m *client.Manager, store *probe.Store, log *slog.Logger, o serveOptions, stderr io.Writer) int {
	if (o.cert == "") != (o.key == "") {
		fmt.Fprintln(stderr, "eas-client: -tls-cert and -tls-key go together")
		return 2
	}
	// TLS is the default: without a certificate named, and without the choice of plain HTTP, and unless the listen address is loopback (which
	// never leaves the machine), the certificate is looked for in /run/secrets/tls, and made there when it is not
	auto := false
	if o.cert == "" && !o.allowHTTP && !listenaddr.Loopback(o.addr) {
		o.cert, o.key = certreload.DefaultFiles()
		auto = true
	}
	if o.cert == "" && o.tlsGenerate {
		fmt.Fprintln(stderr, "eas-client: -tls-generate makes the certificate of a listener that has TLS: it excludes -allow-http and a loopback -listen, where there is none")
		return 2
	}
	if o.cert == "" && (o.healthAddr != "" || len(o.allowHealth.Entries()) > 0) {
		fmt.Fprintln(stderr, "eas-client: -listen-health and -allow-from-health are for the health listener next to HTTPS: there is none without TLS (-listen is plain already)")
		return 2
	}
	// with TLS a listener for /health runs next to the HTTPS one
	healthAddr := ""
	if o.cert != "" {
		var err error
		if healthAddr, err = listenaddr.Second(o.addr, o.healthAddr, o.allowHealth.Entries(), "allow_from_health", false); err != nil {
			fmt.Fprintln(stderr, "eas-client: -listen-health:", err)
			return 2
		}
	}
	if !listenaddr.Loopback(o.addr) && o.token == "" { // the health listener has /health only, and needs none
		fmt.Fprintf(stderr, "eas-client: %s is not a loopback address: the API gives access to the mailboxes and needs a token (-auth-token-file, or EASCLIENT_AUTH_TOKEN)\n", o.addr)
		return 2
	}
	if err := listenaddr.NeedsHTTPSwitch(o.addr, o.cert != "", o.allowHTTP); err != nil {
		fmt.Fprintln(stderr, "eas-client:", err)
		return 2
	}
	if o.allowHTTP && !listenaddr.Loopback(o.addr) {
		log.Warn("the API is served over plain HTTP (-allow-http): that is for a proxy that terminates TLS, the token and the mail cross the network in clear text otherwise; -allow-from should name the proxy", "listen", o.addr, "allow_from", o.allow.Entries())
	}

	// TLS: the pair is held by a store that loads a changed pair while the listener runs
	var certs *certreload.Store
	if o.cert != "" {
		// a key and no certificate: the certificate host has the certificate of the key (before Prepare, which would not make one next to a key of ours)
		if err := certreload.FetchFirst(context.Background(), certreload.UpdateOptions{From: o.tlsUpdateFrom, Name: o.tlsUpdateName}, o.cert, o.key, log); err != nil {
			fmt.Fprintln(stderr, "eas-client:", err)
			return 2
		}
		if auto || o.tlsGenerate {
			if err := certreload.Prepare(certreload.GenerateOptions{CertFile: o.cert, KeyFile: o.key, Names: o.tlsNames, LeafDays: o.tlsDays}, auto, log); err != nil {
				fmt.Fprintln(stderr, "eas-client:", err)
				return 2
			}
		}
		var err error
		if certs, err = certreload.New(o.cert, o.key, log); err != nil {
			fmt.Fprintln(stderr, "eas-client:", err)
			return 2
		}
	}

	// one handler per listener: the same API and token, the list of the peers is the one of the listener
	newHandler := func(allow *ipacl.List, name string, healthOnly bool) http.Handler {
		handler := m.HandlerWith(client.HandlerOptions{Token: o.token, Docs: o.docs, DocsUI: o.docsUI, Allow: allow,
			OnDenied: func(remote string, r *http.Request) {
				log.Debug("request refused by "+name, "remote", remote, "method", r.Method, "path", r.URL.Path)
			}})
		if healthOnly {
			// the plain listener next to HTTPS has /health and nothing else: the API, the page and the specification carry the mail
			// and the token, and those are for HTTPS only
			inner := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					http.NotFound(w, r)
					return
				}
				inner.ServeHTTP(w, r)
			})
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), o.timeout)
			defer cancel()
			handler.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	if o.docs {
		// the browser loads Swagger UI from there; a moving location (@5) cannot be checked against hashes
		log.Info("the page /docs loads Swagger UI from outside", "swagger_ui", o.docsUI, "integrity_check", client.DocsUIHasIntegrity(o.docsUI))
	}
	srv := &http.Server{Addr: o.addr, Handler: newHandler(o.allow, "allow-from", false), ReadHeaderTimeout: 10 * time.Second}
	var healthSrv *http.Server
	var healthLn net.Listener
	if healthAddr != "" {
		var err error
		if healthLn, err = net.Listen("tcp", healthAddr); err != nil { // bound here: a port that is taken stops the start
			fmt.Fprintln(stderr, "eas-client:", err)
			return 1
		}
		defer healthLn.Close()
		healthSrv = &http.Server{Handler: newHandler(o.allowHealth, "allow-from-health", true), ReadHeaderTimeout: 10 * time.Second}
		if !listenaddr.Loopback(healthAddr) {
			log.Warn("the health listener is not on loopback: only -allow-from-health protects it, and the traffic is not encrypted",
				"listen_health", healthAddr, "allow_from_health", o.allowHealth.Entries())
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if certs != nil {
		srv.TLSConfig = certs.TLSConfig()
		if o.tlsUpdateFrom != "" {
			if err := certs.SetUpdate(certreload.UpdateOptions{From: o.tlsUpdateFrom, Name: o.tlsUpdateName, Every: o.tlsUpdateEvery}); err != nil {
				fmt.Fprintln(stderr, "eas-client:", err)
				return 1
			}
			log.Info("TLS certificate renewal: the certificate host is asked", "from", o.tlsUpdateFrom, "name", o.tlsUpdateName, "every", o.tlsUpdateEvery)
		}
		go certs.Watch(ctx, o.tlsCheck)
		go func() { // SIGHUP: load the certificate files now
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					log.Info("SIGHUP: reloading the TLS certificate")
					certs.Update(ctx) // a renewed certificate of the certificate host, if there is one, is in the file then
					_, _ = certs.Reload()
				}
			}
		}()
	}
	stopSave := store.AutoSave(time.Minute, func(err error) { log.Warn("the state could not be saved", "error", err.Error()) })
	defer func() { _ = stopSave() }()

	// the main listener is bound here as well, so that the log can say what the system made of the address
	mainLn, err := net.Listen("tcp", o.addr)
	if err != nil {
		fmt.Fprintln(stderr, "eas-client:", err)
		return 1
	}
	defer mainLn.Close()

	auth, authDetail := "none", "no token is set: every peer that -allow-from lets in may use it"
	if o.token != "" {
		auth, authDetail = "token", "header Authorization: Bearer <token>"
	}
	note := "plain HTTP because the address is loopback: it never leaves the machine"
	switch {
	case o.cert != "":
		note = "HTTPS only: there is no plain HTTP on this port"
	case o.allowHTTP:
		note = "plain HTTP chosen with -allow-http: that is for a proxy that terminates TLS, and -allow-from should name it"
	}
	mode := "read only"
	switch {
	case m.Accounts().AllowWrite && m.Accounts().AllowSend:
		mode = "read, write (mailbox changes) and send"
	case m.Accounts().AllowWrite:
		mode = "read and write (mailbox changes), no sending"
	case m.Accounts().AllowSend:
		mode = "read and send, no mailbox changes"
	}
	endpoints := []listenaddr.Endpoint{
		{Path: "/health", What: "liveness: answers OK", Auth: "none"},
		{Path: "/accounts/...", What: "the REST API, " + strconv.Itoa(len(client.Routes())) + " routes (/openapi.json lists them); it may: " + mode, Auth: auth, Detail: authDetail},
	}
	if o.docs {
		endpoints = append(endpoints,
			listenaddr.Endpoint{Path: "/openapi.json", What: "the specification of the API (OpenAPI 3)", Auth: "none", Detail: "a browser cannot send the header"},
			listenaddr.Endpoint{Path: "/docs", What: "the page where the API can be tried (Swagger UI, loaded by the browser from " + o.docsUI + ")", Auth: "none", Detail: "a browser cannot send the header; the calls from the page need the token"})
	} else {
		endpoints = append(endpoints, listenaddr.Endpoint{Path: "/openapi.json", What: "the specification of the API (OpenAPI 3)", Auth: auth, Detail: authDetail})
	}
	listenaddr.Listener{Name: "main", Configured: o.addr, Bound: mainLn.Addr(), TLS: o.cert != "", Allow: o.allow.Entries(), Endpoints: endpoints, Note: note, PlainWhy: listenaddr.PlainWhy(o.addr, o.allowHTTP)}.Log(log)
	if healthSrv != nil {
		listenaddr.Listener{Name: "health", Configured: healthAddr, Bound: healthLn.Addr(), Allow: o.allowHealth.Entries(),
			Endpoints: []listenaddr.Endpoint{{Path: "/health", What: "liveness: answers OK", Auth: "none"}},
			Note:      "for the health check of a container: the main port is HTTPS and a check would need the certificate; nothing else is served here"}.Log(log)
	}

	errc := make(chan error, 2)
	if healthSrv != nil {
		go func() { errc <- healthSrv.Serve(healthLn) }()
	}
	go func() {
		if o.cert != "" {
			errc <- srv.ServeTLS(mainLn, "", "") // the certificate comes from the store
		} else {
			errc <- srv.Serve(mainLn)
		}
	}()
	log.Info("REST API started", "allow_write", m.Accounts().AllowWrite, "allow_send", m.Accounts().AllowSend,
		"accounts", len(m.Accounts().Accounts))

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "eas-client:", err)
			return 1
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if healthSrv != nil {
			_ = healthSrv.Shutdown(sctx)
		}
		log.Info("REST API stopped")
	}
	return 0
}

// textFlags are the options that give the text of a mail and say whether a copy is kept: send, reply and forward have them.
type textFlags struct {
	body, bodyFile *string
	bodyStdin      *bool
	noSent         *bool
}

func addTextFlags(fs *flag.FlagSet) *textFlags {
	return &textFlags{
		body:      fs.String("body", "", "text of the mail"),
		bodyFile:  fs.String("body-file", "", "read the text of the mail from this file"),
		bodyStdin: fs.Bool("body-stdin", false, "read the text of the mail from stdin"),
		noSent:    fs.Bool("no-sent-copy", false, "do not keep a copy in Sent Items"),
	}
}

// text returns the text from the one source that was given.
func (t *textFlags) text(stdin io.Reader) (string, error) {
	sources := 0
	for _, set := range []bool{*t.body != "", *t.bodyFile != "", *t.bodyStdin} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return "", usageError("give the text with one of -body, -body-file and -body-stdin")
	}
	switch {
	case *t.bodyFile != "":
		b, err := os.ReadFile(*t.bodyFile)
		if err != nil {
			return "", usageError("-body-file: " + err.Error())
		}
		return string(b), nil
	case *t.bodyStdin:
		b, err := io.ReadAll(io.LimitReader(stdin, 10<<20))
		if err != nil {
			return "", usageError("-body-stdin: " + err.Error())
		}
		return string(b), nil
	}
	return *t.body, nil
}

// saveInSent is nil (the default, keep a copy) or a pointer to false.
func (t *textFlags) saveInSent() *bool {
	if *t.noSent {
		f := false
		return &f
	}
	return nil
}

// folderAndID reads the arguments of a command on one mail: "ID" or "FOLDER ID", followed by extra more arguments (the
// destination of a move). The folder may be left out when the id says it (see docs/CLIENT.md); the answer is "" then.
func folderAndID(pos []string, extra int, what string) (folder, id string, rest []string, err error) {
	switch n := len(pos) - extra; n {
	case 1:
		return "", pos[0], pos[1:], nil
	case 2:
		return pos[0], pos[1], pos[2:], nil
	}
	return "", "", nil, usageError(fmt.Sprintf("%s needs [FOLDER] ID%s", what, map[bool]string{true: " DESTINATION", false: ""}[extra > 0]))
}

// calendarCommand runs "calendar list" and "calendar get".
func calendarCommand(ctx context.Context, m *client.Manager, account string, args []string, stdin io.Reader, stderr io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, usageError("calendar needs list, get, ics, create or delete")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("calendar "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch sub {
	case "list":
		from := fs.String("from", "", "start of the list: now (the default), a date 2026-10-10 or a time 2026-10-10T09:00:00Z; earlier shows past events")
		to := fs.String("to", "", "end of the list (default: none)")
		since := fs.String("since", "", "date window asked of the server: 1d, 3d, 1w, 2w, 1m, 3m, 6m (default: as far back as -from needs)")
		limit := fs.String("limit", "50", "number of events, or all")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 1 {
			return nil, usageError("calendar list takes at most one folder")
		}
		p := client.EventParams{From: *from, To: *to, Since: *since}
		if *limit == "all" {
			p.Limit = -1
		} else if _, err := fmt.Sscanf(*limit, "%d", &p.Limit); err != nil || p.Limit < 1 {
			return nil, usageError("-limit is a number of 1 or more, or all")
		}
		folder := ""
		if len(pos) == 1 {
			folder = pos[0]
		}
		return m.Events(ctx, account, folder, p)

	case "create":
		subject := fs.String("subject", "", "subject of the event")
		location := fs.String("location", "", "location")
		start := fs.String("start", "", "start: a time 2026-10-20T09:00:00Z (UTC without a zone) or, with -all-day, a date 2026-12-24")
		end := fs.String("end", "", "end: a time; with -all-day the last day (default: the start day)")
		duration := fs.String("duration", "", "length instead of -end, for example 30m or 2h (default 1h)")
		allDay := fs.Bool("all-day", false, "an all-day event: -start and -end are dates")
		reminder := fs.Int("reminder", 0, "minutes before the start to remind; 0 none")
		busy := fs.String("busy", "", "free, tentative, busy (default), out_of_office or working_elsewhere")
		sensitivity := fs.String("sensitivity", "", "normal (default), personal, private or confidential")
		var required, optional, resources stringList
		fs.Var(&required, "attendee", "invite this person (repeatable, or comma separated; \"Name <a@b>\" gives a name): needs -send")
		fs.Var(&optional, "optional", "invite this person as an optional attendee")
		fs.Var(&resources, "resource", "invite this resource (a room, a device)")
		noInvite := fs.Bool("no-invite-mail", false, "do not mail the invitation: for a server that invites the attendees itself (HCL Traveler does not)")
		tf := addTextFlags(fs)
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		if len(pos) > 1 {
			return nil, usageError("calendar create takes at most one folder")
		}
		text, err := tf.text(stdin)
		if err != nil {
			return nil, err
		}
		req := client.EventRequest{Subject: *subject, Location: *location, Body: text, Start: *start, End: *end, Duration: *duration,
			AllDay: *allDay, Busy: *busy, Sensitivity: *sensitivity, ReminderMinutes: *reminder, NoInviteMail: *noInvite}
		for _, g := range []struct {
			kind string
			list stringList
		}{{"required", required}, {"optional", optional}, {"resource", resources}} {
			for _, v := range g.list {
				a := client.AttendeeRequest{Email: v, Type: g.kind}
				if p, err := mail.ParseAddress(v); err == nil {
					a.Email, a.Name = p.Address, p.Name
				}
				req.Attendees = append(req.Attendees, a)
			}
		}
		folder := ""
		if len(pos) == 1 {
			folder = pos[0]
		}
		return m.CreateEvent(ctx, account, folder, req)

	case "delete":
		noCancel := fs.Bool("no-cancel-mail", false, "do not mail the cancellation of a meeting that you organize to its attendees")
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "calendar delete")
		if err != nil {
			return nil, err
		}
		return m.DeleteEventWith(ctx, account, folder, id, !*noCancel)

	case "get", "ics":
		pos, err := parseArgs(fs, args)
		if err != nil {
			return nil, err
		}
		folder, id, _, err := folderAndID(pos, 0, "calendar "+sub)
		if err != nil {
			return nil, err
		}
		if sub == "ics" {
			return m.ICS(ctx, account, folder, id)
		}
		return m.Event(ctx, account, folder, id)
	}
	return nil, usageError(fmt.Sprintf("calendar %q: use list, get, ics, create or delete", sub))
}

// The default ports of the client: 8443 for HTTPS, which is the default, and 8080 for plain HTTP, which is a choice (-allow-http) and then
// on the loopback interface, unless an address is named.
const (
	defaultListenTLS   = ":8443"
	defaultListenPlain = "127.0.0.1:8080"
)

// defaultListen is the address that was given, or the default of the mode.
func defaultListen(given string, allowHTTP bool) string {
	switch {
	case given != "":
		return given
	case allowHTTP:
		return defaultListenPlain
	}
	return defaultListenTLS
}
