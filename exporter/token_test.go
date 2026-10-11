package exporter

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenHeaderNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "Authorization"}, {"authorization", "Authorization"}, {"x-auth-token", "X-Auth-Token"}, {" X_Config_Token ", "X_config_token"},
	} {
		if got, err := tokenHeader(tc.in); err != nil || tc.want != got {
			t.Errorf("%q: %q, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"X Token", "X:Token", "Cookie", "host", "Content-Type", "Proxy-Authorization", "X-Tok\r\nen", "ü"} {
		if _, err := tokenHeader(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestSetAndGetToken(t *testing.T) {
	h := http.Header{}
	setToken(h, "", "abc")
	if "Bearer abc" != h.Get("Authorization") {
		t.Errorf("default: %v", h)
	}
	r := &http.Request{Header: h}
	if "abc" != getToken(r, "") {
		t.Errorf("get default: %q", getToken(r, ""))
	}
	if "" != getToken(r, "X-Auth-Token") {
		t.Error("another header name does not see Authorization")
	}

	h = http.Header{}
	setToken(h, "x-auth-token", "abc")
	if "abc" != h.Get("X-Auth-Token") || "" != h.Get("Authorization") {
		t.Errorf("named: %v", h)
	}
	r = &http.Request{Header: h}
	if "abc" != getToken(r, "X-Auth-Token") || "" != getToken(r, "") {
		t.Errorf("get named: %q / %q", getToken(r, "X-Auth-Token"), getToken(r, ""))
	}
	// a different scheme in Authorization is not our token
	r = &http.Request{Header: http.Header{"Authorization": {"Basic dXNlcjpwdw=="}}}
	if "" != getToken(r, "") {
		t.Error("Basic is not a Bearer token")
	}
}

func probeStatus(t *testing.T, srv *Server, url string, header ...string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// The token protects /probe: the value, in the default or in a named header, and ?auth=.
func TestProbeTokenValueAndName(t *testing.T) {
	cfg := func(extra string) *Config {
		c, err := LoadConfigBytes([]byte(`{`+extra+`"targets": [{"name": "lab","server": "127.0.0.1:1", "user": "u@example.com", "password": "pw"}]}`), "t", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// the token itself, default header
	srv, _ := NewServer(cfg(`"auth_token": "s3cret", `), nil, nil)
	if s := probeStatus(t, srv, "/probe?target=nope"); s != http.StatusUnauthorized {
		t.Errorf("no token: %d", s)
	}
	if s := probeStatus(t, srv, "/probe?target=nope", "Authorization", "Bearer wrong"); s != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", s)
	}
	if s := probeStatus(t, srv, "/probe?target=nope", "Authorization", "Bearer s3cret"); s != http.StatusNotFound {
		t.Errorf("right token reaches the handler (unknown target): %d", s)
	}
	if s := probeStatus(t, srv, "/probe?target=nope&auth=s3cret"); s != http.StatusNotFound {
		t.Errorf("?auth=: %d", s)
	}

	// a named header: Authorization is left alone
	srv, _ = NewServer(cfg(`"auth_token": "s3cret", "auth_token_name": "X-Auth-Token", `), nil, nil)
	if s := probeStatus(t, srv, "/probe?target=nope", "X-Auth-Token", "s3cret"); s != http.StatusNotFound {
		t.Errorf("named header: %d", s)
	}
	if s := probeStatus(t, srv, "/probe?target=nope", "Authorization", "Bearer s3cret"); s != http.StatusUnauthorized {
		t.Errorf("with a named header Authorization does not count: %d", s)
	}
	if s := probeStatus(t, srv, "/probe?target=nope", "X-Auth-Token", "wrong"); s != http.StatusUnauthorized {
		t.Errorf("wrong value in the named header: %d", s)
	}

	// a file wins over the value, the value over a variable name
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := cfg(`"auth_token": "value", `)
	c.AuthTokenFile = tf
	if tok, err := c.authToken(); err != nil || "from-file" != tok {
		t.Errorf("file first: %q, %v", tok, err)
	}
	c.AuthTokenFile = ""
	t.Setenv("SOME_TOKEN", "from-variable")
	c.AuthTokenEnv = "SOME_TOKEN"
	if tok, _ := c.authToken(); "value" != tok {
		t.Errorf("value second: %q", tok)
	}
	c.AuthToken = ""
	if tok, _ := c.authToken(); "from-variable" != tok {
		t.Errorf("variable name last: %q", tok)
	}

	// the environment variables are the same keys
	c2, err := LoadConfigBytes([]byte(`{"targets": [{"name": "lab","server": "s", "user": "u@example.com", "password": "pw"}]}`), "t",
		[]string{"EASEXPORTER_AUTH_TOKEN=env-token", "EASEXPORTER_AUTH_TOKEN_NAME=X-Probe-Token"})
	if err != nil || "env-token" != c2.AuthToken || "X-Probe-Token" != c2.AuthTokenName || 0 != len(c2.Warnings) {
		t.Errorf("env: %+v, %v", c2, err)
	}

	// a bad name is refused when the configuration is loaded
	if _, err := LoadConfigBytes([]byte(`{"auth_token_name": "Cookie", "targets": [{"name": "lab","server": "s", "user": "u@example.com", "password": "pw"}]}`), "t", nil); err == nil {
		t.Error("a name that is used for other things must be refused")
	}
}

// The token for fetching the configuration can have its own header name too.
func TestRemoteTokenName(t *testing.T) {
	var gotAuth, gotNamed string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotNamed = r.Header.Get("Authorization"), r.Header.Get("X-Config-Token")
		if gotNamed != "tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(remoteDoc))
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"EASEXPORTER_CONFIG_CA_FILE=" + ca, "EASEXPORTER_CONFIG_TOKEN=tok", "EASEXPORTER_CONFIG_TOKEN_NAME=x-config-token"}

	r, err := NewRemote(srv.URL, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if "" != gotAuth || "tok" != gotNamed {
		t.Errorf("headers: Authorization %q, X-Config-Token %q", gotAuth, gotNamed)
	}

	// without a name it is Authorization: Bearer, which this server does not accept
	r, _ = NewRemote(srv.URL, env[:2])
	if _, err := r.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "401") || "Bearer tok" != gotAuth {
		t.Errorf("default header: %v (Authorization %q)", err, gotAuth)
	}

	// a bad name is refused when the remote source is prepared
	if _, err := NewRemote(srv.URL, append(env[:2:2], "EASEXPORTER_CONFIG_TOKEN_NAME=Cookie")); err == nil {
		t.Error("a bad name must be refused")
	}
}
