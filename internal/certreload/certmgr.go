package certreload

// The helpers to fetch a certificate from a certificate host and to decide whether it replaces the one in use. They depend on the standard
// library only, and not on the Store of this package: the file can be copied to another program that has to renew a certificate. The way to
// get the certificate of a name from a host (a connection with the name as SNI) is fetchChainViaHTTP of certmgr.go of smtproxy
// (Nash!Com, Daniel Nashed, Apache 2.0).

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FetchChain connects to host:port with serverName as SNI and returns the chain that the host presents, the leaf first. The connection is
// not verified: what comes back is checked by the caller (Evaluate), and a certificate is only taken when it fits the key that is in use.
func FetchChain(ctx context.Context, host, port, serverName string, timeout time.Duration) ([]*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, // #nosec G402 - see above
	}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	chain := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return nil, errors.New("no certificates received")
	}
	return chain, nil
}

// AskName is the name that a certificate host is asked for: the name that is given, and when there is none, the host name of this machine
// in lower case. It is never taken from a certificate: the host knows what it has for a name (a certificate with wildcards as well) and
// answers with the best match, so the program asks for its own name.
func AskName(given string) (string, error) {
	if given != "" {
		return given, nil
	}
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "", errors.New("the host name is not known to ask the certificate host for: set the name (tls_update_name)")
	}
	return strings.ToLower(name), nil
}

// SamePublicKey says whether the certificate is for the private key: the public key of the key is the one of the certificate. The key is
// what tls.LoadX509KeyPair returns in PrivateKey (RSA, ECDSA, Ed25519).
func SamePublicKey(key crypto.PrivateKey, c *x509.Certificate) bool {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return false
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	return ok && pub.Equal(c.PublicKey)
}

// VerifyChain verifies the chain (the leaf first) for the name against the roots of the system. A chain of a private CA does not verify:
// the caller decides what that means.
func VerifyChain(chain []*x509.Certificate, name string) error {
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{DNSName: name, Intermediates: inter})
	return err
}

// ChainPEM is the chain as PEM, in the order it has: the certificate file of a server, the leaf first.
func ChainPEM(chain []*x509.Certificate) []byte {
	var out []byte
	for _, c := range chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// WriteChainFile writes the chain to the file so that it is there complete or not at all: a program that reads the file never sees half a
// certificate.
func WriteChainFile(path string, chain []*x509.Certificate) error {
	return writeAtomic(path, ChainPEM(chain), 0o644)
}

// Problems of a certificate that is offered. They are what Verdict.Problem says, to tell one problem from another.
const (
	ProblemKey   = "key"   // the certificate is for another key
	ProblemValid = "valid" // not valid now
	ProblemName  = "name"  // not for the name that was asked
)

// Verdict is what Evaluate says about the certificate that is offered.
type Verdict struct {
	// Take is true when the certificate replaces the one in use.
	Take bool
	// Problem is one of the Problem constants when the certificate is not taken because it is wrong, and "" when it is taken or when there
	// is only nothing to take: the same certificate, or one that is not newer.
	Problem string
	// Reason says why, in a sentence that does not name the host.
	Reason string
	// Attrs are key and value pairs for the log: the dates, the serial.
	Attrs []any
}

// Evaluate decides whether the chain that a host offers replaces the certificate in use. The chain is taken only when the certificate is
//
//   - not the one in use,
//   - for the private key in use (the renewal has to keep the key, and a certificate that does not fit can never replace one that works),
//   - valid at now,
//   - for the name, and
//   - newer: it starts later than the one in use.
//
// current is nil when there is no certificate yet, only the key: then the first and the last point do not apply.
func Evaluate(chain []*x509.Certificate, key crypto.PrivateKey, current *x509.Certificate, name string, now time.Time) Verdict {
	leaf := chain[0]
	dates := []any{"not_before", leaf.NotBefore.Format(time.RFC3339), "not_after", leaf.NotAfter.Format(time.RFC3339)}
	switch {
	case current != nil && string(leaf.Raw) == string(current.Raw):
		return Verdict{Reason: "the host has the certificate in use"}
	case !SamePublicKey(key, leaf):
		return Verdict{Problem: ProblemKey, Reason: "the certificate of the host is for another key than the one in use: it is not taken (the renewal has to keep the key)",
			Attrs: []any{"serial", leaf.SerialNumber.Text(16)}}
	case now.Before(leaf.NotBefore) || now.After(leaf.NotAfter):
		return Verdict{Problem: ProblemValid, Reason: "the certificate of the host is not valid now", Attrs: dates}
	case leaf.VerifyHostname(name) != nil:
		return Verdict{Problem: ProblemName, Reason: "the certificate of the host is not for the name that was asked"}
	case current != nil && !leaf.NotBefore.After(current.NotBefore):
		return Verdict{Reason: "the certificate of the host is not newer than the one in use", Attrs: append(dates, "in_use", current.NotBefore.Format(time.RFC3339))}
	}
	return Verdict{Take: true, Reason: "the certificate of the host is newer and for the key in use", Attrs: append(dates, "serial", leaf.SerialNumber.Text(16))}
}

// writeAtomic writes a file so that it is there complete or not at all.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
