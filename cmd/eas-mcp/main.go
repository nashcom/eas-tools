// eas-mcp is an MCP server for the mailboxes of eas-client: an AI client such as Claude Code lists the folders and the mail of a mailbox
// through it, and, when sending is allowed, sends mail. It speaks MCP over stdin and stdout; the log goes to stderr.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/clientcfg"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/internal/listenaddr"
	"github.com/nashcom/eas-tools/internal/oidcauth"
	"github.com/nashcom/eas-tools/probe"
)

const version = probe.Version

const usageText = `eas-mcp - MCP server for ActiveSync mailboxes (Claude Code and other MCP clients)

Usage: eas-mcp [options]

Transport: http is the default: the program serves the MCP endpoint /mcp on -listen (default :8445, HTTPS; 127.0.0.1:8081 with
-allow-http), for a container. -transport stdio is for a client that starts the program and talks to it over stdin and stdout.

The accounts, the config file (-config) and the environment (EASCLIENT_...) are those of eas-client, see docs/CLIENT.md.
The tools: accounts, server_info, list_folders, list_messages, get_message, get_message_eml, list_events, get_event, get_event_ics, list_contacts, get_contact, get_contact_vcard, export_contacts; with -send send_message, reply_message, forward_message;
with -write mark_read, mark_unread, move_message, delete_message, create_event, delete_event. Reading is always allowed; sending and writing are switches of their
own, and -send-domains limits where mail may go.
Register it with Claude Code (stdio):  claude mcp add --transport stdio eas -- /path/to/eas-mcp -transport stdio -config /path/to/client.json

Options:
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ))
}

// options are the options of eas-mcp itself, next to those of clientcfg that it has in common with eas-client. Every one of them can be given
// on the command line, as an environment variable (EASCLIENT_ and the name in upper case, "-" written as "_") and as a key in the config file (the
// name, "-" written as "_"): internal/settings does that for all the options of the flag set, and a test checks it.
type options struct {
	transport       *string
	listen          *string
	tokenFile       *string
	tlsCert         *string
	tlsKey          *string
	tlsGen          *bool
	tlsDays         *int
	tlsCheck        *time.Duration
	tlsFrom         *string
	tlsName         *string
	tlsEvery        *time.Duration
	allowHTTP       *bool
	health          *string
	oidcIssuer      *string
	oidcAudience    *string
	oidcClaim       *string
	oidcCache       *string
	oidcCompat      *string
	auditLog        *string
	stateless       *bool
	publicURL       *string
	oidcUsers       clientcfg.StringList
	oidcScopes      clientcfg.StringList
	allowFrom       clientcfg.StringList
	allowFromHealth clientcfg.StringList
	tlsNames        clientcfg.StringList
}

// defineOptions adds the options of eas-mcp to the flag set.
func defineOptions(fs *flag.FlagSet) *options {
	o := &options{}
	o.transport = fs.String("transport", "http", "http (the default): serve the MCP endpoint /mcp on -listen, for a container; stdio: the client starts the program and talks over stdin and stdout")
	o.listen = fs.String("listen", "", "http: address of the endpoint. Default: :8445 (HTTPS: TLS is the default), and 127.0.0.1:8081 with -allow-http (unencrypted is loopback unless an address is named)")
	o.tokenFile = fs.String("auth-token-file", "", "http: file with the bearer token of the endpoint (first line); or the environment variable EASCLIENT_AUTH_TOKEN")
	o.tlsCert = fs.String("tls-cert", "", "http: certificate file; with -tls-key the endpoint is served over HTTPS (default: /run/secrets/tls/tls.crt, made there when it is not)")
	o.tlsKey = fs.String("tls-key", "", "http: private key file")
	o.tlsGen = fs.Bool("tls-generate", false, "http: make a small CA and a certificate when -tls-cert and -tls-key do not exist yet (never overwritten)")
	o.tlsDays = fs.Int("tls-valid-days", certreload.DefaultLeafDays, "http: how many days a certificate of the micro CA is valid (1 to 3650); made again at every start")
	o.tlsCheck = fs.Duration("tls-check-interval", time.Minute, "http: how often the certificate and key files are looked at")
	o.tlsFrom = fs.String("tls-update-from", "", "http: host:port of a certificate host (such as the Domino CertMgr) that is asked for a renewed certificate, see docs/TLS.md")
	o.tlsName = fs.String("tls-update-name", "", "http: the name that the certificate host is asked for (SNI); default: the host name of this machine")
	o.tlsEvery = fs.Duration("tls-update-interval", certreload.DefaultUpdateEvery, "http: how often the certificate host is asked")
	o.allowHTTP = fs.Bool("allow-http", false, "http: serve plain HTTP although there is no certificate: for a proxy that terminates TLS")
	o.health = fs.String("listen-health", "", "http: with TLS a second listener, plain HTTP, /health only; default: the loopback interface and the port after -listen")
	o.oidcIssuer = fs.String("oidc-issuer", "", "http: the issuer URL of an OpenID Connect provider: clients log in there and send a JWT access token, which is checked here (OIDC is tried first, the local token is the fallback)")
	o.oidcAudience = fs.String("oidc-audience", "", "http: what the JWT has to be issued for (its aud claim); required with -oidc-issuer unless -public-url is given, which is its default")
	o.oidcClaim = fs.String("oidc-user-claim", "", "http: the claim of the JWT that names the user; default: the first of email, preferred_username, upn and sub")
	o.oidcCache = fs.String("oidc-cache-file", "", "http: where the signing keys of the provider are saved, for use when it cannot be reached; default: next to the state file")
	o.oidcCompat = fs.String("oidc-compat-issuer", "provider", "http: what /.well-known/oauth-authorization-server and /.well-known/openid-configuration of this server answer, for clients that look for the login server here: provider (the discovery document of the provider as it is), origin (the same with the issuer changed to the address of this server) or off (404)")
	o.stateless = fs.Bool("stateless", true, "http: no session of the protocol: every request stands alone, and the answer carries no Mcp-Session-Id (needed for the protocol 2026-07-28, older clients work as well; no requests of the server to the client, which the tools do not make); -stateless=false keeps sessions")
	o.auditLog = fs.String("audit-log", "", "the audit log: a file (appended to, readable by the owner only; SIGHUP opens it again, for log rotation), stderr, or stdout (not with -transport stdio): one JSON object per line in the OpenTelemetry log data model, with who called which tool, on which account and item, to which recipients, and the result; never the text of a mail; default: none")
	o.publicURL = fs.String("public-url", "", "http: optional: the address that the clients use for the endpoint, such as https://eas-mcp.example.com:8445/mcp, which the protected resource metadata names; default: the address of each request (the host and port that the client used, X-Forwarded-Host and X-Forwarded-Proto behind a proxy)")
	fs.Var(&o.oidcUsers, "oidc-users", "http: the users that are allowed with a JWT: the e-mail address of the token against these patterns, * is any characters: alice@example.com, *@example.com or * for everybody; required with -oidc-issuer")
	fs.Var(&o.oidcScopes, "oidc-scopes", "http: scopes that a JWT has to have (and that the metadata offers to the clients); default: none")
	fs.Var(&o.allowFrom, "allow-from", "http: only these peers may connect: addresses, networks (192.168.0.0/16), ranges and the name private; loopback is always allowed; default: no restriction")
	fs.Var(&o.allowFromHealth, "allow-from-health", "http: the peers that may connect to the health listener; required when it is not on loopback")
	fs.Var(&o.tlsNames, "tls-names", "http: the names of a certificate that is made; default: localhost, 127.0.0.1, ::1 and the host name")
	return o
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, environ func() []string) int {
	fs := flag.NewFlagSet("eas-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := clientcfg.Define(fs)
	o := defineOptions(fs)
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
		fmt.Fprintf(stderr, "eas-mcp: "+format+"\n", a...)
		return 2
	}
	res, err := cf.Resolve(fs, environ, os.ReadFile, "auth_token")
	if err != nil {
		return fail("%v", err)
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "eas-mcp: warning:", w)
	}
	if *cf.ShowVersion {
		fmt.Fprintln(stdout, "eas-mcp", version)
		return 0
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q: eas-mcp takes options only", fs.Arg(0))
	}
	if *cf.HealthCheck { // before the accounts: the check needs the address only
		port, err := listenaddr.HealthPort(defaultListen(*o.listen, *o.allowHTTP), *o.health, *o.tlsCert != "", *o.allowHTTP)
		if err != nil {
			return fail("health-check: %v", err)
		}
		return listenaddr.CheckHealth(port, stderr)
	}
	mode := *o.transport
	if mode != "stdio" && mode != "http" {
		return fail("-transport %q: use http or stdio", *o.transport)
	}
	if mode == "stdio" && *cf.PwStdin {
		return fail("-password-stdin cannot be used with -transport stdio: stdin carries the MCP protocol; use -password-file or the environment variable")
	}

	// stdout is the MCP channel: everything else goes to stderr
	logger, err := cf.Logger(res, slog.LevelInfo, stderr, "eas-mcp", version)
	if err != nil {
		return fail("%v", err)
	}
	aud, err := newAuditor(*o.auditLog, version, mode == "stdio")
	if err != nil {
		return fail("%v", err)
	}
	defer aud.close()
	if aud != nil {
		go func() { // SIGHUP: the audit file is opened again, after the rotation of the log
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			for range hup {
				if err := aud.reopen(); err != nil {
					logger.Error("the audit log could not be opened again: it goes on in the old file", "error", err.Error())
				} else {
					logger.Info("SIGHUP: the audit log was opened again")
				}
			}
		}()
	}
	accounts, err := cf.Accounts(res, stdin, false)
	if err != nil {
		return fail("%v", err)
	}
	m, store, closeAll, err := cf.Open(accounts, logger)
	if err != nil {
		return fail("%v", err)
	}
	defer closeAll()

	info := m.Accounts()
	names := make([]string, 0, len(info.Accounts))
	for _, a := range info.Accounts {
		names = append(names, a.Name)
	}
	logger.Info("started", "version", version, "transport", mode, "accounts", names, "allow_send", *cf.Send, "send_domains", []string(cf.SendDomains))
	if *cf.Send && len(cf.SendDomains) == 0 {
		logger.Warn("sending is allowed to every domain: -send-domains limits where mail may go")
	}
	aud.event(context.Background(), slog.LevelInfo, "mcp.server.start", "the server started", "mcp.transport", mode, "mcp.accounts", names, "mcp.allow_send", *cf.Send,
		"mcp.allow_write", *cf.Write, "mcp.send_domains", []string(cf.SendDomains))
	defer aud.event(context.Background(), slog.LevelInfo, "mcp.server.stop", "the server stopped")
	mcpSrv := newServer(m, serverOptions{Send: *cf.Send, Write: *cf.Write, SendDomains: cf.SendDomains, Timeout: *cf.Timeout, Log: logger, Audit: aud})

	if mode == "http" {
		token := res.Values["auth_token"]
		if *o.tokenFile != "" {
			raw, err := os.ReadFile(*o.tokenFile)
			if err != nil {
				return fail("-auth-token-file: %v", err)
			}
			t, err := clientcfg.FirstLine(strings.NewReader(string(raw)))
			if err != nil || t == "" {
				return fail("-auth-token-file: no token in %s", *o.tokenFile)
			}
			token = t
		}
		var verifier *oidcauth.Verifier
		if *o.oidcIssuer == "" {
			if len(o.oidcUsers) > 0 || len(o.oidcScopes) > 0 || *o.oidcAudience != "" || *o.oidcClaim != "" || *o.oidcCache != "" {
				return fail("the -oidc-... options need -oidc-issuer")
			}
		} else {
			audience := *o.oidcAudience
			if audience == "" {
				audience = *o.publicURL
			}
			if audience == "" {
				return fail("-oidc-audience is needed: what the JWT has to be issued for (the URL of the endpoint, or the name that the provider knows it by); -public-url is its default")
			}
			cache := *o.oidcCache
			if cache == "" {
				if sp := cf.StateFilePath(); sp != "" {
					cache = filepath.Join(filepath.Dir(sp), "oidc-keys.json")
				}
			}
			hc, err := issuerClient(*cf.CAFile)
			if err != nil {
				return fail("-ca-file: %v", err)
			}
			cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			verifier, err = oidcauth.New(cctx, oidcauth.Config{Issuer: *o.oidcIssuer, Audience: audience, Users: o.oidcUsers, UserClaim: *o.oidcClaim, CacheFile: cache, HTTPClient: hc, Log: logger})
			cancel()
			if err != nil {
				return fail("-oidc-issuer: %v", err)
			}
		}
		an, err := newAuthn(token, verifier, *o.publicURL, o.oidcScopes, logger)
		if err != nil {
			return fail("%v", err)
		}
		switch *o.oidcCompat {
		case "provider", "origin", "off":
			an.compat = *o.oidcCompat
		default:
			return fail("-oidc-compat-issuer %q: use provider, origin or off", *o.oidcCompat)
		}
		an.audit = aud
		var issuer string
		var users []string
		if verifier != nil {
			issuer, users = verifier.Issuer(), []string(o.oidcUsers)
		}
		aud.event(context.Background(), slog.LevelInfo, "mcp.auth.config", "the authentication of the endpoint", "auth.oidc_issuer", issuer, "auth.oidc_users", users,
			"auth.oidc_scopes", []string(o.oidcScopes), "auth.local_token", token != "", "http.stateless", *o.stateless)
		acl, err := ipacl.Parse(o.allowFrom)
		if err != nil {
			return fail("allow-from: %v", err)
		}
		aclHealth, err := ipacl.Parse(o.allowFromHealth)
		if err != nil {
			return fail("allow-from-health: %v", err)
		}
		if *o.tlsDays < 1 || *o.tlsDays > certreload.MaxLeafDays {
			return fail("-tls-valid-days: %d, use a number from 1 to %d (default %d)", *o.tlsDays, certreload.MaxLeafDays, certreload.DefaultLeafDays)
		}
		if *o.tlsCert != "" && *o.allowHTTP {
			return fail("-allow-http and -tls-cert exclude each other: a listener that has TLS speaks nothing else")
		}
		if *o.tlsFrom != "" && *o.allowHTTP {
			return fail("-tls-update-from renews the certificate of a listener that has TLS: it excludes -allow-http")
		}
		return serveHTTP(mcpSrv, store, logger, httpOptions{send: *cf.Send, stateless: *o.stateless, addr: defaultListen(*o.listen, *o.allowHTTP), authn: an, cert: *o.tlsCert, key: *o.tlsKey, allow: acl,
			allowHTTP: *o.allowHTTP, tlsGenerate: *o.tlsGen, tlsNames: o.tlsNames, tlsCheck: *o.tlsCheck, tlsDays: *o.tlsDays,
			tlsUpdateFrom: *o.tlsFrom, tlsUpdateName: *o.tlsName, tlsUpdateEvery: *o.tlsEvery, healthAddr: *o.health, allowHealth: aclHealth}, stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcpSrv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logger.Error("the MCP connection ended", "error", err.Error())
		return 1
	}
	return 0
}

// issuerClient is the HTTP client for the OIDC provider: the roots of the system, and the CA of -ca-file when there is one (a provider with
// a certificate of a private CA).
func issuerClient(caFile string) (*http.Client, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	if caFile == "" {
		return c, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s has no certificate", caFile)
	}
	c.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}
