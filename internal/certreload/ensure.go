package certreload

import (
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultDir is where the certificate is looked for when nothing is specified: the directory that a Kubernetes secret of the type
// kubernetes.io/tls (tls.crt, tls.key, and ca.crt with cert-manager) or a volume of Docker is mounted on. A variable, so that a test can
// use another place.
var DefaultDir = "/run/secrets/tls"

// DefaultFiles are the certificate and the key that are used when nothing is specified.
func DefaultFiles() (cert, key string) {
	return filepath.Join(DefaultDir, "tls.crt"), filepath.Join(DefaultDir, "tls.key")
}

// MicroCAName is the name of the CA that this program makes: what tells a certificate that is ours from one of the administrator.
const MicroCAName = "easprobe micro CA"

// Outcome says what Ensure did.
type Outcome string

const (
	// Created: there was no certificate, a CA and a certificate were made.
	Created Outcome = "created"
	// Renewed: the pair is ours and was made again, both the certificate and the key, with the same CA.
	Renewed Outcome = "renewed"
	// Recreated: only one of the two files was there, in a directory that is ours: both were made again.
	Recreated Outcome = "recreated"
	// Kept: the certificate is not ours (it comes from a CA of the administrator, or from ACME): it is not touched.
	Kept Outcome = "kept"
)

// Ensure is the start of a listener that makes its own certificate when it has none. What is ours is decided by names, never by guessing:
//
//   - the certificate is ours when it was issued by the CA called MicroCAName (this program made it, today or by an earlier start, with
//     this CA or with one that is gone);
//   - the directory is ours when ca.crt and ca.key are there, are a pair, and are the micro CA.
//
// What follows: no files: made (Created). Both files, and the certificate is ours: made again, both files, at every start (Renewed), with
// the same CA and the same names unless the options say others, so that a certificate never runs out while the service is restarted now and
// then. Both files and the certificate is not ours: left as it is (Kept), whatever else is in the directory; a certificate of the
// administrator is never changed. Only one of the two files: made again, both (Recreated) when the directory is ours or the certificate that
// is there is, and an error otherwise, because then it is something that someone has to look at. A CA in the directory that is not the micro
// CA is never used to sign anything.
//
// When the files cannot be written (a read-only directory) a pair that is there is kept and the error is returned together with Kept, so
// that the caller can warn and go on; for a pair that is not there it is the error.
func Ensure(o GenerateOptions) (Outcome, error) {
	if o.CertFile == "" || o.KeyFile == "" {
		return "", fmt.Errorf("a certificate file and a key file are needed")
	}
	if o.RootCertFile == "" {
		o.RootCertFile = filepath.Join(filepath.Dir(o.CertFile), "ca.crt")
	}
	if o.RootKeyFile == "" {
		o.RootKeyFile = filepath.Join(filepath.Dir(o.CertFile), "ca.key")
	}
	defer lockDir(filepath.Dir(o.CertFile))()

	hasCert, hasKey := exists(o.CertFile), exists(o.KeyFile)
	caThere := exists(o.RootCertFile) || exists(o.RootKeyFile)
	caOurs := ourCA(o)
	// the certificate of our CA whose key was removed (to get a new CA) or lost: it is replaced by a new CA at this start
	stale := !caOurs && staleOwnCA(o)
	if caThere && !caOurs && !stale {
		// a CA that is not ours (the ca.crt that cert-manager puts next to a certificate, or a CA of the administrator with its key): it is never
		// used to sign. With a pair of the administrator next to it there is nothing to do; with nothing, we cannot make our own beside it.
		if hasCert && hasKey {
			return Kept, nil
		}
		return "", fmt.Errorf("%s is not the micro CA of this program (%q): not making a certificate next to it", o.RootCertFile, MicroCAName)
	}

	var leaf *x509.Certificate
	if hasCert {
		leaf = firstCertificate(o.CertFile)
	}
	leafOurs := leaf != nil && leaf.Issuer.CommonName == MicroCAName

	var outcome Outcome
	switch {
	case !hasCert && !hasKey:
		outcome = Created
	case hasCert && hasKey && leafOurs:
		outcome = Renewed
	case hasCert && hasKey:
		return Kept, nil // a certificate of the administrator
	case caOurs || stale || leafOurs: // one file is missing, in a directory that is ours
		outcome = Recreated
	default:
		return "", fmt.Errorf("TLS certificate %s and key %s: only one of them is there, and nothing here tells that this program made them: not generating over it", o.CertFile, o.KeyFile)
	}
	if stale { // the CA of the micro CA without its key: a new CA, which the clients have to trust; the files of the old one are removed
		for _, f := range []string{o.RootCertFile, o.RootKeyFile} {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				return Kept, fmt.Errorf("the generated certificate could not be renewed: the old CA could not be removed: %w", err)
			}
		}
	}

	// made again with the names and the kind of key of the pair that is there, unless the options say otherwise
	if leaf != nil {
		if len(o.Names) == 0 {
			for _, n := range leaf.DNSNames {
				o.Names = append(o.Names, n)
			}
			for _, ip := range leaf.IPAddresses {
				o.Names = append(o.Names, ip.String())
			}
		}
		if o.Key == "" {
			if _, ok := leaf.PublicKey.(*rsa.PublicKey); ok {
				o.Key = "rsa"
			}
		}
	}
	if o.Key == "" {
		o.Key = "ecdsa"
	}
	if o.LeafDays <= 0 {
		o.LeafDays = DefaultLeafDays
	}
	if o.RootDays <= 0 {
		o.RootDays = 3650
	}
	if err := os.MkdirAll(filepath.Dir(o.CertFile), 0o700); err != nil && outcome != Created {
		return Kept, fmt.Errorf("the generated certificate could not be renewed: %w", err)
	} else if err != nil {
		return "", err
	}
	if err := write(o); err != nil {
		if outcome == Created {
			return "", err
		}
		return Kept, fmt.Errorf("the generated certificate could not be renewed: %w", err)
	}
	return outcome, nil
}

