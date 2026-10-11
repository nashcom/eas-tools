package certreload_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

func ensureOpts(dir string) certreload.GenerateOptions {
	return certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com"}}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// externalPair writes tls.crt and tls.key of a CA that is not ours, as an administrator or ACME would; with caFile also its ca.crt, as
// cert-manager does (never the key of the CA).
func externalPair(t *testing.T, dir, issuer string, caFile bool) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: issuer}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "eas.example.com"}, DNSNames: []string{"eas.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(0, 3, 0), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caPEM...)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(dir, 0o700))
	must(os.WriteFile(filepath.Join(dir, "tls.crt"), chain, 0o644))
	must(os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	if caFile {
		must(os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o644))
	}
}

func TestTheDefaultPlaceIsTheOneOfKubernetesSecrets(t *testing.T) {
	cert, key := certreload.DefaultFiles()
	if filepath.ToSlash(cert) != "/run/secrets/tls/tls.crt" || filepath.ToSlash(key) != "/run/secrets/tls/tls.key" {
		t.Errorf("default files %q %q", cert, key)
	}
}

// What the micro CA made is made again at every start, the certificate and the key, with the same CA.
func TestEnsureCreatesAndThenRenewsBothFilesWithTheSameCA(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	if out, err := certreload.Ensure(o); err != nil || out != certreload.Created {
		t.Fatalf("first start: %v %v", out, err)
	}
	for _, f := range []string{"tls.crt", "tls.key", "ca.crt", "ca.key"} { // the CA is kept in the same directory
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	first := chainOf(t, o.CertFile)
	certBefore, keyBefore, caBefore := read(t, o.CertFile), read(t, o.KeyFile), read(t, filepath.Join(dir, "ca.crt"))

	o2 := o
	o2.Names = nil // the names are those of the certificate that is there
	if out, err := certreload.Ensure(o2); err != nil || out != certreload.Renewed {
		t.Fatalf("second start: %v %v", out, err)
	}
	if read(t, o.CertFile) == certBefore || read(t, o.KeyFile) == keyBefore {
		t.Error("the certificate and the key are both made again, and one of them was not")
	}
	if read(t, filepath.Join(dir, "ca.crt")) != caBefore {
		t.Error("another CA was made: a client that trusts ca.crt would not trust the renewed certificate")
	}
	second := chainOf(t, o.CertFile)
	if first[0].SerialNumber.Cmp(second[0].SerialNumber) == 0 {
		t.Error("the serial is the same")
	}
	dns := strings.Join(second[0].DNSNames, " ")
	if !strings.Contains(dns, "eas.example.com") || !strings.Contains(dns, "localhost") || len(second[0].IPAddresses) < 2 {
		t.Errorf("the names (loopback names always in): %v %v", second[0].DNSNames, second[0].IPAddresses)
	}
	if err := second[0].CheckSignatureFrom(first[1]); err != nil {
		t.Errorf("the renewed certificate is not signed by the CA of the first: %v", err)
	}
	if _, err := certreload.New(o.CertFile, o.KeyFile, quiet()); err != nil {
		t.Errorf("the renewed pair cannot be loaded: %v", err)
	}
	// no temporary files are left behind
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
}

// A certificate of a CA of the administrator is never changed, whatever is next to it: that is what the micro CA must not break.
func TestEnsureNeverTouchesACertificateOfAnotherCA(t *testing.T) {
	setups := map[string]func(dir string){
		"no CA files":                           func(dir string) { externalPair(t, dir, "Real Corp Issuing CA", false) },
		"ca.crt only, as cert-manager makes it": func(dir string) { externalPair(t, dir, "Real Corp Issuing CA", true) },
		"our CA files next to it": func(dir string) {
			externalPair(t, dir, "Real Corp Issuing CA", false)
			other := t.TempDir()
			if _, err := certreload.Generate(ensureOpts(other)); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"ca.crt", "ca.key"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(read(t, filepath.Join(other, f))), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		},
	}
	for name, setup := range setups {
		dir := t.TempDir()
		setup(dir)
		certBefore, keyBefore := read(t, filepath.Join(dir, "tls.crt")), read(t, filepath.Join(dir, "tls.key"))
		if out, err := certreload.Ensure(ensureOpts(dir)); err != nil || out != certreload.Kept {
			t.Errorf("%s: %v %v", name, out, err)
		}
		if read(t, filepath.Join(dir, "tls.crt")) != certBefore || read(t, filepath.Join(dir, "tls.key")) != keyBefore {
			t.Errorf("%s: a certificate of another CA was changed", name)
		}
	}
}

// Only one of the two files, in a directory that is ours (or with a certificate that is ours): both are made again.
func TestEnsureRepairsADirectoryWhereOneFileIsMissing(t *testing.T) {
	for _, missing := range []string{"tls.crt", "tls.key"} {
		dir := t.TempDir()
		o := ensureOpts(dir)
		if _, err := certreload.Ensure(o); err != nil {
			t.Fatal(err)
		}
		caBefore := read(t, filepath.Join(dir, "ca.crt"))
		if err := os.Remove(filepath.Join(dir, missing)); err != nil {
			t.Fatal(err)
		}
		if out, err := certreload.Ensure(o); err != nil || out != certreload.Recreated {
			t.Fatalf("%s missing: %v %v", missing, out, err)
		}
		if _, err := certreload.New(o.CertFile, o.KeyFile, quiet()); err != nil {
			t.Errorf("%s missing: the pair cannot be loaded: %v", missing, err)
		}
		if read(t, filepath.Join(dir, "ca.crt")) != caBefore {
			t.Errorf("%s missing: another CA was made", missing)
		}
	}
}

// Only one file of an administrator, or only a key with nothing that says that it is ours: an error, nothing is made.
func TestEnsureDoesNotMakeAnythingNextToAFileOfSomeoneElse(t *testing.T) {
	// a key without a certificate and without a CA of ours
	dir := t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", false)
	_ = os.Remove(filepath.Join(dir, "tls.crt"))
	keyBefore := read(t, filepath.Join(dir, "tls.key"))
	if out, err := certreload.Ensure(ensureOpts(dir)); err == nil || out != "" {
		t.Errorf("a key alone: %q %v", out, err)
	}
	if read(t, filepath.Join(dir, "tls.key")) != keyBefore {
		t.Error("a key of someone else was replaced")
	}
	// a certificate of a real CA without its key
	dir = t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", false)
	_ = os.Remove(filepath.Join(dir, "tls.key"))
	if out, err := certreload.Ensure(ensureOpts(dir)); err == nil || out != "" {
		t.Errorf("a certificate of a real CA without its key: %q %v", out, err)
	}
	// a CA that is not ours in the directory, with nothing else: nothing is made next to it, and it is not used to sign
	dir = t.TempDir()
	externalPair(t, dir, "Real Corp Issuing CA", true)
	_ = os.Remove(filepath.Join(dir, "tls.crt"))
	_ = os.Remove(filepath.Join(dir, "tls.key"))
	if out, err := certreload.Ensure(ensureOpts(dir)); err == nil || out != "" || !strings.Contains(err.Error(), "not the micro CA") {
		t.Errorf("a CA that is not ours: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls.crt")); err == nil {
		t.Error("a certificate was made next to a CA that is not ours")
	}
}

// A certificate that an earlier micro CA issued (the CA is gone, or was made again) is ours too: it is made again, with the CA that is there.
func TestEnsureRenewsACertificateOfAnEarlierMicroCA(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	if _, err := certreload.Ensure(o); err != nil {
		t.Fatal(err)
	}
	oldCA := read(t, filepath.Join(dir, "ca.crt"))
	_ = os.Remove(filepath.Join(dir, "ca.crt"))
	_ = os.Remove(filepath.Join(dir, "ca.key"))
	if out, err := certreload.Ensure(o); err != nil || out != certreload.Renewed {
		t.Fatalf("the CA is gone: %v %v", out, err)
	}
	if read(t, filepath.Join(dir, "ca.crt")) == oldCA {
		t.Error("the CA was not made again")
	}
	chain := chainOf(t, o.CertFile)
	if err := chain[0].CheckSignatureFrom(chain[1]); err != nil {
		t.Errorf("the pair is not signed by the CA that is in the file: %v", err)
	}
}

// The exporter and the client of a container may share the directory: two that start at the same moment make one CA, and both get a
// certificate that it signed.
func TestTwoProgramsSharingTheDirectoryMakeOneCA(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() { _, err := certreload.Ensure(o); results <- err }()
	}
	for i := 0; i < 6; i++ {
		if err := <-results; err != nil {
			t.Errorf("a start failed: %v", err)
		}
	}
	chain := chainOf(t, o.CertFile)
	if len(chain) != 2 {
		t.Fatalf("chain %d", len(chain))
	}
	if err := chain[0].CheckSignatureFrom(chain[1]); err != nil {
		t.Errorf("the certificate is not signed by the CA that is in the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".easprobe-tls.lock")); err == nil {
		t.Error("the lock was left behind")
	}
}

// A change of the names takes effect at the next start, the loopback names stay.
func TestANewNameIsInTheCertificateAfterTheNextStart(t *testing.T) {
	dir := t.TempDir()
	o := ensureOpts(dir)
	if _, err := certreload.Ensure(o); err != nil {
		t.Fatal(err)
	}
	o.Names = []string{"new.example.com"}
	if out, err := certreload.Ensure(o); err != nil || out != certreload.Renewed {
		t.Fatalf("%v %v", out, err)
	}
	dns := strings.Join(chainOf(t, o.CertFile)[0].DNSNames, " ")
	if !strings.Contains(dns, "new.example.com") || strings.Contains(dns, "eas.example.com") || !strings.Contains(dns, "localhost") {
		t.Errorf("DNS names after the change: %v", dns)
	}
}
