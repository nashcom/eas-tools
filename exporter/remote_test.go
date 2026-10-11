package exporter

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// remoteFixture is an https:// server with a document, a token and its CA certificate as a file.
type remoteFixture struct {
	srv   *httptest.Server
	ca    string
	token atomic.Value
	body  atomic.Value
	hits  atomic.Int64
}

func newRemoteFixture(t *testing.T, document string) *remoteFixture {
	t.Helper()
	f := &remoteFixture{}
	f.token.Store("s3cret")
	f.body.Store(document)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.token.Load().(string) {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/redirect-http":
			http.Redirect(w, r, "http://example.invalid/config.json", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", maxRemoteConfig+10)))
		default:
			_, _ = w.Write([]byte(f.body.Load().(string)))
		}
	}))
	t.Cleanup(f.srv.Close)

	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(f.ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *remoteFixture) env(extra ...string) []string {
	return append([]string{"EASEXPORTER_CONFIG_TOKEN=s3cret", "EASEXPORTER_CONFIG_CA_FILE=" + f.ca,
		"EASEXPORTER_CONFIG_CACHE=" + filepath.Join(filepath.Dir(f.ca), "cache", "config-cache.json")}, extra...)
}

const remoteDoc = `{"targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com"}]}`

func TestRemoteFetchWithToken(t *testing.T) {
	f := newRemoteFixture(t, remoteDoc)
	r, err := NewRemote(f.srv.URL+"/config.json", f.env())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.Fetch(context.Background())
	if err != nil || string(raw) != remoteDoc {
		t.Fatalf("fetch: %q, %v", raw, err)
	}
	if 60 != r.CheckSeconds {
		t.Errorf("reload default %d", r.CheckSeconds)
	}

	// a wrong token is refused and never shows up in the message
	f.token.Store("rotated")
	_, err = r.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("wrong token: %v", err)
	}
}

func TestRemoteTokenFileIsReadAtEveryFetch(t *testing.T) {
	f := newRemoteFixture(t, remoteDoc)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRemote(f.srv.URL, []string{"EASEXPORTER_CONFIG_TOKEN_FILE=" + tokenFile, "EASEXPORTER_CONFIG_CA_FILE=" + f.ca})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}

	// rotate the token on both sides
	f.token.Store("next")
	if err := os.WriteFile(tokenFile, []byte("next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err != nil {
		t.Errorf("rotated token file: %v", err)
	}

	if err := os.WriteFile(tokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty token file: %v", err)
	}
}

func TestRemoteOnlyHTTPS(t *testing.T) {
	for _, loc := range []string{"http://example.com/c.json", "ftp://example.com/c.json", "file:///etc/eas-tools/config.json", "https://", "https://user:pw@example.com/c.json"} {
		if _, err := NewRemote(loc, nil); err == nil {
			t.Errorf("%q must be refused", loc)
		}
	}
	if _, err := NewRemote("http://example.com/c.json", nil); err == nil || !strings.Contains(err.Error(), "only https://") {
		t.Errorf("message: %v", err)
	}
	if !IsRemote("https://x") || !IsRemote("http://x") || IsRemote("/etc/eas-tools/config.json") || IsRemote(`C:\config.json`) {
		t.Error("IsRemote")
	}
}

func TestRemoteRefusesRedirectToHTTPAndBigDocuments(t *testing.T) {
	f := newRemoteFixture(t, remoteDoc)
	r, err := NewRemote(f.srv.URL+"/redirect-http", f.env())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "only https://") {
		t.Errorf("redirect to http: %v", err)
	}

	r, _ = NewRemote(f.srv.URL+"/big", f.env())
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("big document: %v", err)
	}
}

