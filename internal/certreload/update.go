package certreload

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"
)

// DefaultUpdateEvery is how seldom the certificate host is asked: the files are looked at every minute, but a certificate changes every
// few months, and the host is asked once at the start anyhow. Check does not ask more often than this, whatever its own interval is.
const DefaultUpdateEvery = time.Hour

// UpdateOptions make a store ask a host for a renewed certificate. The host is the one that issues and keeps the certificates (the
// Domino CertMgr, for one): it presents the certificate for a name when it is asked with that name as SNI, and that is all this needs.
// The helpers that do the work (FetchChain, Evaluate) are in certmgr.go and do not depend on the store.
type UpdateOptions struct {
	// From is the host and port of the certificate host: "certmgr.example.com:443".
	From string
	// Name is the SNI name that the certificate is asked for. Default: the host name of this machine. It is never taken from a
	// certificate: the host answers with the best match for the name that is asked, wildcards included.
	Name string
	// Every is how seldom the host is asked (default DefaultUpdateEvery, one hour). The first time is at the start.
	Every time.Duration
	// Timeout of one connection (default 10 seconds).
	Timeout time.Duration
}

// SetUpdate makes Check ask the host for a renewed certificate before it looks at the files. It is called before Watch is started.
func (s *Store) SetUpdate(o UpdateOptions) error {
	host, port, err := net.SplitHostPort(o.From)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("the certificate host %q is not host:port", o.From)
	}
	if o.Every <= 0 {
		o.Every = DefaultUpdateEvery
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	s.mu.Lock()
	s.upd = &o
	s.mu.Unlock()
	return nil
}

// FetchFirst is for the start of a program that has a private key and no certificate yet: it asks the certificate host for the certificate
// of the key and writes it to the certificate file, so that the program can load the pair. The name is the one of the options, or the
// host name of this machine (AskName), the same name as the renewal asks for later. It does nothing when there is no certificate host in the options, when the certificate file is there (the
// renewal is the business of the store then) or when there is no key (the certificate of the micro CA is made then, as always). A
// certificate host that does not have a certificate for the key is an error: without a certificate there is nothing to serve.
func FetchFirst(ctx context.Context, o UpdateOptions, certFile, keyFile string, log *slog.Logger) error {
	if o.From == "" || exists(certFile) || !exists(keyFile) {
		return nil
	}
	host, port, err := net.SplitHostPort(o.From)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("the certificate host %q is not host:port", o.From)
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	name, err := AskName(o.Name)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("TLS key %s: %w", keyFile, err)
	}
	key, err := readPrivateKey(raw)
	if err != nil {
		return fmt.Errorf("TLS key %s: %w", keyFile, err)
	}
	chain, err := FetchChain(ctx, host, port, name, o.Timeout)
	if err != nil {
		return fmt.Errorf("there is a TLS key %s and no certificate, and the certificate host %s did not answer for the name %s: %w", keyFile, o.From, name, err)
	}
	if v := Evaluate(chain, key, nil, name, time.Now()); !v.Take {
		return fmt.Errorf("there is a TLS key %s and no certificate, and the certificate host %s has none that fits for the name %s: %s (the name is the host name unless tls_update_name says another)",
			keyFile, o.From, name, v.Reason)
	}
	if err := VerifyChain(chain, name); err != nil && log != nil {
		log.Warn("the new TLS certificate does not verify against the system roots, it is taken because it is for our key", "name", name, "error", err.Error())
	}
	if err := WriteChainFile(certFile, chain); err != nil {
		return fmt.Errorf("the certificate that the host sent cannot be written to %s: %w", certFile, err)
	}
	if log != nil {
		log.Info("TLS certificate fetched: there was a key and no certificate", "from", o.From, "name", name, "not_after", chain[0].NotAfter.Format(time.RFC3339),
			"serial", chain[0].SerialNumber.Text(16), "file", certFile)
	}
	return nil
}

// Update asks the certificate host now, whatever the time since the last time, and writes a renewed certificate to the file. It does not
// load it: Check does (CheckNow does both), and so does Reload. It is for what a person asks for: SIGHUP and the request to check.
func (s *Store) Update(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastUpdate = time.Time{}
	s.mu.Unlock()
	s.update(ctx)
}

// CheckNow is Check for a request: the certificate host is asked now, and what it has is loaded.
func (s *Store) CheckNow(ctx context.Context) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	s.lastUpdate = time.Time{}
	s.mu.Unlock()
	return s.Check(ctx)
}

// update asks the certificate host whether it has a newer certificate for the key in use, and writes it to the certificate file when it
// does. The files are then picked up by the check that follows, like any other change: the key file is never touched. What decides is
// Evaluate: the certificate has to be for the key in use, valid now, for the name, and newer.
func (s *Store) update(ctx context.Context) {
	s.mu.Lock()
	u := s.upd
	if u == nil || (!s.lastUpdate.IsZero() && time.Since(s.lastUpdate) < u.Every) {
		s.mu.Unlock()
		return
	}
	s.lastUpdate = time.Now()
	s.mu.Unlock()

	cur := s.cur.Load()
	if cur.leaf.Issuer.CommonName == MicroCAName {
		s.log.Debug("the certificate is the one of the micro CA: it is made again at every start, there is nothing to update")
		return
	}
	name, err := AskName(u.Name)
	if err != nil {
		s.updateNote("update-name", err.Error())
		return
	}
	host, port, _ := net.SplitHostPort(u.From)
	chain, err := FetchChain(ctx, host, port, name, u.Timeout)
	if err != nil {
		if ctx.Err() == nil {
			s.updateNote("fetch", "the certificate host did not answer", "from", u.From, "name", name, "error", err.Error())
		}
		return
	}

	v := Evaluate(chain, cur.cert.PrivateKey, cur.leaf, name, time.Now())
	attrs := append([]any{"from", u.From, "name", name}, v.Attrs...)
	if !v.Take {
		if v.Problem == "" {
			s.updateNote("", "TLS certificate checked at the certificate host: "+v.Reason+", nothing to update", attrs...)
		} else {
			s.updateNote(v.Problem, v.Reason, attrs...)
		}
		return
	}

	// The key fits, so the certificate works for this server; whether the clients trust it is the business of the CA. A warning is
	// enough: a certificate of a private CA does not verify against the roots of the system.
	if err := VerifyChain(chain, name); err != nil {
		s.log.Warn("the new TLS certificate does not verify against the system roots, it is taken because it is for our key", "name", name, "error", err.Error())
	}
	if err := WriteChainFile(s.certFile, chain); err != nil {
		s.updateNote("write", "the new certificate cannot be written", "file", s.certFile, "error", err.Error())
		return
	}
	s.updates.Add(1)
	s.updateNote("", "")
	s.log.Info("TLS certificate fetched", append(attrs, "file", s.certFile)...)
}

// updateNote says what the update found. A problem (the key of another certificate, a host that does not answer) is a warning the first
// time and when it changes, and at the debug level while it stays the same. What is no problem (nothing newer) is said at the info
// level, so that the log shows that the host was asked. The kind is "" for what is no problem; it is also what ends a problem.
func (s *Store) updateNote(kind, msg string, args ...any) {
	s.mu.Lock()
	was := s.updateProblem
	s.updateProblem = kind
	s.mu.Unlock()
	switch {
	case kind == "":
		if was != "" {
			s.log.Info("the certificate host works again")
		}
		if msg != "" {
			s.log.Info(msg, args...)
		}
	case kind == was:
		s.log.Debug(msg, args...)
	default:
		s.log.Warn(msg, args...)
	}
}
