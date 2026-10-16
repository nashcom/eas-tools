package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/internal/listenaddr"
	"github.com/nashcom/eas-tools/probe"
)

// The ports: the pattern of eas-client (8443 HTTPS, 8080 plain, 8444 health), one step up so that both can run on one machine.
const (
	defaultListenTLS   = ":8445"
	defaultListenPlain = "127.0.0.1:8081"
)

// defaultListen is the address when none is given: HTTPS on all interfaces, and with -allow-http plain HTTP on the loopback interface only
// (unencrypted is loopback unless an address is named).
func defaultListen(given string, allowHTTP bool) string {
	switch {
	case given != "":
		return given
	case allowHTTP:
		return defaultListenPlain
	}
	return defaultListenTLS
}

// httpOptions are the settings of the transport "http". They mean what the settings of the same name of eas-client serve mean, see
// docs/TLS.md: TLS is the default, plain HTTP is a choice, and the certificate is looked for in /run/secrets/tls.
type httpOptions struct {
	addr, cert, key string
	authn           *authn
	allow           *ipacl.List
	allowHTTP       bool
	send            bool // sending is allowed: only for the warning
	stateless       bool // no session of the protocol: every request stands alone
	tlsGenerate     bool
	tlsNames        []string
	tlsCheck        time.Duration
	tlsDays         int
	tlsUpdateFrom   string
	tlsUpdateName   string
	tlsUpdateEvery  time.Duration
	healthAddr      string // with TLS: the listener of /health next to HTTPS; default loopback, the next port
	allowHealth     *ipacl.List
}

// httpHandler is what a listener serves: /mcp (the MCP endpoint, with the token) and /health. The list of the peers is checked first, for
// every request. A health listener (healthOnly) serves /health and nothing else.
func httpHandler(srv *mcp.Server, a *authn, allow *ipacl.List, healthOnly, stateless bool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "OK\n")
	})
	if !healthOnly {
		mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Logger: log, Stateless: stateless})
		mux.Handle("/mcp", a.wrap(mcpHandler))
		if a != nil && a.oidc != nil { // where a client finds the provider to log in with: no token is needed to read it
			for _, p := range a.metaPaths {
				mux.Handle(p, a.metadata())
			}
			if a.compat != "off" {
				for _, p := range legacyPaths { // where a client that does not read the metadata looks for the login server
					mux.Handle(p, a.authServerMetadata())
				}
			}
		}
	}
	// everything that no pattern above takes: the log says which addresses the clients ask for and do not find (the query is not logged: it can hold secrets)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Info("a request for an address that does not exist", "method", r.Method, "path", r.URL.Path, "host", r.Host, "remote", r.RemoteAddr, "user_agent", r.UserAgent())
		http.NotFound(w, r)
	})
	return allow.Wrap(mux, func(remote string, r *http.Request) {
		log.Debug("request refused by allow-from", "remote", remote, "method", r.Method, "path", r.URL.Path)
	})
}

