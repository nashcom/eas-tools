package certreload

// The generation of a small CA and of a server certificate follows certmgr.go of smtproxy (Nash!Com, Daniel Nashed, Apache 2.0). There
// it ends the program on an error and writes through a global logger; here it returns the error. The default key is ECDSA P-256: the
// RSA 4096 key that smtproxy makes without a curve name takes seconds to generate.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GenerateOptions say what Generate makes, and where.
type GenerateOptions struct {
	// CertFile and KeyFile are the certificate (the leaf and the CA after it, as a server sends it) and its key.
	CertFile, KeyFile string
	// RootCertFile and RootKeyFile are the CA; by default ca.crt and ca.key next to CertFile. Keep them: the next certificate is signed by
	// the same CA, so that a client that trusts it once keeps trusting.
	RootCertFile, RootKeyFile string
	// Names are the names of the certificate: DNS names and IP addresses. Default: localhost, 127.0.0.1, ::1 and the name of the host.
	Names []string
	// CommonName defaults to the first name.
	CommonName string
	// Key is "ecdsa" (the default, P-256) or "rsa" (3072 bit).
	Key string
	// LeafDays (default 365) and RootDays (default 3650) are the lifetimes.
	LeafDays, RootDays int
}

// Generate makes a certificate and its key when there are none: a small CA (or the one that is there already) signs a server
// certificate. It returns true when it made one. Files that are there are never touched; when only one of certificate and key is there it
// is an error, because that is a state that someone has to look at.
func Generate(o GenerateOptions) (created bool, err error) {
	if o.CertFile == "" || o.KeyFile == "" {
		return false, errors.New("a certificate file and a key file are needed")
	}
	if o.Key == "" {
		o.Key = "ecdsa"
	}
	if o.Key != "ecdsa" && o.Key != "rsa" {
		return false, fmt.Errorf("key type %q: use ecdsa or rsa", o.Key)
	}
	dir := filepath.Dir(o.CertFile)
	if o.RootCertFile == "" {
		o.RootCertFile = filepath.Join(dir, "ca.crt")
	}
	if o.RootKeyFile == "" {
		o.RootKeyFile = filepath.Join(dir, "ca.key")
	}
	if o.LeafDays <= 0 {
		o.LeafDays = DefaultLeafDays
	}
	if o.RootDays <= 0 {
		o.RootDays = 3650
	}

	hasCert, hasKey := exists(o.CertFile), exists(o.KeyFile)
	switch {
	case hasCert && hasKey:
		return false, nil
	case hasCert != hasKey:
		return false, fmt.Errorf("TLS certificate %s and key %s: only one of them is there; not generating over it", o.CertFile, o.KeyFile)
	}
	if err := write(o); err != nil {
		return false, err
	}
	return true, nil
}

// write makes the files: the CA (or the one that is there) signs a new server certificate. The files of the pair are replaced atomically.
func write(o GenerateOptions) error {
	for _, f := range []string{o.CertFile, o.KeyFile, o.RootCertFile, o.RootKeyFile} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			return err
		}
	}

	root, rootKey, rootPEM, err := loadOrMakeRoot(o)
	if err != nil {
		return err
	}
	leafKey, err := newKey(o.Key)
	if err != nil {
		return err
	}
	names := certificateNames(o.Names)
	leafPEM, err := makeLeaf(o, names, leafKey, root, rootKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	// both files are written to temporary files first and moved into place together: a failure while writing leaves the old pair as it was,
	// not a new key with an old certificate
	tmpKey, err := stage(o.KeyFile, keyPEM, 0o600)
	if err != nil {
		return err
	}
	tmpCert, err := stage(o.CertFile, append(append([]byte(nil), leafPEM...), rootPEM...), 0o644)
	if err != nil {
		_ = os.Remove(tmpKey)
		return err
	}
	if err := os.Rename(tmpKey, o.KeyFile); err != nil {
		_ = os.Remove(tmpKey)
		_ = os.Remove(tmpCert)
		return err
	}
	if err := os.Rename(tmpCert, o.CertFile); err != nil {
		_ = os.Remove(tmpCert)
		return err
	}
	return nil
}

// stage writes the data to a temporary file next to the path and returns its name.
func stage(path string, data []byte, perm os.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func exists(f string) bool {
	_, err := os.Stat(f)
	return err == nil
}

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial number: %w", err)
	}
	return n, nil
}