func TestRemoteUntrustedCertificateIsRefused(t *testing.T) {
	f := newRemoteFixture(t, remoteDoc)
	// no CA file: the test server's certificate is not trusted
	r, err := NewRemote(f.srv.URL, []string{"EASEXPORTER_CONFIG_TOKEN=s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err == nil {
		t.Error("an untrusted certificate must be refused")
	}
	if 0 != f.hits.Load() {
		t.Errorf("no request must reach the handler: %d", f.hits.Load())
	}
}

func TestRemoteCache(t *testing.T) {
	f := newRemoteFixture(t, remoteDoc)
	r, err := NewRemote(f.srv.URL, f.env())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.LoadCache(); err == nil {
		t.Error("no cache yet")
	}
	if err := r.SaveCache([]byte(remoteDoc)); err != nil {
		t.Fatal(err)
	}
	raw, err := r.LoadCache()
	if err != nil || string(raw) != remoteDoc {
		t.Errorf("cache: %q, %v", raw, err)
	}
	if st, err := os.Stat(r.CachePath); err != nil || (st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' && !onDrvFs(r.CachePath)) {
		t.Errorf("the cache may hold passwords: mode %v, %v", st.Mode(), err)
	}
	// the temporary file must not stay behind
	entries, _ := os.ReadDir(filepath.Dir(r.CachePath))
	if 1 != len(entries) {
		t.Errorf("files next to the cache: %d", len(entries))
	}
}

// onDrvFs reports a file system that cannot keep Unix modes (WSL drives): the mode check is not meaningful there.
func onDrvFs(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().Perm() == 0o777
}

func TestRemoteReloadSetting(t *testing.T) {
	r, err := NewRemote("https://example.com/c.json", []string{"EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS=0"})
	if err != nil || 0 != r.CheckSeconds {
		t.Errorf("reload 0: %+v, %v", r, err)
	}
	if _, err := NewRemote("https://example.com/c.json", []string{"EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS=often"}); err == nil {
		t.Error("a bad number must be refused")
	}
}

// A remote document can be completed locally: the target of the environment gets its password from here, the rest
// comes from the document.
func TestRemoteDocumentCompletedByTheEnvironment(t *testing.T) {
	doc := `{"targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com"}]}`
	if _, err := LoadConfigBytes([]byte(doc), "remote", nil); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("without a password the document is refused: %v", err)
	}
	c, err := LoadConfigBytes([]byte(doc), "remote", []string{"EASEXPORTER_TARGET=lab", "EASEXPORTER_PASSWORD=local-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Targets["lab"].password(); "local-secret" != p || "traveler.example.com" != c.Targets["lab"].Server {
		t.Errorf("target %+v", c.Targets["lab"])
	}
	// the variables of the remote source are not unknown settings
	c, err = LoadConfigBytes([]byte(doc), "remote", []string{"EASEXPORTER_TARGET=lab", "EASEXPORTER_PASSWORD=x",
		"EASEXPORTER_CONFIG_TOKEN=t", "EASEXPORTER_CONFIG_CACHE=/x", "EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS=60"})
	if err != nil || 0 != len(c.Warnings) {
		t.Errorf("warnings %v, %v", c.Warnings, err)
	}
}

// The server's validators save the download: with an ETag the next fetch asks "if none match" and a 304 is not an
// error. A document that was not accepted is fetched again.
func TestRemoteConditionalFetch(t *testing.T) {
	var gets, notModified atomic.Int64
	etag := `"v1"`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(remoteDoc))
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRemote(srv.URL, []string{"EASEXPORTER_CONFIG_CA_FILE=" + ca})
	if err != nil {
		t.Fatal(err)
	}

	if raw, err := r.Fetch(context.Background()); err != nil || string(raw) != remoteDoc {
		t.Fatalf("first fetch: %q, %v", raw, err)
	}
	// not accepted yet (the caller may still find the document invalid): it is fetched again, in full
	if raw, err := r.Fetch(context.Background()); err != nil || string(raw) != remoteDoc {
		t.Fatalf("a document that was not accepted must be fetched again: %q, %v", raw, err)
	}
	r.Accept()
	if _, err := r.Fetch(context.Background()); !errors.Is(err, ErrNotModified) {
		t.Fatalf("after Accept the server may answer 304: %v", err)
	}
	if 1 != notModified.Load() || 3 != gets.Load() {
		t.Errorf("gets %d, 304s %d", gets.Load(), notModified.Load())
	}

	// the document changes on the server: a new ETag, a full answer
	etag = `"v2"`
	if raw, err := r.Fetch(context.Background()); err != nil || string(raw) != remoteDoc {
		t.Errorf("changed document: %q, %v", raw, err)
	}
}

func TestSameConfigIgnoresHowTheDocumentLooks(t *testing.T) {
	a := `{"min_interval_seconds": 61, "targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com", "password": "pw"}]}`
	b := `{
	  "targets": [{"name": "lab","password": "pw", "user": "u@example.com", "server": "traveler.example.com"}],
	  "min_interval_seconds": 61
	}`
	c := `{"min_interval_seconds": 62, "targets": [{"name": "lab","server": "traveler.example.com", "user": "u@example.com", "password": "pw"}]}`

	cfgA, err := LoadConfigBytes([]byte(a), "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfgA, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfgB, _ := LoadConfigBytes([]byte(b), "b", nil)
	cfgC, _ := LoadConfigBytes([]byte(c), "c", nil)
	if !srv.SameConfig(cfgB) {
		t.Error("spaces and the order of keys are no change")
	}
	if srv.SameConfig(cfgC) {
		t.Error("a different value is a change")
	}
}
