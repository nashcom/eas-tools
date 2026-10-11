package certreload_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

func days(t *testing.T, file string) int {
	t.Helper()
	leaf := chainOf(t, file)[0]
	return int(time.Until(leaf.NotAfter).Hours()/24 + 0.5)
}

// The lifetime of a certificate that the micro CA makes is a setting; 365 days are the default; it is the lifetime of the certificate that is
// made again at every start as well.
func TestTheLifetimeOfTheCertificate(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	if _, err := certreload.Ensure(o); err != nil {
		t.Fatal(err)
	}
	if d := days(t, o.CertFile); d < 364 || d > 366 {
		t.Errorf("the default is %d days, want 365", d)
	}

	o.LeafDays = 30
	if out, err := certreload.Ensure(o); err != nil || out != certreload.Renewed {
		t.Fatalf("%v %v", out, err)
	}
	if d := days(t, o.CertFile); d < 29 || d > 31 {
		t.Errorf("after the start with 30 days: %d days", d)
	}
}

// A certificate never outlives its CA: a client would not accept it.
func TestACertificateNeverOutlivesTheCA(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), LeafDays: 3000, RootDays: 100}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	chain := chainOf(t, o.CertFile)
	if chain[0].NotAfter.After(chain[1].NotAfter) {
		t.Errorf("the certificate ends %v, after its CA at %v", chain[0].NotAfter, chain[1].NotAfter)
	}
}
