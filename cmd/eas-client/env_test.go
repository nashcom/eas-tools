package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// serve with nothing but environment variables: the way a container is started.
func TestServeIsConfiguredByTheEnvironment(t *testing.T) {
	base := []string{
		"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw",
		"EASCLIENT_DOCS=true", "EASCLIENT_TIMEZONE=Europe/Berlin", "EASCLIENT_NO_STATE=true",
	}
	runWith := func(extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}

	// on 0.0.0.0 without a token it does not start: the listen address of the environment is the one that counts
	if code, errs := runWith("EASCLIENT_LISTEN=0.0.0.0:0"); code != 2 || !strings.Contains(errs, "token") {
		t.Errorf("no token: %d %s", code, errs)
	}
	// with the token it tries to listen, and an address that cannot be listened on stops it at once with the error of the system
	if code, errs := runWith("EASCLIENT_LISTEN=bad-address", "EASCLIENT_AUTH_TOKEN=tok", "EASCLIENT_ALLOW_HTTP=true"); code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("a token and a bad address: %d %s", code, errs)
	}
	// a wrong value of the environment is reported, not ignored
	if code, errs := runWith("EASCLIENT_DOCS=maybe"); code != 2 || !strings.Contains(errs, "EASCLIENT_DOCS") {
		t.Errorf("a wrong switch: %d %s", code, errs)
	}
	if code, errs := runWith("EASCLIENT_TIMEZONE=Mars/Base"); code != 2 || !strings.Contains(errs, "time zone") {
		t.Errorf("a wrong zone: %d %s", code, errs)
	}
	// a variable with the prefix that is no setting is a warning (a typo is likely)
	var out, errb bytes.Buffer
	run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string {
		return append(append([]string{}, base...), "EASCLIENT_DOKS=true", "EASCLIENT_LISTEN=0.0.0.0:0")
	})
	if !strings.Contains(errb.String(), "EASCLIENT_DOKS") {
		t.Errorf("a misspelled variable was not reported: %s", errb.String())
	}
}

func TestDocsUIIsCheckedAtStart(t *testing.T) {
	base := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true", "EASCLIENT_DOCS=true",
		"EASCLIENT_LISTEN=bad-address", "EASCLIENT_AUTH_TOKEN=tok", "EASCLIENT_ALLOW_HTTP=true"}
	runWith := func(extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}
	if code, errs := runWith("EASCLIENT_DOCS_UI=swagger"); code != 2 || !strings.Contains(errs, "docs-ui") {
		t.Errorf("a wrong location: %d %s", code, errs)
	}
	// a good location passes the check: the start goes on to the listen address, which is wrong on purpose
	if code, errs := runWith("EASCLIENT_DOCS_UI=https://mirror.example.com/swagger-ui"); code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("a good location: %d %s", code, errs)
	}
}

func TestAllowFromIsCheckedAtStart(t *testing.T) {
	base := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true",
		"EASCLIENT_LISTEN=bad-address", "EASCLIENT_AUTH_TOKEN=tok", "EASCLIENT_ALLOW_HTTP=true"}
	runWith := func(args []string, extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run(append(args, "serve"), strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}
	if code, errs := runWith(nil, "EASCLIENT_ALLOW_FROM=10.0.0.0/8, garbage"); code != 2 || !strings.Contains(errs, "allow-from") || !strings.Contains(errs, "garbage") {
		t.Errorf("a wrong entry in the environment: %d %s", code, errs)
	}
	if code, errs := runWith([]string{"-allow-from", "10.0.0.300"}); code != 2 || !strings.Contains(errs, "10.0.0.300") {
		t.Errorf("a wrong entry on the command line: %d %s", code, errs)
	}
	// good entries pass the check and the start goes on to the listen address, which is wrong on purpose
	if code, errs := runWith([]string{"-allow-from", "192.168.0.0/16", "-allow-from", "10.1.1.1-10.1.1.9"}, "EASCLIENT_ALLOW_FROM=10.0.0.0/8"); code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("good entries: %d %s", code, errs)
	}
}