func newKey(kind string) (crypto.Signer, error) {
	if kind == "rsa" {
		k, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			return nil, fmt.Errorf("RSA key: %w", err)
		}
		return k, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ECDSA key: %w", err)
	}
	return k, nil
}

// readPrivateKey reads a private key in PEM of any of the usual forms: PKCS#8, PKCS#1 (RSA) and SEC 1 (EC).
func readPrivateKey(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if s, ok := key.(crypto.Signer); ok {
			return s, nil
		}
		return nil, errors.New("unsupported private key type")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("unsupported private key format %s", block.Type)
}

// loadOrMakeRoot reads the CA when its files are there, and makes it otherwise.
func loadOrMakeRoot(o GenerateOptions) (*x509.Certificate, crypto.Signer, []byte, error) {
	hasCert, hasKey := exists(o.RootCertFile), exists(o.RootKeyFile)
	if hasCert != hasKey {
		return nil, nil, nil, fmt.Errorf("CA certificate %s and key %s: only one of them is there; not generating over it", o.RootCertFile, o.RootKeyFile)
	}
	if hasCert {
		certPEM, err := os.ReadFile(o.RootCertFile)
		if err != nil {
			return nil, nil, nil, err
		}
		keyPEM, err := os.ReadFile(o.RootKeyFile)
		if err != nil {
			return nil, nil, nil, err
		}
		block, _ := pem.Decode(certPEM)
		if block == nil {
			return nil, nil, nil, fmt.Errorf("CA certificate %s: no PEM block", o.RootCertFile)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return nil, nil, nil, fmt.Errorf("CA certificate %s: not a CA certificate (%v)", o.RootCertFile, err)
		}
		key, err := readPrivateKey(keyPEM)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("CA key %s: %w", o.RootKeyFile, err)
		}
		return cert, key, certPEM, nil
	}

	key, err := newKey(o.Key)
	if err != nil {
		return nil, nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: MicroCAName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, o.RootDays),
		KeyUsage: x509.KeyUsageCRLSign | x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:     true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writeAtomic(o.RootKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, nil, err
	}
	if err := writeAtomic(o.RootCertFile, certPEM, 0o644); err != nil {
		return nil, nil, nil, err
	}
	return cert, key, certPEM, nil
}

// makeLeaf signs the server certificate with the CA.
func makeLeaf(o GenerateOptions, names []string, key crypto.Signer, root *x509.Certificate, rootKey crypto.Signer) ([]byte, error) {
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	var dns []string
	var ips []net.IP
	for _, n := range names {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, n)
		}
	}
	cn := o.CommonName
	if cn == "" {
		if len(dns) > 0 {
			cn = dns[0]
		} else if len(ips) > 0 {
			cn = ips[0].String()
		}
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: cn},
		NotBefore: now.Add(-time.Minute), NotAfter: leafEnd(now, o.LeafDays, root),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true,
		DNSNames: dns, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, root, key.Public(), rootKey)
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// certificateNames are the names of the generated certificate. The loopback names (localhost, 127.0.0.1, ::1) are always in, so that a
// check on the machine itself works. Without names of the administrator the host name is added; with names it is not, they replace it: in
// a container the host name is an id that nobody uses, and the name that the clients use is what has to be in the certificate.
func certificateNames(given []string) []string {
	loopback := []string{"localhost", "127.0.0.1", "::1"}
	if len(given) == 0 {
		if h, err := os.Hostname(); err == nil && h != "" && !strings.EqualFold(h, "localhost") {
			return append(loopback, h)
		}
		return loopback
	}
	out := append([]string(nil), given...)
	for _, l := range loopback {
		present := false
		for _, g := range out {
			g = strings.TrimSpace(g)
			if strings.EqualFold(g, l) || (net.ParseIP(g) != nil && net.ParseIP(g).Equal(net.ParseIP(l))) {
				present = true
			}
		}
		if !present {
			out = append(out, l)
		}
	}
	return out
}

// The lifetime of a certificate that the micro CA makes: 365 days by default, from 1 to 3650. The CA lives ten years.
const (
	DefaultLeafDays = 365
	MaxLeafDays     = 3650
)

// leafEnd is the end of a certificate that is valid for days: never later than the end of its CA, which a client would not accept.
func leafEnd(now time.Time, days int, ca *x509.Certificate) time.Time {
	end := now.AddDate(0, 0, days)
	if ca != nil && end.After(ca.NotAfter) {
		return ca.NotAfter
	}
	return end
}
