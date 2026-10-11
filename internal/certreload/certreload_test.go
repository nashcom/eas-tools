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
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// pair makes a certificate and its key, as PEM.
func pair(t *testing.T, name string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte, serial *big.Int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), serial
}

func valid(t *testing.T, name string) (certPEM, keyPEM []byte, serial *big.Int) {
	return pair(t, name, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
}

type files struct{ dir, cert, key string }

func write(t *testing.T, f files, certPEM, keyPEM []byte) {
	t.Helper()
	if err := os.WriteFile(f.cert, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.key, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, name string) (files, *big.Int) {
	t.Helper()
	dir := t.TempDir()
	f := files{dir, filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")}
	c, k, serial := valid(t, name)
	write(t, f, c, k)
	return f, serial
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serialOf(t *testing.T, s *certreload.Store) *big.Int {
	t.Helper()
	c, err := s.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber
}

func TestLoadsAPairAndDescribesIt(t *testing.T) {
	f, serial := setup(t, "eas.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if got := serialOf(t, s); got.Cmp(serial) != 0 {
		t.Errorf("serial %v, want %v", got, serial)
	}
	in := s.Info()
	if in.Subject != "CN=eas.example.com" || !strings.Contains(strings.Join(in.DNSNames, " "), "eas.example.com") || in.KeyType != "ECDSA P-256" ||
		len(strings.Split(in.SHA256, ":")) != 32 || in.NotAfter.Before(time.Now().Add(80*24*time.Hour)) || in.Reloads != 0 || in.Failures != 0 {
		t.Errorf("info = %+v", in)
	}
	cfg := s.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 || cfg.GetCertificate == nil || len(cfg.Certificates) != 0 {
		t.Errorf("config = %+v", cfg)
	}
}

func TestStartFailsLoudlyOnABadPair(t *testing.T) {
	f, _ := setup(t, "a.example.com")
	_, otherKey, _ := valid(t, "b.example.com")
	for name, fn := range map[string]func(){
		"missing certificate": func() { _ = os.Remove(f.cert) },
		"missing key":         func() { _ = os.Remove(f.key) },
		"key of another cert": func() { _ = os.WriteFile(f.key, otherKey, 0o600) },
		"garbage":             func() { _ = os.WriteFile(f.cert, []byte("not a certificate"), 0o600) },
	} {
		g, _ := setup(t, "a.example.com")
		f = g
		fn()
		_, err := certreload.New(f.cert, f.key, quiet())
		if err == nil || !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "key") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// an expired certificate at the start is a warning, not a refusal: the operator sees it and the service starts
	dir := t.TempDir()
	ef := files{dir, filepath.Join(dir, "c"), filepath.Join(dir, "k")}
	c, k, _ := pair(t, "old.example.com", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	write(t, ef, c, k)
	if _, err := certreload.New(ef.cert, ef.key, quiet()); err != nil {
		t.Errorf("an expired certificate at the start: %v", err)
	}
}

func TestReloadPicksUpANewPairAndKeepsTheOldOneOnAFailure(t *testing.T) {
	f, first := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}

	c2, k2, second := valid(t, "two.example.com")
	write(t, f, c2, k2)
	changed, err := s.Reload()
	if err != nil || !changed {
		t.Fatalf("reload = %v, %v", changed, err)
	}
	if serialOf(t, s).Cmp(second) != 0 || s.Info().Reloads != 1 || s.Info().Subject != "CN=two.example.com" {
		t.Errorf("after the reload: serial %v, info %+v", serialOf(t, s), s.Info())
	}

	// the same files again: nothing changes
	if changed, err := s.Reload(); err != nil || changed {
		t.Errorf("the same pair: %v, %v", changed, err)
	}

	// broken: the pair of before stays and the failure is counted
	for name, fn := range map[string]func(){
		"garbage":       func() { _ = os.WriteFile(f.cert, []byte("junk"), 0o600) },
		"key mismatch":  func() { c3, _, _ := valid(t, "three.example.com"); _ = os.WriteFile(f.cert, c3, 0o600) },
		"missing files": func() { _ = os.Remove(f.cert); _ = os.Remove(f.key) },
	} {
		write(t, f, c2, k2)
		fn()
		before := s.Info().Failures
		if _, err := s.Reload(); err == nil {
			t.Errorf("%s: a broken pair was loaded", name)
		}
		if serialOf(t, s).Cmp(second) != 0 || s.Info().Failures != before+1 {
			t.Errorf("%s: the old pair was lost or the failure was not counted (%+v)", name, s.Info())
		}
	}
	_ = first

	// an expired replacement is refused, the pair in use stays
	ec, ek, _ := pair(t, "dead.example.com", time.Now().Add(-48*time.Hour), time.Now().Add(-time.Minute))
	write(t, f, ec, ek)
	if _, err := s.Reload(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired replacement: %v", err)
	}
	if serialOf(t, s).Cmp(second) != 0 {
		t.Error("the expired replacement is in use")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestWatchReloadsWhenTheFilesChange(t *testing.T) {
	f, _ := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Watch(ctx, 15*time.Millisecond)

	time.Sleep(60 * time.Millisecond) // a few rounds without a change: nothing happens
	if s.Info().Reloads != 0 {
		t.Fatalf("a reload without a change: %+v", s.Info())
	}
	c2, k2, second := valid(t, "two.example.com")
	write(t, f, c2, k2)
	waitFor(t, "the new certificate", func() bool { return serialOf(t, s).Cmp(second) == 0 })
	if s.Info().Reloads != 1 {
		t.Errorf("reloads = %d, want 1", s.Info().Reloads)
	}
}

// The tool that renews a certificate writes the certificate first and the key a moment later: for a moment the pair does not fit. The
// old pair stays in use, and the new one is taken as soon as the key is there.
func TestWatchSurvivesACertificateThatIsWrittenBeforeItsKey(t *testing.T) {
	f, first := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Watch(ctx, 15*time.Millisecond)

	c2, k2, second := valid(t, "two.example.com")
	if err := os.WriteFile(f.cert, c2, 0o600); err != nil { // the key is still the old one
		t.Fatal(err)
	}
	waitFor(t, "the failure of the mismatch", func() bool { return s.Info().Failures >= 1 })
	if serialOf(t, s).Cmp(first) != 0 {
		t.Fatal("a certificate without its key was taken")
	}
	failures := s.Info().Failures
	time.Sleep(80 * time.Millisecond)
	if s.Info().Failures != failures {
		t.Errorf("the same failure is tried again and again: %d, then %d", failures, s.Info().Failures)
	}

	if err := os.WriteFile(f.key, k2, 0o600); err != nil { // now the key
		t.Fatal(err)
	}
	waitFor(t, "the new pair", func() bool { return serialOf(t, s).Cmp(second) == 0 })
}

func TestNewConnectionsGetTheNewCertificate(t *testing.T) {
	f, first := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	// a plain TLS listener: httptest puts a certificate of its own into a configuration that has none
	ln, err := tls.Listen("tcp", "127.0.0.1:0", s.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	url := "https://" + ln.Addr().String()

	peer := func() *big.Int {
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}
		resp, err := (&http.Client{Transport: tr}).Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.TLS.PeerCertificates[0].SerialNumber
	}
	if peer().Cmp(first) != 0 {
		t.Fatal("the first certificate is not the one that is served")
	}
	c2, k2, second := valid(t, "two.example.com")
	write(t, f, c2, k2)
	if _, err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if peer().Cmp(second) != 0 {
		t.Error("a new connection still gets the old certificate: the listener is not restarted by a reload")
	}
}

func TestWatchStopsWithItsContext(t *testing.T) {
	f, _ := setup(t, "one.example.com")
	s, _ := certreload.New(f.cert, f.key, quiet())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Watch(ctx, 10*time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch does not stop")
	}
}

// Docker and Kubernetes secrets are links that are swapped for a new directory: the files are the same names with other content.
func TestAChangeBehindALinkIsSeen(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "v1"), filepath.Join(dir, "v2")
	for _, d := range []string{one, two} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c1, k1, _ := valid(t, "one.example.com")
	c2, k2, second := valid(t, "two.example.com")
	_ = os.WriteFile(filepath.Join(one, "tls.crt"), c1, 0o600)
	_ = os.WriteFile(filepath.Join(one, "tls.key"), k1, 0o600)
	_ = os.WriteFile(filepath.Join(two, "tls.crt"), c2, 0o600)
	_ = os.WriteFile(filepath.Join(two, "tls.key"), k2, 0o600)
	link := filepath.Join(dir, "current")
	if err := os.Symlink(one, link); err != nil {
		t.Skipf("no links here: %v", err)
	}
	s, err := certreload.New(filepath.Join(link, "tls.crt"), filepath.Join(link, "tls.key"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Watch(ctx, 15*time.Millisecond)

	tmp := filepath.Join(dir, "current.new")
	_ = os.Symlink(two, tmp)
	if err := os.Rename(tmp, link); err != nil {
		t.Skipf("a link cannot be swapped here: %v", err)
	}
	waitFor(t, "the certificate behind the swapped link", func() bool { return serialOf(t, s).Cmp(second) == 0 })
}

// Check is what Watch does in every round, for a caller that has its own clock: it loads a changed pair and does nothing otherwise.
func TestCheckLoadsOnlyWhatChanged(t *testing.T) {
	f, first := setup(t, "one.example.com")
	s, err := certreload.New(f.cert, f.key, quiet())
	if err != nil {
		t.Fatal(err)
	}
	s.Settle = 5 * time.Millisecond

	for i := 0; i < 3; i++ {
		if s.Check(t.Context()) {
			t.Fatal("a pair that did not change was loaded")
		}
	}
	if s.Info().Reloads != 0 || serialOf(t, s).Cmp(first) != 0 {
		t.Fatalf("nothing changed, but %+v", s.Info())
	}

	c2, k2, second := valid(t, "two.example.com")
	write(t, f, c2, k2)
	if !s.Check(t.Context()) || serialOf(t, s).Cmp(second) != 0 || s.Info().Reloads != 1 {
		t.Fatalf("the new pair was not taken: %+v", s.Info())
	}
	if s.Check(t.Context()) {
		t.Error("the same pair was taken twice")
	}

	// a pair that does not fit is complained about once, not at every check
	_ = os.WriteFile(f.key, []byte("not a key"), 0o600)
	for i := 0; i < 4; i++ {
		if s.Check(t.Context()) {
			t.Fatal("a broken pair was taken")
		}
	}
	if s.Info().Failures != 1 || serialOf(t, s).Cmp(second) != 0 {
		t.Errorf("failures %d, want 1 (and the pair in use unchanged)", s.Info().Failures)
	}
	// and when the files are put right, the next check takes them
	c3, k3, third := valid(t, "three.example.com")
	write(t, f, c3, k3)
	if !s.Check(t.Context()) || serialOf(t, s).Cmp(third) != 0 {
		t.Error("the repaired pair was not taken")
	}
}

func TestCheckWaitsForTheContext(t *testing.T) {
	f, _ := setup(t, "one.example.com")
	s, _ := certreload.New(f.cert, f.key, quiet())
	s.Settle = time.Hour // a change is seen, and the settle time is long
	c2, k2, _ := valid(t, "two.example.com")
	write(t, f, c2, k2)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if s.Check(ctx) {
		t.Error("a certificate was taken although the context ended during the wait")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Check did not stop with its context")
	}
}

func TestCheckOfNoStoreDoesNothing(t *testing.T) {
	var none *certreload.Store
	if none.Check(t.Context()) {
		t.Error("a nil store loaded something")
	}
}
