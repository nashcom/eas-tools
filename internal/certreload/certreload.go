// Package certreload serves a TLS certificate that can be replaced while the listener runs.
//
// The pattern comes from smtproxy (Nash!Com, Daniel Nashed, Apache 2.0): the certificate is held in an atomic value that GetCertificate
// reads for every handshake, a reload loads the files and swaps the value, and a failed reload leaves the pair in use as it is. A reload is
// started by a signal (SIGHUP), by a request, or by Watch, which looks at the files and loads them when they changed, after a short pause
// so that the tool that writes the certificate and the key can finish.
package certreload

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Store holds the certificate of a listener.
type Store struct {
	certFile, keyFile string
	log               *slog.Logger

	// Settle is how long Watch waits after it saw a change before it loads (default one second).
	Settle time.Duration

	cur atomic.Pointer[loaded]

	mu                sync.Mutex // one reload at a time, and the two fingerprints
	seen, tried       fingerprint
	reloads, failures atomic.Int64
	checking          atomic.Bool // a Check is running

	upd           *UpdateOptions // ask a certificate host for a renewed certificate (SetUpdate)
	lastUpdate    time.Time      // the last time the host was asked
	updateProblem string         // what went wrong the last time, to log a problem once
	updates       atomic.Int64   // certificates fetched from the host
}

type loaded struct {
	cert *tls.Certificate
	leaf *x509.Certificate
	at   time.Time
}

// fingerprint says whether the files changed: the SHA-256 of both. Reading follows links, so a link that is swapped for another
// directory (what Docker and Kubernetes do with secrets) is a change. The content decides, not time and size: two pairs that are written in
// the same instant have the same time and almost the same size, and a file that is replaced within one tick of the clock must be seen too.
// The files are a few kilobytes, so reading them in every round costs nothing.
type fingerprint struct{ cert, key [sha256.Size]byte }

// New loads the pair. A pair that cannot be loaded is an error: the start fails loudly, before the listener is there. An expired
// certificate is not: it is logged as a warning, so that the operator sees it and the service still starts.
func New(certFile, keyFile string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Store{certFile: certFile, keyFile: keyFile, log: log, Settle: time.Second}
	l, err := s.load(true)
	if err != nil {
		return nil, err
	}
	s.cur.Store(l)
	s.seen, _ = s.stat()
	s.logLoaded("TLS certificate loaded", l)
	return s, nil
}

// load reads and checks the pair.
func (s *Store) load(initial bool) (*loaded, error) {
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("TLS certificate %s and key %s: %w", s.certFile, s.keyFile, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("TLS certificate %s: %w", s.certFile, err)
	}
	cert.Leaf = leaf
	now := time.Now()
	if now.After(leaf.NotAfter) {
		if !initial {
			return nil, fmt.Errorf("TLS certificate %s: the new certificate is expired (not after %s); the one in use stays", s.certFile, leaf.NotAfter.Format(time.RFC3339))
		}
		s.log.Warn("the TLS certificate is expired", "subject", leaf.Subject.String(), "not_after", leaf.NotAfter.Format(time.RFC3339))
	}
	return &loaded{cert: &cert, leaf: leaf, at: now}, nil
}

func (s *Store) logLoaded(msg string, l *loaded) {
	s.logChain(msg, l)
}

func (s *Store) stat() (fingerprint, bool) {
	c, err1 := os.ReadFile(s.certFile)
	k, err2 := os.ReadFile(s.keyFile)
	if err1 != nil || err2 != nil {
		return fingerprint{}, false
	}
	return fingerprint{sha256.Sum256(c), sha256.Sum256(k)}, true
}

// GetCertificate is for tls.Config: the pair in use at the time of the handshake.
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cur.Load().cert, nil
}

// TLSConfig is the configuration of a listener that serves the pair of the store, TLS 1.2 and up. A reload needs no new listener.
func (s *Store) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.GetCertificate, NextProtos: []string{"h2", "http/1.1"}}
}

// Reload loads the files again. It returns true when the certificate is another one now. When the files cannot be loaded (a key that
// does not fit, a file that is missing, an expired certificate) the pair in use stays, the failure is counted and logged, and the
// error is returned.
func (s *Store) Reload() (changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fp, _ := s.stat()
	l, err := s.load(false)
	if err != nil {
		s.failures.Add(1)
		s.tried = fp
		s.log.Warn("TLS certificate reload failed: the certificate in use stays", "error", err.Error())
		return false, err
	}
	s.seen, s.tried = fp, fingerprint{}
	if old := s.cur.Load(); old != nil && string(old.leaf.Raw) == string(l.leaf.Raw) {
		return false, nil
	}
	s.cur.Store(l)
	s.reloads.Add(1)
	s.logLoaded("TLS certificate reloaded", l)
	return true, nil
}

// Check looks at the files and loads them when they changed since the last look, after Settle. It returns true when the certificate is
// another one now. A pair that could not be loaded is not tried again until the files change once more, so a check that is called often
// does not complain every time. A check that is already running makes this one return at once.
func (s *Store) Check(ctx context.Context) bool {
	if s == nil { // a listener without TLS has no store
		return false
	}
	if !s.checking.CompareAndSwap(false, true) {
		return false
	}
	defer s.checking.Store(false)

	s.update(ctx) // a renewed certificate of the host is written to the file first, the look at the files below then loads it

	s.mu.Lock()
	fp, ok := s.stat()
	skip := !ok || fp == s.seen || fp == s.tried
	s.mu.Unlock()
	if skip {
		return false
	}
	s.log.Debug("the TLS certificate files changed")
	select { // the tool that renews the certificate writes it and the key one after the other
	case <-ctx.Done():
		return false
	case <-time.After(s.Settle):
	}
	changed, _ := s.Reload() // the failure is counted and logged there
	return changed
}

// Watch calls Check every interval (default one minute) until ctx ends.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.mu.Lock()
	asks := s.upd != nil
	s.mu.Unlock()
	if asks { // the certificate host is asked at the start, not after the first interval
		s.Check(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Check(ctx)
		}
	}
}
