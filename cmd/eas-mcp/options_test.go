package main

import (
	"encoding/json"
	"flag"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/internal/clientcfg"
)

// values has a value for every option, different from its default. A new option has to be added here: the test below fails for an option that
// is not, so that every option is tried on the command line, in the environment and in the file.
var values = map[string]string{
	// of clientcfg, in common with eas-client
	"account": "lab", "server": "traveler.example.com", "email": "alice@example.com", "user": "alice", "password-env": "MY_PASSWORD_VARIABLE",
	"password-file": "/run/secrets/pw", "device-id": "ABC123", "profile": "iphone", "provision": "never", "insecure": "true",
	"ca-file": "/run/secrets/ca.pem", "state-file": "/data/state.json", "no-state": "true", "write": "true", "send": "true",
	"send-domains": "example.com,*.example.org", "timeout": "2m", "timezone": "Europe/Berlin", "trace": "/tmp/trace.log", "verbose": "true",
	"debug": "true", "log-format": "json",
	// of eas-mcp
	"transport": "stdio", "listen": ":9999", "auth-token-file": "/run/secrets/token", "tls-cert": "/c/tls.crt", "tls-key": "/c/tls.key",
	"tls-generate": "true", "tls-valid-days": "90", "tls-check-interval": "5m", "tls-update-from": "certmgr.example.com:443",
	"tls-update-name": "eas-mcp.example.com", "tls-update-interval": "2h", "allow-http": "true", "listen-health": "127.0.0.1:9998",
	"oidc-issuer": "https://idp.example.com/realms/lab", "oidc-audience": "https://eas-mcp.example.com/mcp", "oidc-user-claim": "preferred_username",
	"oidc-cache-file": "/data/oidc-keys.json", "oidc-compat-issuer": "origin", "stateless": "false", "audit-log": "/var/log/eas-mcp-audit.json", "public-url": "https://eas-mcp.example.com/mcp", "oidc-users": "alice@example.com,*@example.org",
	"oidc-scopes": "mail.read,mail.send", "allow-from": "10.0.0.0/8,private", "allow-from-health": "10.1.0.5", "tls-names": "eas-mcp.example.com,10.0.0.9",
}

// notSettings are the options that are only for the command line: the config file itself (the variable EASCLIENT_CONFIG names it), the
// version, the health check (a variable that turns the program into one is a trap), and the password on stdin.
var notSettings = map[string]bool{"config": true, "version": true, "health-check": true, "password-stdin": true}

func newFlagSet() (*flag.FlagSet, *clientcfg.Flags, *options) {
	fs := flag.NewFlagSet("eas-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, clientcfg.Define(fs), defineOptions(fs)
}

// expected is the value that an option has after it was set to raw, as the flag package says it.
func expected(t *testing.T, name, raw string) string {
	t.Helper()
	fs, _, _ := newFlagSet()
	if err := fs.Set(name, raw); err != nil {
		t.Fatalf("%s=%q: %v", name, raw, err)
	}
	return fs.Lookup(name).Value.String()
}

func allOptions() []string {
	fs, _, _ := newFlagSet()
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	return names
}

func TestEveryOptionHasAValueForTheTests(t *testing.T) {
	for _, name := range allOptions() {
		if _, ok := values[name]; !ok && !notSettings[name] {
			t.Errorf("-%s is an option and has no value in values: add it, so that it is tried on the command line, in the environment and in the file", name)
		}
	}
	for name := range values {
		fs, _, _ := newFlagSet()
		if fs.Lookup(name) == nil {
			t.Errorf("values has -%s, which is not an option any more", name)
		}
	}
}

func TestEveryOptionCanBeGivenOnTheCommandLine(t *testing.T) {
	var args []string
	for name, raw := range values {
		args = append(args, "-"+name+"="+raw)
	}
	fs, _, _ := newFlagSet()
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	for name, raw := range values {
		if got, want := fs.Lookup(name).Value.String(), expected(t, name, raw); got != want {
			t.Errorf("-%s: %q, want %q", name, got, want)
		}
	}
}

func TestEveryOptionCanBeGivenInTheEnvironment(t *testing.T) {
	var env []string
	for name, raw := range values {
		env = append(env, "EASCLIENT_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))+"="+raw)
	}
	fs, cf, _ := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	res, err := cf.Resolve(fs, func() []string { return env }, nil, "auth_token")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) > 0 {
		t.Errorf("warnings, an environment variable that is not known: %v", res.Warnings)
	}
	for name, raw := range values {
		if got, want := fs.Lookup(name).Value.String(), expected(t, name, raw); got != want {
			t.Errorf("EASCLIENT_%s: %q, want %q", strings.ToUpper(strings.ReplaceAll(name, "-", "_")), got, want)
		}
	}
}

func TestEveryOptionCanBeGivenInTheConfigFile(t *testing.T) {
	doc := map[string]any{}
	for name, raw := range values {
		key := strings.ReplaceAll(name, "-", "_")
		switch fs, _, _ := newFlagSet(); fs.Lookup(name).Value.(type) {
		case *clientcfg.StringList:
			doc[key] = strings.Split(raw, ",") // a list is a JSON array
		default:
			if raw == "true" || raw == "false" {
				doc[key] = raw == "true"
			} else if n, err := strconv.Atoi(raw); err == nil {
				doc[key] = n
			} else {
				doc[key] = raw // text, and a duration as "5m"
			}
		}
	}
	doc["accounts"] = []map[string]string{{"name": "lab", "server": "s", "email": "a@example.com", "password_env": "X"}}
	raw, _ := json.Marshal(doc)

	fs, cf, _ := newFlagSet()
	if err := fs.Parse([]string{"-config", "mcp.json"}); err != nil {
		t.Fatal(err)
	}
	res, err := cf.Resolve(fs, func() []string { return nil }, func(string) ([]byte, error) { return raw, nil }, "auth_token")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) > 0 {
		t.Errorf("warnings: %v", res.Warnings)
	}
	for name, raw := range values {
		if got, want := fs.Lookup(name).Value.String(), expected(t, name, raw); got != want {
			t.Errorf("%s in the file: %q, want %q", strings.ReplaceAll(name, "-", "_"), got, want)
		}
	}
}

func TestTheSecretsAreNotOptions(t *testing.T) {
	// a password and a token never come from the command line, where every user of the machine sees them: the environment and the file only
	fs, cf, _ := newFlagSet()
	for _, name := range []string{"password", "auth-token", "auth_token"} {
		if fs.Lookup(name) != nil {
			t.Errorf("-%s is an option: a secret on the command line is visible to every user of the machine", name)
		}
	}
	_ = fs.Parse(nil)
	res, err := cf.Resolve(fs, func() []string { return []string{"EASCLIENT_PASSWORD=pw", "EASCLIENT_AUTH_TOKEN=tok"} }, nil, "auth_token")
	if err != nil || res.Values["password"] != "pw" || res.Values["auth_token"] != "tok" {
		t.Errorf("from the environment: %+v %v", res.Values, err)
	}
}
