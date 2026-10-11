package exporter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// The configuration can be fetched from an https:// location instead of a file: -config https://host/path, or
// EASEXPORTER_CONFIG. How to fetch it is never part of the document itself, it comes from the environment:
//
//	EASEXPORTER_CONFIG_TOKEN_FILE     file with the bearer token (first line), read at every fetch so it can be rotated
//	EASEXPORTER_CONFIG_TOKEN          the token itself
//	EASEXPORTER_CONFIG_TOKEN_NAME     optional name of the header that carries it (default: Authorization: Bearer <token>; for
//	                                  another name just "<name>: <token>", for a server where Authorization is taken)
//	EASEXPORTER_CONFIG_CA_FILE        PEM file with additional trusted CA certificates for this location
//	EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS
//	                                  how often to check for a change (default 60 seconds, 0 = only at start)
//	EASEXPORTER_CONFIG_CACHE          where the last good document is kept (default /data/config-cache.json, or the
//	                                  user cache folder); the exporter starts from it when the location is down
const (
	envConfigTokenFile = "EASEXPORTER_CONFIG_TOKEN_FILE"
	envConfigToken     = "EASEXPORTER_CONFIG_TOKEN"
	envConfigTokenName = "EASEXPORTER_CONFIG_TOKEN_NAME"
	envConfigCAFile    = "EASEXPORTER_CONFIG_CA_FILE"
	envConfigCheck     = "EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS"
	envConfigCache     = "EASEXPORTER_CONFIG_CACHE"

	maxRemoteConfig     = 1 << 20 // 1 MiB: a configuration is small, anything bigger is a mistake
	remoteTimeout       = 20 * time.Second
	defaultCheckSeconds = 60
	maxRedirects        = 3
)

// remoteEnvNames are the variables of the remote source, so that they are not reported as unknown settings.
var remoteEnvNames = []string{envConfigTokenFile, envConfigToken, envConfigTokenName, envConfigCAFile, envConfigCheck, envConfigCache}

// ErrNotModified is returned by Fetch when the server says the document is the one that was fetched before.
var ErrNotModified = errors.New("the configuration did not change (HTTP 304)")

// Remote is a configuration document at an https:// location.
type Remote struct {
	URL          string
	Token        string
	TokenFile    string
	TokenName    string // the header that carries the token, "" = Authorization: Bearer
	CAFile       string
	CheckSeconds int
	CachePath    string

	// The validators of the last document that was fetched: the next request asks the server to answer 304 (no body)
	// when nothing changed. They only save the download; whether to apply a document is decided by its content.
	etag, lastModified               string // sent with the next request: those of the last document that was accepted
	pendingETag, pendingLastModified string // those of the document that was just fetched, until Accept

	client *http.Client
}

// IsRemote reports whether the config location is a URL.
func IsRemote(location string) bool {
	return strings.Contains(location, "://")
}

// NewRemote prepares the remote source from the location and the environment. Only https:// is accepted.
func NewRemote(location string, environ []string) (*Remote, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("config location: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("config location %s: only https:// is supported", u.Redacted())
	}
	if u.Host == "" {
		return nil, fmt.Errorf("config location %s: no host", u.Redacted())
	}
	if u.User != nil {
		return nil, errors.New("config location: user and password in the URL are not supported, use the token")
	}

	env := map[string]string{}
	for _, line := range environ {
		if k, v, ok := strings.Cut(line, "="); ok && v != "" {
			env[k] = v
		}
	}

	r := &Remote{URL: location, Token: env[envConfigToken], TokenFile: env[envConfigTokenFile], CAFile: env[envConfigCAFile], TokenName: env[envConfigTokenName],
		CheckSeconds: defaultCheckSeconds, CachePath: env[envConfigCache]}
	if _, err := tokenHeader(r.TokenName); err != nil {
		return nil, fmt.Errorf("%s: %v", envConfigTokenName, err)
	}
	if v := env[envConfigCheck]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%s=%q: not a number of seconds", envConfigCheck, v)
		}
		r.CheckSeconds = n
	}
	if r.CachePath == "" {
		r.CachePath = defaultCachePath()
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if r.CAFile != "" {
		pem, err := os.ReadFile(r.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envConfigCAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate found in %s", envConfigCAFile, r.CAFile)
		}
	}
	r.client = &http.Client{
		Timeout:   remoteTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s refused: only https:// is supported", req.URL.Redacted())
			}
			return nil
		},
	}
	return r, nil
}

// defaultCachePath is /data/config-cache.json in the container, otherwise the user cache folder; "" when there is none.
func defaultCachePath() string {
	if st, err := os.Stat("/data"); err == nil && st.IsDir() {
		return "/data/config-cache.json"
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "easprobe", "config-cache.json")
	}
	return ""
}

// token returns the bearer token; the file is read at every call.
func (r *Remote) token() (string, error) {
	if r.TokenFile != "" {
		t, err := readFirstLine(r.TokenFile)
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		if t == "" {
			return "", errors.New("token file is empty")
		}
		return t, nil
	}
	return r.Token, nil
}

// Fetch downloads the document. Errors never contain the token.
func (r *Remote) Fetch(ctx context.Context) ([]byte, error) {
	token, err := r.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "eas-blackbox-exporter/"+probe.Version)
	if token != "" {
		setToken(req.Header, r.TokenName, token)
	}
	if r.etag != "" {
		req.Header.Set("If-None-Match", r.etag)
	}
	if r.lastModified != "" {
		req.Header.Set("If-Modified-Since", r.lastModified)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the configuration: %s", redact(err, token))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && (r.etag != "" || r.lastModified != "") {
		return nil, ErrNotModified
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching the configuration: HTTP %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteConfig+1))
	if err != nil {
		return nil, fmt.Errorf("fetching the configuration: %s", redact(err, token))
	}
	if len(raw) > maxRemoteConfig {
		return nil, fmt.Errorf("fetching the configuration: more than %d bytes", maxRemoteConfig)
	}
	r.pendingETag, r.pendingLastModified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return raw, nil
}

// redact makes sure a token cannot end up in a message.
func redact(err error, token string) string {
	s := err.Error()
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return s
}

// SaveCache keeps the last good document, readable only by the owner because it may hold passwords.
func (r *Remote) SaveCache(raw []byte) error {
	if r.CachePath == "" {
		return errors.New("no place for the configuration cache (set " + envConfigCache + ")")
	}
	if err := os.MkdirAll(filepath.Dir(r.CachePath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.CachePath), ".config-cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(name, 0o600)); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, r.CachePath); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// LoadCache returns the last good document.
func (r *Remote) LoadCache() ([]byte, error) {
	if r.CachePath == "" {
		return nil, errors.New("no configuration cache")
	}
	return os.ReadFile(r.CachePath)
}

// Accept tells the Remote that the document returned by the last Fetch was valid (and applied, or the same as the one in
// use): from now on the server may answer "not modified" for it. A document that was refused is not accepted, so that
// the next check fetches it again and a broken document keeps being reported until it is fixed.
func (r *Remote) Accept() {
	r.etag, r.lastModified = r.pendingETag, r.pendingLastModified
}

// Compared says what the next fetch compares with, for the log: the ETag and Last-Modified of the document in use.
func (r *Remote) Compared() string {
	if r.etag == "" && r.lastModified == "" {
		return "the server sent no ETag or Last-Modified, so the document is fetched in full every time"
	}
	var parts []string
	if r.etag != "" {
		parts = append(parts, "ETag "+r.etag)
	}
	if r.lastModified != "" {
		parts = append(parts, "Last-Modified "+r.lastModified)
	}
	return strings.Join(parts, ", ")
}