func TestTLSOfTheListener(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	// port 0: with TLS the plain listener next to it takes a free port of the loopback interface, and the HTTPS address cannot be resolved
	base := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true",
		"EASCLIENT_LISTEN=bad-address:0", "EASCLIENT_AUTH_TOKEN=tok"}
	runWith := func(extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}

	// a certificate without a key, or the other way round, is not a TLS setting
	if code, errs := runWith("EASCLIENT_TLS_CERT=" + cert); code != 2 || !strings.Contains(errs, "go together") {
		t.Errorf("a certificate without a key: %d %s", code, errs)
	}
	// files that are not there are an error, with the names of the files
	if code, errs := runWith("EASCLIENT_TLS_CERT="+cert, "EASCLIENT_TLS_KEY="+key); code != 2 || !strings.Contains(errs, "tls.crt") {
		t.Errorf("no files: %d %s", code, errs)
	}
	// -tls-generate makes them (and the CA), and the start goes on to the listen address, which is wrong on purpose
	code, errs := runWith("EASCLIENT_TLS_CERT="+cert, "EASCLIENT_TLS_KEY="+key, "EASCLIENT_TLS_GENERATE=true", "EASCLIENT_TLS_NAMES=eas.example.com, 10.1.2.3")
	if code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("generate: %d %s", code, errs)
	}
	for _, f := range []string{cert, key, filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was not made: %v", f, err)
		}
	}
	raw, _ := os.ReadFile(cert)
	// a second start renews it: it is ours (the CA next to it signed it), and the CA stays
	caBefore, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	code, errs = runWith("EASCLIENT_TLS_CERT="+cert, "EASCLIENT_TLS_KEY="+key, "EASCLIENT_TLS_GENERATE=true")
	again, _ := os.ReadFile(cert)
	caAfter, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if code != 1 || !strings.Contains(errs, "bad-address") || string(again) == string(raw) || string(caAfter) != string(caBefore) {
		t.Errorf("the second start: %d %s (renewed %v, same CA %v)", code, errs, string(again) != string(raw), string(caAfter) == string(caBefore))
	}
	// a pair that does not fit is refused before the listener is there
	_ = os.WriteFile(key, []byte("not a key"), 0o600)
	if code, errs := runWith("EASCLIENT_TLS_CERT="+cert, "EASCLIENT_TLS_KEY="+key); code != 2 || !strings.Contains(errs, "TLS certificate") {
		t.Errorf("a wrong key: %d %s", code, errs)
	}
}

func TestHealthListenerNextToTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	tlsEnv := []string{"EASCLIENT_TLS_CERT=" + cert, "EASCLIENT_TLS_KEY=" + key, "EASCLIENT_TLS_GENERATE=true"}
	base := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true",
		"EASCLIENT_LISTEN=bad-address:0", "EASCLIENT_AUTH_TOKEN=tok"}
	runWith := func(extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}

	// without TLS there is one listener: the settings of the other one have no meaning
	for _, v := range []string{"EASCLIENT_LISTEN_HEALTH=127.0.0.1:9000", "EASCLIENT_ALLOW_FROM_HEALTH=10.0.0.0/8"} {
		if code, errs := runWith(v, "EASCLIENT_ALLOW_HTTP=true"); code != 2 || !strings.Contains(errs, "health listener") {
			t.Errorf("%s without TLS: %d %s", v, code, errs)
		}
	}
	// not on loopback: the list of the peers is required
	if code, errs := runWith(append(tlsEnv, "EASCLIENT_LISTEN_HEALTH=0.0.0.0:9000")...); code != 2 || !strings.Contains(errs, "allow_from_health") {
		t.Errorf("open without a list: %d %s", code, errs)
	}
	if code, errs := runWith(append(tlsEnv, "EASCLIENT_LISTEN_HEALTH=0.0.0.0:9000", "EASCLIENT_ALLOW_FROM_HEALTH=10.0.0.0/8, garbage")...); code != 2 || !strings.Contains(errs, "garbage") {
		t.Errorf("a wrong entry: %d %s", code, errs)
	}
	// with the list the start goes on to the HTTPS address, which cannot be resolved on purpose; the health listener was bound before
	if code, errs := runWith(append(tlsEnv, "EASCLIENT_LISTEN_HEALTH=127.0.0.1:0", "EASCLIENT_ALLOW_FROM_HEALTH=10.1.2.0/24")...); code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("a good setting: %d %s", code, errs)
	}
}