// serveHTTP serves the MCP endpoint over HTTPS (the default) or, by choice, plain HTTP, until the program is stopped.
func serveHTTP(mcpSrv *mcp.Server, store *probe.Store, log *slog.Logger, o httpOptions, stderr io.Writer) int {
	fail := func(code int, a ...any) int {
		fmt.Fprintln(stderr, append([]any{"eas-mcp:"}, a...)...)
		return code
	}
	if (o.cert == "") != (o.key == "") {
		return fail(2, "-tls-cert and -tls-key go together")
	}
	// TLS is the default: without a certificate named, without the choice of plain HTTP, and unless the address is loopback, the certificate
	// is looked for in /run/secrets/tls, and made there when it is not
	auto := false
	if o.cert == "" && !o.allowHTTP && !listenaddr.Loopback(o.addr) {
		o.cert, o.key = certreload.DefaultFiles()
		auto = true
	}
	if o.cert == "" && o.tlsGenerate {
		return fail(2, "-tls-generate makes the certificate of a listener that has TLS: it excludes -allow-http and a loopback -listen, where there is none")
	}
	if o.cert == "" && (o.healthAddr != "" || len(o.allowHealth.Entries()) > 0) {
		return fail(2, "-listen-health and -allow-from-health are for the health listener next to HTTPS: there is none without TLS (-listen is plain already)")
	}
	healthAddr := ""
	if o.cert != "" {
		var err error
		if healthAddr, err = listenaddr.Second(o.addr, o.healthAddr, o.allowHealth.Entries(), "allow_from_health", false); err != nil {
			return fail(2, "-listen-health:", err)
		}
	}
	if !listenaddr.Loopback(o.addr) && !o.authn.enabled() {
		// not an error: the network can protect the endpoint (a proxy, a firewall, the list of peers); the log says what the endpoint is
		log.Warn("the MCP endpoint has no token and is not on loopback: every peer that can reach it may read the mailboxes (and send mail with -send); protect it on the network or with -allow-from, or set EASCLIENT_AUTH_TOKEN or use OIDC (-oidc-issuer)",
			"listen", o.addr, "allow_from", o.allow.Entries(), "allow_send", o.send)
	}
	if err := listenaddr.NeedsHTTPSwitch(o.addr, o.cert != "", o.allowHTTP); err != nil {
		return fail(2, err)
	}
	if o.allowHTTP && !listenaddr.Loopback(o.addr) {
		log.Warn("the endpoint is served over plain HTTP (-allow-http): that is for a proxy that terminates TLS, the token and the mail cross the network in clear text otherwise; -allow-from should name the proxy",
			"listen", o.addr, "allow_from", o.allow.Entries())
	}

	// TLS: the pair is held by a store that loads a changed pair while the listener runs
	var certs *certreload.Store
	if o.cert != "" {
		if err := certreload.FetchFirst(context.Background(), certreload.UpdateOptions{From: o.tlsUpdateFrom, Name: o.tlsUpdateName}, o.cert, o.key, log); err != nil {
			return fail(2, err)
		}
		if auto || o.tlsGenerate {
			if err := certreload.Prepare(certreload.GenerateOptions{CertFile: o.cert, KeyFile: o.key, Names: o.tlsNames, LeafDays: o.tlsDays}, auto, log); err != nil {
				return fail(2, err)
			}
		}
		var err error
		if certs, err = certreload.New(o.cert, o.key, log); err != nil {
			return fail(2, err)
		}
	}

	srv := &http.Server{Addr: o.addr, Handler: httpHandler(mcpSrv, o.authn, o.allow, false, o.stateless, log), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	var healthSrv *http.Server
	var healthLn net.Listener
	if healthAddr != "" {
		var err error
		if healthLn, err = net.Listen("tcp", healthAddr); err != nil { // bound here: a port that is taken stops the start
			return fail(1, err)
		}
		defer healthLn.Close()
		healthSrv = &http.Server{Handler: httpHandler(mcpSrv, nil, o.allowHealth, true, o.stateless, log), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
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
				return fail(1, err)
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
					certs.Update(ctx)
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
		return fail(1, err)
	}
	defer mainLn.Close()

	auth, authDetail := o.authn.describe()
	note := "plain HTTP because the address is loopback: it never leaves the machine"
	switch {
	case o.cert != "":
		note = "HTTPS only: there is no plain HTTP on this port"
	case o.allowHTTP:
		note = "plain HTTP chosen with -allow-http: that is for a proxy that terminates TLS, and -allow-from should name it"
	}
	listenaddr.Listener{Name: "main", Configured: o.addr, Bound: mainLn.Addr(), TLS: o.cert != "", Allow: o.allow.Entries(),
		Endpoints: []listenaddr.Endpoint{
			{Path: "/health", What: "liveness: answers OK", Auth: "none"},
			{Path: "/mcp", What: "the MCP endpoint (Streamable HTTP)", Auth: auth, Detail: authDetail},
		}, Note: note, PlainWhy: listenaddr.PlainWhy(o.addr, o.allowHTTP)}.Log(log)
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
	log.Info("MCP endpoint started", "stateless", o.stateless, "protocol_versions", strings.Join(mcp.SupportedProtocolVersions(), ","))

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fail(1, err)
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if healthSrv != nil {
			_ = healthSrv.Shutdown(sctx)
		}
		log.Info("MCP endpoint stopped")
	}
	return 0
}
