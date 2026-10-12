package certreload_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// certFor makes a self-signed certificate of a name for a key.
func certFor(t *testing.T, key *ecdsa.PrivateKey, name string, notBefore, notAfter time.Time) (der []byte, serial *big.Int) {
	t.Helper()
	serial, _ = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der, serial
}

func pemOf(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }

// host is a certificate host: it presents one certificate, whatever name it is asked for, and counts the connections.
type host struct {
	addr  string
	asked atomic.Int64
	cert  atomic.Pointer[tls.Certificate]
}

func startHost(t *testing.T, key *ecdsa.PrivateKey, der []byte) *host {
	t.Helper()
	h := &host{}
	h.cert.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		h.asked.Add(1)
		return h.cert.Load(), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return h
}

func (h *host) serve(key *ecdsa.PrivateKey, der []byte) {
	h.cert.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
}

// inUse sets up a store with a certificate of the key that is valid since two hours.
func inUse(t *testing.T, name string) (*certreload.Store, files, *ecdsa.PrivateKey, *big.Int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, serial := certFor(t, key, name, time.Now().Add(-2*time.Hour), time.Now().Add(30*24*time.Hour))
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	dir := t.TempDir()
	f := files{dir, dir + "/tls.crt", dir + "/tls.key"}
	write(t, f, pemOf(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = time.Millisecond
	return s, f, key, serial
}

func TestACertificateOfTheHostThatIsNewerAndForTheKeyIsTaken(t *testing.T) {
	s, f, key, old := inUse(t, "eas.example.com")
	der, serial := certFor(t, key, "eas.example.com", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	h := startHost(t, key, der)
	if err := s.SetUpdate(certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}); err != nil {
		t.Fatal(err)
	}
	if !s.Check(context.Background()) {
		t.Fatal("the check did not load the certificate of the host")
	}
	if got := serialOf(t, s); got.Cmp(serial) != 0 || got.Cmp(old) == 0 {
		t.Errorf("serial %v, want %v", got, serial)
	}
	if raw, _ := os.ReadFile(f.cert); string(raw) != string(pemOf(der)) {
		t.Error("the certificate file is not the certificate of the host")
	}
}

func TestACertificateOfAnotherKeyAnOlderOneAndAnExpiredOneAreNotTaken(t *testing.T) {
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for name, c := range map[string]struct {
		otherKey            bool
		notBefore, notAfter time.Duration
	}{
		"another key": {true, -time.Hour, 90 * 24 * time.Hour},
		"older":       {false, -3 * time.Hour, 90 * 24 * time.Hour},
		"expired":     {false, -time.Hour, -time.Minute},
		"not yet":     {false, time.Hour, 90 * 24 * time.Hour},
	} {
		s, _, key, old := inUse(t, "eas.example.com")
		k := key
		if c.otherKey {
			k = other
		}
		der, _ := certFor(t, k, "eas.example.com", time.Now().Add(c.notBefore), time.Now().Add(c.notAfter))
		h := startHost(t, k, der)
		if err := s.SetUpdate(certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}); err != nil {
			t.Fatal(err)
		}
		if s.Check(context.Background()) || serialOf(t, s).Cmp(old) != 0 {
			t.Errorf("%s: the certificate of the host was taken", name)
		}
		if h.asked.Load() == 0 {
			t.Errorf("%s: the host was not asked", name)
		}
	}
}

func TestACertificateForAnotherNameIsNotTaken(t *testing.T) {
	s, _, key, old := inUse(t, "eas.example.com")
	der, _ := certFor(t, key, "other.example.com", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	h := startHost(t, key, der)
	if err := s.SetUpdate(certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}); err != nil {
		t.Fatal(err)
	}
	if s.Check(context.Background()) || serialOf(t, s).Cmp(old) != 0 {
		t.Error("a certificate for another name was taken")
	}
}

func TestTheHostIsAskedSeldomAndOnRequest(t *testing.T) {
	s, _, key, _ := inUse(t, "eas.example.com")
	der, _ := certFor(t, key, "eas.example.com", time.Now().Add(-3*time.Hour), time.Now().Add(90*24*time.Hour)) // older: nothing to take
	h := startHost(t, key, der)
	if err := s.SetUpdate(certreload.UpdateOptions{From: h.addr, Name: "eas.example.com", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.Check(ctx)
	s.Check(ctx)
	s.Check(ctx)
	if n := h.asked.Load(); n != 1 {
		t.Errorf("the host was asked %d times by three checks, want once: the interval is an hour", n)
	}
	s.CheckNow(ctx)
	if n := h.asked.Load(); n != 2 {
		t.Errorf("the host was asked %d times after a check on request, want 2", n)
	}
	s.Update(ctx)
	if n := h.asked.Load(); n != 3 {
		t.Errorf("the host was asked %d times after an update on request, want 3", n)
	}
}

func TestTheHostIsAskedAtTheStartOfWatch(t *testing.T) {
	s, _, key, _ := inUse(t, "eas.example.com")
	der, serial := certFor(t, key, "eas.example.com", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	h := startHost(t, key, der)
	if err := s.SetUpdate(certreload.UpdateOptions{From: h.addr, Name: "eas.example.com", Every: time.Hour}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx, time.Hour) // the interval is long: only the first look at the start can have loaded it
	for i := 0; i < 200 && serialOf(t, s).Cmp(serial) != 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if serialOf(t, s).Cmp(serial) != 0 {
		t.Error("the certificate of the host was not taken at the start")
	}
}

func TestAHostThatDoesNotAnswerChangesNothing(t *testing.T) {
	s, _, _, old := inUse(t, "eas.example.com")
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	if err := s.SetUpdate(certreload.UpdateOptions{From: addr, Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if s.Check(context.Background()) || serialOf(t, s).Cmp(old) != 0 {
		t.Error("the certificate changed without a host")
	}
}

func TestTheCertificateHostIsHostAndPort(t *testing.T) {
	s, _, _, _ := inUse(t, "eas.example.com")
	for _, bad := range []string{"", "certmgr.example.com", ":443", "certmgr.example.com:"} {
		if err := s.SetUpdate(certreload.UpdateOptions{From: bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestAKeyWithoutACertificateGetsItFromTheHost(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	der, serial := certFor(t, key, "eas.example.com", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	h := startHost(t, key, der)
	dir := t.TempDir()
	f := files{dir, dir + "/tls.crt", dir + "/tls.key"}
	if err := os.WriteFile(f.key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	o := certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}
	if err := certreload.FetchFirst(context.Background(), o, f.cert, f.key, quiet()); err != nil {
		t.Fatal(err)
	}
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil || serialOf(t, s).Cmp(serial) != 0 {
		t.Fatalf("the certificate of the host was not loaded: %v", err)
	}
	// with the certificate there it does nothing, and so it does without a certificate host
	asked := h.asked.Load()
	if err := certreload.FetchFirst(context.Background(), o, f.cert, f.key, quiet()); err != nil || h.asked.Load() != asked {
		t.Errorf("asked again although the certificate is there: %v", err)
	}
}

func TestAKeyWithoutACertificateAndAHostWithTheWrongCertificateIsAnError(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	der, _ := certFor(t, other, "eas.example.com", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	h := startHost(t, other, der)
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/tls.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	err := certreload.FetchFirst(context.Background(), certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}, dir+"/tls.crt", dir+"/tls.key", quiet())
	if err == nil {
		t.Fatal("a certificate for another key was taken")
	}
	if _, serr := os.Stat(dir + "/tls.crt"); serr == nil {
		t.Error("a certificate file was written")
	}
	// no key: nothing to do, the micro CA makes the certificate as always
	if err := certreload.FetchFirst(context.Background(), certreload.UpdateOptions{From: h.addr, Name: "eas.example.com"}, dir+"/x.crt", dir+"/x.key", quiet()); err != nil {
		t.Errorf("without a key: %v", err)
	}
}

func TestTheNameThatIsAskedIsTheGivenOneOrTheHostName(t *testing.T) {
	if n, err := certreload.AskName("eas.example.com"); err != nil || n != "eas.example.com" {
		t.Errorf("given: %q %v", n, err)
	}
	host, _ := os.Hostname()
	if n, err := certreload.AskName(""); err != nil || n != strings.ToLower(host) {
		t.Errorf("default: %q %v, want the host name %q", n, err, strings.ToLower(host))
	}
}