// TLS is the default: without a certificate named, without the choice of plain HTTP and off the loopback, the certificate is looked for in the
// default directory and made there. Plain HTTP is a choice, except on loopback; TLS and the choice exclude each other.
func TestTLSIsTheDefault(t *testing.T) {
	base := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true", "EASCLIENT_AUTH_TOKEN=tok"}
	runWith := func(extra ...string) (int, string) {
		var out, errb bytes.Buffer
		env := append(append([]string{}, base...), extra...)
		code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env })
		return code, errb.String()
	}
	defaultDir := certreload.DefaultDir
	t.Cleanup(func() { certreload.DefaultDir = defaultDir })

	// a network address, nothing else: the certificate is made in the default directory (with the CA next to it), and HTTPS is served; the start
	// goes on to the address, which cannot be resolved on purpose
	certreload.DefaultDir = t.TempDir()
	code, errs := runWith("EASCLIENT_LISTEN=bad-address:0")
	if code != 1 || !strings.Contains(errs, "bad-address") || !strings.Contains(errs, "made by the micro CA") {
		t.Errorf("TLS by default: %d %s", code, errs)
	}
	for _, f := range []string{"tls.crt", "tls.key", "ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(certreload.DefaultDir, f)); err != nil {
			t.Errorf("%s was not made in the default directory: %v", f, err)
		}
	}
	// the next start finds it and renews it
	if code, errs := runWith("EASCLIENT_LISTEN=bad-address:0"); code != 1 || !strings.Contains(errs, "made again at every start") {
		t.Errorf("the second start: %d %s", code, errs)
	}

	// a directory that cannot be written and holds nothing: refused, with the ways out
	file := filepath.Join(t.TempDir(), "a-file")
	_ = os.WriteFile(file, nil, 0o600)
	certreload.DefaultDir = filepath.Join(file, "tls") // below a file: it cannot exist
	if code, errs := runWith("EASCLIENT_LISTEN=bad-address:0"); code != 2 || !strings.Contains(errs, "allow_http") || !strings.Contains(errs, "tls.crt") {
		t.Errorf("no certificate and none can be made: %d %s", code, errs)
	}

	// the choice of plain HTTP: no certificate is looked for
	certreload.DefaultDir = filepath.Join(file, "tls")
	if code, errs := runWith("EASCLIENT_LISTEN=bad-address", "EASCLIENT_ALLOW_HTTP=true"); code != 1 || !strings.Contains(errs, "bad-address") {
		t.Errorf("allow_http: %d %s", code, errs)
	}
	// loopback needs no choice, and no certificate (a wrong port: the error is the one of the system)
	if code, errs := runWith("EASCLIENT_LISTEN=127.0.0.1:99999"); code != 1 || strings.Contains(errs, "allow_http") || strings.Contains(errs, "micro CA") {
		t.Errorf("loopback: %d %s", code, errs)
	}
	// TLS and plain HTTP exclude each other
	if code, errs := runWith("EASCLIENT_TLS_CERT=c", "EASCLIENT_TLS_KEY=k", "EASCLIENT_ALLOW_HTTP=true"); code != 2 || !strings.Contains(errs, "exclude") {
		t.Errorf("TLS and allow_http: %d %s", code, errs)
	}
	// -tls-generate where there is no TLS is an error: there is nothing to make
	if code, errs := runWith("EASCLIENT_LISTEN=127.0.0.1:99999", "EASCLIENT_TLS_GENERATE=true"); code != 2 || !strings.Contains(errs, "tls-generate") {
		t.Errorf("tls_generate on loopback: %d %s", code, errs)
	}
}

func TestTheDefaultPortsOfTheClient(t *testing.T) {
	for _, c := range []struct {
		given string
		http  bool
		want  string
	}{{"", false, ":8443"}, {"", true, "127.0.0.1:8080"}, {"0.0.0.0:9000", false, "0.0.0.0:9000"}, {":9000", true, ":9000"}} {
		if got := defaultListen(c.given, c.http); got != c.want {
			t.Errorf("defaultListen(%q, %v) = %q, want %q", c.given, c.http, got, c.want)
		}
	}
	// nothing given: HTTPS on 8443 on all interfaces, which is not loopback, so the token is needed
	var out, errb bytes.Buffer
	env := []string{"EASCLIENT_SERVER=traveler.example.com", "EASCLIENT_EMAIL=alice@example.com", "EASCLIENT_PASSWORD=pw", "EASCLIENT_NO_STATE=true"}
	if code := run([]string{"serve"}, strings.NewReader(""), &out, &errb, func() []string { return env }); code != 2 || !strings.Contains(errb.String(), ":8443") || !strings.Contains(errb.String(), "token") {
		t.Errorf("the default: %d %s", code, errb.String())
	}
}
