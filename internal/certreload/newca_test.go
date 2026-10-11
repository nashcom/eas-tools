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

// To get a new CA: remove ca.key (or all the files). The next start makes a new CA and a certificate that it signed.
func TestRemovingTheKeyOfTheMicroCAGivesANewCA(t *testing.T) {
	for name, remove := range map[string][]string{
		"only ca.key":                    {"ca.key"},
		"ca.key and the pair":            {"ca.key", "tls.crt", "tls.key"},
		"ca.crt and ca.key":              {"ca.crt", "ca.key"},
		"everything":                     {"ca.crt", "ca.key", "tls.crt", "tls.key"},
		"ca.key and the key of the pair": {"ca.key", "tls.key"},
		"ca.key and the certificate":     {"ca.key", "tls.crt"},
	} {
		dir := t.TempDir()
		o := ensureOpts(dir)
		if _, err := certreload.Ensure(o); err != nil {
			t.Fatal(err)
		}
		oldCA := read(t, filepath.Join(dir, "ca.crt"))
		for _, f := range remove {
			if err := os.Remove(filepath.Join(dir, f)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := certreload.Ensure(o); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if read(t, filepath.Join(dir, "ca.crt")) == oldCA {
			t.Errorf("%s: the CA is the old one", name)
		}
		chain := chainOf(t, o.CertFile)
		if err := chain[0].CheckSignatureFrom(chain[1]); err != nil {
			t.Errorf("%s: the certificate is not signed by the new CA: %v", name, err)
		}
		if _, err := certreload.New(o.CertFile, o.KeyFile, quiet()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
			t.Errorf("%s: the new CA has no key file: %v", name, err)
		}
	}
}

// The start says that there is a new CA, so that nobody finds out from the clients.
func TestANewCAIsSaidInTheLog(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	if err := certreload.Prepare(o, true, quiet()); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "ca.key"))
	var buf bytes.Buffer
	if err := certreload.Prepare(o, true, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "a new CA was made") || !strings.Contains(out, "level=WARN") {
		t.Errorf("no word about the new CA:\n%s", out)
	}
	// a start that keeps the CA does not say it
	buf.Reset()
	if err := certreload.Prepare(o, true, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "a new CA was made") {
		t.Errorf("the CA is the same and the log says that it is new:\n%s", buf.String())
	}
}

// The ca.crt of our CA without a key does not give a new CA when a certificate of someone else is next to it: that is not ours to replace.
func TestACertificateOfSomeoneElseIsKeptEvenWhenOurCAHasNoKey(t *testing.T) {
	other := t.TempDir()
	if _, err := certreload.Generate(ensureOpts(other)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", false)
	caOnly := read(t, filepath.Join(other, "ca.crt"))
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(caOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	certBefore, keyBefore := read(t, filepath.Join(dir, "tls.crt")), read(t, filepath.Join(dir, "tls.key"))
	if out, err := certreload.Ensure(ensureOpts(dir)); err != nil || out != certreload.Kept {
		t.Fatalf("%v %v", out, err)
	}
	if read(t, filepath.Join(dir, "tls.crt")) != certBefore || read(t, filepath.Join(dir, "tls.key")) != keyBefore || read(t, filepath.Join(dir, "ca.crt")) != caOnly {
		t.Error("a certificate of someone else, or the ca.crt next to it, was changed")
	}
}

// A ca.crt that is not ours (cert-manager's, a CA of the administrator) without a key is never replaced.
func TestTheCACertificateOfAnotherCAIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", true)
	_ = os.Remove(filepath.Join(dir, "tls.crt"))
	_ = os.Remove(filepath.Join(dir, "tls.key"))
	ca := read(t, filepath.Join(dir, "ca.crt"))
	if out, err := certreload.Ensure(ensureOpts(dir)); err == nil || out != "" {
		t.Errorf("%q %v", out, err)
	}
	if read(t, filepath.Join(dir, "ca.crt")) != ca {
		t.Error("the CA of someone else was replaced")
	}
}
