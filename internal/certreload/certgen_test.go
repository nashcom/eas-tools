package certreload_test

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

func chainOf(t *testing.T, file string) []*x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, raw = pem.Decode(raw)
		if b == nil {
			break
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestGenerateMakesACAAndACertificateThatAStoreCanLoad(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "sub", "tls.crt"), KeyFile: filepath.Join(dir, "sub", "tls.key"),
		Names: []string{"eas.example.com", "10.1.2.3", "::1"}}
	created, err := certreload.Generate(o)
	if err != nil || !created {
		t.Fatalf("generate = %v, %v", created, err)
	}
	for _, f := range []string{o.CertFile, o.KeyFile, filepath.Join(dir, "sub", "ca.crt"), filepath.Join(dir, "sub", "ca.key")} {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if st.Mode().Perm()&0o077 != 0 && !strings.HasSuffix(f, ".crt") { // a key is for the owner only (Windows does not say)
			if os.PathSeparator == '/' {
				t.Errorf("%s has the mode %v", f, st.Mode().Perm())
			}
		}
	}

	chain := chainOf(t, o.CertFile)
	if len(chain) != 2 || chain[0].IsCA || !chain[1].IsCA {
		t.Fatalf("the certificate file has %d certificates, want the leaf and the CA", len(chain))
	}
	leaf := chain[0]
	if !strings.Contains(strings.Join(leaf.DNSNames, " "), "eas.example.com") || len(leaf.IPAddresses) != 3 { // 10.1.2.3, ::1 and 127.0.0.1 (the loopback names are added)
		t.Errorf("names: dns %v, ips %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if got := leaf.PublicKeyAlgorithm.String(); got != "ECDSA" {
		t.Errorf("the default key is %s, want ECDSA (RSA 4096 takes seconds to make)", got)
	}
	if d := time.Until(leaf.NotAfter); d < 300*24*time.Hour || d > 400*24*time.Hour {
		t.Errorf("the certificate is valid for %v, want about a year", d)
	}
	if d := time.Until(chain[1].NotAfter); d < 3000*24*time.Hour {
		t.Errorf("the CA is valid for %v, want years", d)
	}

	// a client that trusts the CA accepts the certificate for its names
	pool := x509.NewCertPool()
	pool.AddCert(chain[1])
	for _, name := range []string{"eas.example.com", "10.1.2.3"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.com"}); err == nil {
		t.Error("the certificate is valid for a name that it does not have")
	}

	// and the store takes the pair, the chain included
	s, err := certreload.New(o.CertFile, o.KeyFile, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetCertificate(nil); len(c.Certificate) != 2 {
		t.Errorf("the store serves %d certificates, want the leaf and the CA", len(c.Certificate))
	}
}

func TestGenerateDoesNotTouchWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"a.example.com"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(o.CertFile)
	created, err := certreload.Generate(o)
	after, _ := os.ReadFile(o.CertFile)
	if err != nil || created || string(before) != string(after) {
		t.Errorf("a pair that is there: %v, %v, changed %v", created, err, string(before) != string(after))
	}

	// only one of the two: not a state to generate over, the operator has to look
	_ = os.Remove(o.KeyFile)
	if _, err := certreload.Generate(o); err == nil || !strings.Contains(err.Error(), "tls.key") {
		t.Errorf("a certificate without its key: %v", err)
	}
	if after2, _ := os.ReadFile(o.CertFile); string(after2) != string(before) {
		t.Error("a certificate without a key was overwritten")
	}
}

// When the certificate is gone (it expired and was deleted), the next one is signed by the same CA: the clients keep trusting it.
func TestGenerateKeepsTheCA(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"a.example.com"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	ca1 := chainOf(t, o.CertFile)[1].Raw
	_ = os.Remove(o.CertFile)
	_ = os.Remove(o.KeyFile)
	if created, err := certreload.Generate(o); err != nil || !created {
		t.Fatalf("again: %v, %v", created, err)
	}
	if string(chainOf(t, o.CertFile)[1].Raw) != string(ca1) {
		t.Error("another CA was made, so that the clients that trust the first one do not trust the new certificate")
	}
}

func TestGenerateWithRSAAndTheDefaultNames(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Key: "rsa"}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	leaf := chainOf(t, o.CertFile)[0]
	k, ok := leaf.PublicKey.(*rsa.PublicKey)
	if !ok || k.N.BitLen() < 2048 {
		t.Errorf("key %T", leaf.PublicKey)
	}
	// without names the certificate is for this machine: localhost and its addresses of loopback, and the host name
	if !strings.Contains(strings.Join(leaf.DNSNames, " "), "localhost") || len(leaf.IPAddresses) < 2 {
		t.Errorf("default names: dns %v, ips %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if _, err := certreload.Generate(certreload.GenerateOptions{CertFile: filepath.Join(dir, "x.crt"), KeyFile: filepath.Join(dir, "x.key"), Key: "dsa"}); err == nil {
		t.Error("an unknown key type was accepted")
	}
	if _, err := certreload.Generate(certreload.GenerateOptions{CertFile: "", KeyFile: ""}); err == nil {
		t.Error("no files, no error")
	}
}

// The names of the administrator replace the host name and are added to the loopback names, which are always in: the certificate of a
// container is for the name that the clients use, and a check on the machine itself still works.
func TestNamesReplaceTheHostNameAndKeepTheLoopbackNames(t *testing.T) {
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), Names: []string{"eas.example.com", "LOCALHOST", "127.0.0.1"}}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	leaf := chainOf(t, o.CertFile)[0]
	dns := strings.ToLower(strings.Join(leaf.DNSNames, " "))
	if !strings.Contains(dns, "eas.example.com") || strings.Count(dns, "localhost") != 1 {
		t.Errorf("DNS names %v: the name is in, localhost once", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 2 { // 127.0.0.1 given and not doubled, ::1 added
		t.Errorf("IP addresses %v", leaf.IPAddresses)
	}
	if host, _ := os.Hostname(); host != "" && !strings.EqualFold(host, "localhost") && strings.Contains(dns, strings.ToLower(host)) {
		t.Errorf("the host name %q is in the certificate although names were given", host)
	}

	// without names the host name is in, as before
	o2 := certreload.GenerateOptions{CertFile: filepath.Join(dir, "b.crt"), KeyFile: filepath.Join(dir, "b.key")}
	if _, err := certreload.Generate(o2); err != nil {
		t.Fatal(err)
	}
	leaf2 := chainOf(t, o2.CertFile)[0]
	if host, _ := os.Hostname(); host != "" && !strings.EqualFold(host, "localhost") && !strings.Contains(strings.ToLower(strings.Join(leaf2.DNSNames, " ")), strings.ToLower(host)) {
		t.Errorf("the host name %q is not in %v", host, leaf2.DNSNames)
	}
}
