// eas-mcp is an MCP server for the mailboxes of eas-client: an AI client such as Claude Code lists the folders and the mail of a mailbox
// through it, and, when sending is allowed, sends mail. It speaks MCP over stdin and stdout; the log goes to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/internal/certreload"
	"github.com/nashcom/eas-tools/internal/clientcfg"
	"github.com/nashcom/eas-tools/internal/ipacl"
	"github.com/nashcom/eas-tools/probe"
)

const version = probe.Version

const usageText = `eas-mcp - MCP server for ActiveSync mailboxes (Claude Code and other MCP clients)

Usage: eas-mcp [options]

Transport: http is the default: the program serves the MCP endpoint /mcp on -listen (default :8445, HTTPS; 127.0.0.1:8081 with
-allow-http), for a container. -transport stdio is for a client that starts the program and talks to it over stdin and stdout.

The accounts, the config file (-config) and the environment (EASCLIENT_...) are those of eas-client, see docs/CLIENT.md.
The tools: accounts, server_info, list_folders, list_messages, get_message, and with -send send_message.
Reading is always allowed; sending is a switch of its own (-send), and -send-domains limits where mail may go.
Register it with Claude Code (stdio):  claude mcp add --transport stdio eas -- /path/to/eas-mcp -transport stdio -config /path/to/client.json

Options:
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Environ))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, environ func() []string) int {
	fs := flag.NewFlagSet("eas-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := clientcfg.Define(fs)
	var (
		transport = fs.String("transport", "http", "http (the default): serve the MCP endpoint /mcp on -listen, for a container; stdio: the client starts the program and talks over stdin and stdout")
		listen    = fs.String("listen", "", "http: address of the endpoint. Default: :8445 (HTTPS: TLS is the default), and 127.0.0.1:8081 with -allow-http (unencrypted is loopback unless an address is named)")
		tokenFile = fs.String("auth-token-file", "", "http: file with the bearer token of the endpoint (first line); or the environment variable EASCLIENT_AUTH_TOKEN")
		tlsCert   = fs.String("tls-cert", "", "http: certificate file; with -tls-key the endpoint is served over HTTPS (default: /run/secrets/tls/tls.crt, made there when it is not)")
		tlsKey    = fs.String("tls-key", "", "http: private key file")
		tlsGen    = fs.Bool("tls-generate", false, "http: make a small CA and a certificate when -tls-cert and -tls-key do not exist yet (never overwritten)")
		tlsDays   = fs.Int("tls-valid-days", certreload.DefaultLeafDays, "http: how many days a certificate of the micro CA is valid (1 to 3650); made again at every start")
		tlsCheck  = fs.Duration("tls-check-interval", time.Minute, "http: how often the certificate and key files are looked at")
		tlsFrom   = fs.String("tls-update-from", "", "http: host:port of a certificate host (such as the Domino CertMgr) that is asked for a renewed certificate, see docs/TLS.md")
		tlsName   = fs.String("tls-update-name", "", "http: the name that the certificate host is asked for (SNI); default: the host name of this machine")
		tlsEvery  = fs.Duration("tls-update-interval", certreload.DefaultUpdateEvery, "http: how often the certificate host is asked")
		allowHTTP = fs.Bool("allow-http", false, "http: serve plain HTTP although there is no certificate: for a proxy that terminates TLS")
		health    = fs.String("listen-health", "", "http: with TLS a second listener, plain HTTP, /health only; default: the loopback interface and the port after -listen")
	)
	var allowFrom, allowFromHealth, tlsNames clientcfg.StringList
	fs.Var(&allowFrom, "allow-from", "http: only these peers may connect: addresses, networks (192.168.0.0/16), ranges and the name private; loopback is always allowed; default: no restriction")
	fs.Var(&allowFromHealth, "allow-from-health", "http: the peers that may connect to the health listener; required when it is not on loopback")
	fs.Var(&tlsNames, "tls-names", "http: the names of a certificate that is made; default: localhost, 127.0.0.1, ::1 and the host name")
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
	mode := *transport
	if mode != "stdio" && mode != "http" {
		return fail("-transport %q: use http or stdio", *transport)
	}
	if mode == "stdio" && *cf.PwStdin {
		return fail("-password-stdin cannot be used with -transport stdio: stdin carries the MCP protocol; use -password-file or the environment variable")
	}

	// stdout is the MCP channel: everything else goes to stderr
	logger, err := cf.Logger(res, slog.LevelInfo, stderr, "eas-mcp", version)
	if err != nil {
		return fail("%v", err)
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

	if *cf.Write {
		logger.Warn("-write has no effect: this version of eas-mcp has no tool that changes a mailbox")
	}
	info := m.Accounts()
	names := make([]string, 0, len(info.Accounts))
	for _, a := range info.Accounts {
		names = append(names, a.Name)
	}
	logger.Info("started", "version", version, "transport", mode, "accounts", names, "allow_send", *cf.Send, "send_domains", []string(cf.SendDomains))
	if *cf.Send && len(cf.SendDomains) == 0 {
		logger.Warn("sending is allowed to every domain: -send-domains limits where mail may go")
	}
	mcpSrv := newServer(m, *cf.Send, cf.SendDomains, *cf.Timeout)

	if mode == "http" {
		token := res.Values["auth_token"]
		if *tokenFile != "" {
			raw, err := os.ReadFile(*tokenFile)
			if err != nil {
				return fail("-auth-token-file: %v", err)
			}
			t, err := clientcfg.FirstLine(strings.NewReader(string(raw)))
			if err != nil || t == "" {
				return fail("-auth-token-file: no token in %s", *tokenFile)
			}
			token = t
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
		if *tlsFrom != "" && *allowHTTP {
			return fail("-tls-update-from renews the certificate of a listener that has TLS: it excludes -allow-http")
		}
		return serveHTTP(mcpSrv, store, logger, httpOptions{send: *cf.Send, addr: defaultListen(*listen, *allowHTTP), token: token, cert: *tlsCert, key: *tlsKey, allow: acl,
			allowHTTP: *allowHTTP, tlsGenerate: *tlsGen, tlsNames: tlsNames, tlsCheck: *tlsCheck, tlsDays: *tlsDays,
			tlsUpdateFrom: *tlsFrom, tlsUpdateName: *tlsName, tlsUpdateEvery: *tlsEvery, healthAddr: *health, allowHealth: aclHealth}, stderr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcpSrv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		logger.Error("the MCP connection ended", "error", err.Error())
		return 1
	}
	return 0
}
