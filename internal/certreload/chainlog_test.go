package certreload_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/internal/certreload"
)

func capture(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level}))
}

// The start says what the listener serves, short: the files, and one line for each certificate of the chain with the CN, the names, the key,
// the signature, the SHA-1 fingerprint and the days left. Whatever is empty is left out; the rest is at debug level.
func TestTheStartDumpsTheChainShort(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com", "10.1.2.3"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := certreload.New(o.CertFile, o.KeyFile, capture(&buf, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log("\n" + out)

	var files string
	var certs []string // the lines of certificates: only the leaf
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.Contains(l, "msg=\"TLS certificate loaded\""):
			files = l
		case strings.Contains(l, "msg=\"TLS certificate\""):
			certs = append(certs, l)
		}
	}
	if !strings.Contains(files, "cert=") || !strings.Contains(files, "key=") || !strings.Contains(files, "chain=2") {
		t.Errorf("the line of the files: %s", files)
	}
	if len(certs) != 1 {
		t.Fatalf("%d certificate lines, want the leaf alone:\n%s", len(certs), out)
	}
	leaf := certs[0]
	for _, want := range []string{"cn=eas.example.com", "dns=", "eas.example.com", "ips=", "10.1.2.3", "key=\"ECDSA P-256\"", "sig=ECDSA-SHA256", "sha1=", "days=", "root=\"easprobe micro CA\""} {
		if !strings.Contains(leaf, want) {
			t.Errorf("the leaf line lacks %q: %s", want, leaf)
		}
	}
	// and nothing of the long fields is there at info level
	for _, l := range certs {
		for _, no := range []string{"serial", "sha256", "key_usage", "ext_key_usage", "ski=", "aki=", "not_before", "not_after", "issuer", "subject", "position", "public_key", "days_left"} {
			if strings.Contains(l, no) {
				t.Errorf("%q is too much for a certificate line: %s", no, l)
			}
		}
		if len(l) > 330 { // with the time and the level
			t.Errorf("a certificate line of %d characters: %s", len(l), l)
		}
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a good chain is warned about:\n%s", out)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Error("the log shows a key")
	}

	// at debug level the rest is there
	buf.Reset()
	if _, err := certreload.New(o.CertFile, o.KeyFile, capture(&buf, slog.LevelDebug)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TLS certificate detail", "position=\"leaf (the server certificate)\"", "position=\"root CA (self-signed)\"", "issuer=", "serial=",
		"sha256=", "key_usage=", "ext_key_usage=\"ServerAuth, ClientAuth\"", "not_before=", "not_after=", "aki=", "ski="} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the debug log lacks %q", want)
		}
	}
}

// A certificate file whose links do not fit is warned about at the start: the order is wrong or a CA is missing.
func TestAChainThatDoesNotFitIsWarnedAbout(t *testing.T) {
	dir := t.TempDir()
	a := certreload.GenerateOptions{CertFile: filepath.Join(dir, "a", "tls.crt"), KeyFile: filepath.Join(dir, "a", "tls.key"), Names: []string{"a.example.com"}}
	b := certreload.GenerateOptions{CertFile: filepath.Join(dir, "b", "tls.crt"), KeyFile: filepath.Join(dir, "b", "tls.key"), Names: []string{"b.example.com"}}
	for _, o := range []certreload.GenerateOptions{a, b} {
		if _, err := certreload.Generate(o); err != nil {
			t.Fatal(err)
		}
	}
	// the leaf of a, with the CA of b behind it
	leafA, _ := os.ReadFile(a.CertFile)
	caB, _ := os.ReadFile(filepath.Join(dir, "b", "ca.crt"))
	first := strings.SplitAfter(string(leafA), "-----END CERTIFICATE-----\n")[0]
	if err := os.WriteFile(a.CertFile, []byte(first+string(caB)), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := certreload.New(a.CertFile, a.KeyFile, capture(&buf, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not signed by the next one") {
		t.Errorf("no warning for a chain that does not fit:\n%s", out)
	}
}

// A certificate that ends soon is a warning of its own, short.
func TestACertificateThatEndsSoonIsWarnedAbout(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com"}, LeafDays: 5}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := certreload.New(o.CertFile, o.KeyFile, capture(&buf, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN msg=\"TLS certificate expires soon\" cn=eas.example.com days=4") && !strings.Contains(out, "days=5") {
		t.Errorf("no short warning about the certificate that ends soon:\n%s", out)
	}
}

// The root is named on the line of the leaf: for a certificate of someone else too.
func TestTheLineOfTheLeafNamesTheRoot(t *testing.T) {
	dir := t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", false)
	var buf bytes.Buffer
	if _, err := certreload.New(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), capture(&buf, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "cn=eas.example.com") || !strings.Contains(out, "root=\"Real Corp Issuing CA\"") {
		t.Errorf("the root is not named:\n%s", out)
	}
}

// The days left are the first field of the line of the leaf: it is what matters most.
func TestTheDaysLeftComeFirstOnTheLineOfTheLeaf(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := certreload.New(o.CertFile, o.KeyFile, capture(&buf, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "msg=\"TLS certificate\" days=") {
		t.Errorf("the days are not first:\n%s", out)
	}
}