// ourCA says whether ca.crt and ca.key in the directory are the micro CA: a CA certificate called MicroCAName that signed itself, and the
// key that belongs to it.
func ourCA(o GenerateOptions) bool {
	if !exists(o.RootCertFile) || !exists(o.RootKeyFile) {
		return false
	}
	ca := firstCertificate(o.RootCertFile)
	if ca == nil || !ca.IsCA || ca.Subject.CommonName != MicroCAName || ca.CheckSignatureFrom(ca) != nil {
		return false
	}
	raw, err := os.ReadFile(o.RootKeyFile)
	if err != nil {
		return false
	}
	key, err := readPrivateKey(raw)
	if err != nil {
		return false
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	return ok && pub.Equal(ca.PublicKey)
}

func firstCertificate(file string) *x509.Certificate {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return c
}

// Prepare is what a program does at the start before it loads the pair, when it makes its own certificate (the files are the default ones, or
// the administrator asked for generation): Ensure, and the log of what happened. auto says that the files are the default ones, which only
// changes the message of an error. The generated certificate is for a try: it says so, loudly, and says that the CA has to be trusted.
func Prepare(o GenerateOptions, auto bool, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	caBefore, _ := os.ReadFile(filepath.Join(filepath.Dir(o.CertFile), "ca.crt"))
	out, err := Ensure(o)
	if caAfter, _ := os.ReadFile(filepath.Join(filepath.Dir(o.CertFile), "ca.crt")); err == nil && len(caBefore) > 0 && string(caAfter) != string(caBefore) {
		log.Warn("a new CA was made, because the key of the old one was removed or lost: the clients have to trust the new ca.crt", "ca", filepath.Join(filepath.Dir(o.CertFile), "ca.crt"))
	}
	switch {
	case out == Created:
		log.Warn("no TLS certificate was there: one was made by the micro CA of easprobe, which is for a try; clients have to trust the CA (ca.crt in the same directory), and anything else should be given a certificate of its own",
			"certificate", o.CertFile, "ca", filepath.Join(filepath.Dir(o.CertFile), "ca.crt"))
	case out == Renewed:
		log.Info("the TLS certificate and key were made by the micro CA of easprobe and are made again at every start; the CA stays, so clients that trust it keep trusting",
			"certificate", o.CertFile)
	case out == Recreated:
		log.Warn("only one of the TLS certificate and its key was there, in a directory of the micro CA of easprobe: both were made again",
			"certificate", o.CertFile, "key", o.KeyFile)
	case out == Kept && err != nil:
		log.Warn("the TLS certificate of the micro CA could not be renewed: the one that is there is used", "certificate", o.CertFile, "error", err.Error())
		return nil
	case out == Kept:
		log.Debug("the TLS certificate is not one of the micro CA of easprobe: it is used as it is, and never changed", "certificate", o.CertFile)
	}
	if err != nil && auto {
		return fmt.Errorf("%w; mount a certificate (tls.crt and tls.key) on %s, or set tls_cert and tls_key, or choose plain HTTP with allow_http", err, filepath.Dir(o.CertFile))
	}
	return err
}

// lockDir takes a lock on the directory of the certificate while Ensure works: the exporter and the client of a container may share the
// directory (the same volume or secret), and two programs that start at the same moment must not make two CAs. The lock is a file made
// with O_EXCL, which a program that crashed leaves behind: one that is older than a minute is taken over. When the directory cannot be
// written there is nothing to protect, and no lock.
func lockDir(dir string) func() {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}
	}
	name := filepath.Join(dir, ".easprobe-tls.lock")
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(name) }
		}
		if !os.IsExist(err) {
			return func() {}
		}
		if st, err := os.Stat(name); err == nil && time.Since(st.ModTime()) > time.Minute {
			_ = os.Remove(name)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// staleOwnCA says whether ca.crt is the certificate of the micro CA without a key that fits: the key was removed to get a new CA, or lost.
// Anything else in the place of ca.crt (the ca.crt of cert-manager, a CA of the administrator) is not.
func staleOwnCA(o GenerateOptions) bool {
	if !exists(o.RootCertFile) {
		return false
	}
	ca := firstCertificate(o.RootCertFile)
	return ca != nil && ca.IsCA && ca.Subject.CommonName == MicroCAName && ca.CheckSignatureFrom(ca) == nil
}
