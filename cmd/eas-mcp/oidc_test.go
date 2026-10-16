package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/internal/oidcauth"
)

// idp is a fake OpenID Connect provider with one signing key.
type idp struct {
	*httptest.Server
	key *ecdsa.PrivateKey
}

func newIdP(t *testing.T) *idp {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.URL, "jwks_uri": p.URL + "/jwks", "authorization_endpoint": p.URL + "/auth", "token_endpoint": p.URL + "/token"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "ES256", Use: "sig"}}})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *idp) token(t *testing.T, audience, email string) string {
	t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: p.key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"iss": p.URL, "aud": audience, "email": email, "sub": "u1", "exp": time.Now().Add(time.Hour).Unix()})
	obj, _ := sig.Sign(payload)
	raw, _ := obj.CompactSerialize()
	return raw
}

const resourceURL = "https://eas-mcp.example.com:8445/mcp"

// oidcEndpoint serves the MCP endpoint of a manager on the mock ActiveSync server, with OIDC and, when localToken is not empty, the local token.
func oidcEndpoint(t *testing.T, p *idp, localToken string) *httptest.Server {
	t.Helper()
	return oidcEndpointAt(t, p, localToken, resourceURL)
}

// oidcEndpointAt is the same with another public URL; "" takes the address from every request.
func oidcEndpointAt(t *testing.T, p *idp, localToken, publicURL string) *httptest.Server {
	t.Helper()
	return oidcEndpointLog(t, p, localToken, publicURL, nil, quiet())
}

func oidcEndpointLog(t *testing.T, p *idp, localToken, publicURL string, scopes []string, log *slog.Logger, tweak ...func(*authn)) *httptest.Server {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{{Name: "default", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	v, err := oidcauth.New(context.Background(), oidcauth.Config{Issuer: p.URL, Audience: resourceURL, Users: []string{"*@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := newAuthn(localToken, v, publicURL, scopes, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tweak {
		f(a)
	}
	api := httptest.NewServer(httpHandler(newServer(m, serverOptions{Audit: a.audit}), a, nil, false, true, log))
	t.Cleanup(api.Close)
	return api
}

func post(t *testing.T, url, authorization string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestWithoutATokenTheAnswerPointsToTheMetadata(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpoint(t, p, "")
	resp := post(t, api.URL+"/mcp", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	want := `resource_metadata="https://eas-mcp.example.com:8445/.well-known/oauth-protected-resource/mcp"`
	if h := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(h, "Bearer") || !strings.Contains(h, want) {
		t.Errorf("WWW-Authenticate = %q, want it to hold %s", h, want)
	}
}

func TestTheProtectedResourceMetadataNamesTheProvider(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpoint(t, p, "")
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := http.Get(api.URL + path) // no token: a client reads it to find the provider
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, resp, err)
		}
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if doc.Resource != resourceURL || len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != p.URL {
			t.Errorf("%s: %+v", path, doc)
		}
	}
}

func TestAJWTOfTheProviderOpensTheEndpoint(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpoint(t, p, "")
	tr := &mcp.StreamableClientTransport{Endpoint: api.URL + "/mcp", HTTPClient: &http.Client{Transport: withToken{p.token(t, resourceURL, "alice@example.com")}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders"}); err != nil || res.IsError {
		t.Errorf("list_folders: %v %+v", err, res)
	}
}

func TestATokenThatTheProviderOrTheListRefusesIsNotLetIn(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpoint(t, p, "")
	if r := post(t, api.URL+"/mcp", "Bearer "+p.token(t, resourceURL, "mallory@evil.org")); r.StatusCode != http.StatusForbidden {
		t.Errorf("a user who is not on the list: %d, want 403", r.StatusCode)
	}
	if r := post(t, api.URL+"/mcp", "Bearer "+p.token(t, "https://other.example.com/mcp", "alice@example.com")); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a token for another audience: %d, want 401", r.StatusCode)
	}
	if r := post(t, api.URL+"/mcp", "Bearer not-a-token"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("garbage: %d, want 401", r.StatusCode)
	}
}

func TestTheLocalTokenIsTheFallback(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpoint(t, p, "s3cret")
	if r := post(t, api.URL+"/mcp", "Bearer s3cret"); r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden {
		t.Errorf("the local token: %d", r.StatusCode)
	}
	if r := post(t, api.URL+"/mcp", "Bearer "+p.token(t, resourceURL, "alice@example.com")); r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden {
		t.Errorf("a JWT next to the local token: %d", r.StatusCode)
	}
	if r := post(t, api.URL+"/mcp", "Bearer wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d, want 401", r.StatusCode)
	}
}

func TestTheOldAddressesOfTheLoginServerAnswerWithTheProvidersEndpoints(t *testing.T) {
	for _, compat := range []string{"provider", "origin", "off", ""} {
		t.Run("compat="+compat, func(t *testing.T) { oldAddresses(t, compat) })
	}
}

func oldAddresses(t *testing.T, compat string) {
	p := newIdP(t)
	api := oidcEndpointLog(t, p, "", resourceURL, nil, quiet(), func(a *authn) { a.compat = compat })
	host := strings.TrimPrefix(api.URL, "http://")
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		resp, err := http.Get(api.URL + path)
		if compat == "off" {
			if err != nil || resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s with off: %v %v, want 404", path, resp, err)
			}
			continue
		}
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", path, resp, err)
		}
		var doc map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		// the endpoints are the provider's; the issuer is the provider's too (the default), or with "origin" the address that was asked
		wantIssuer := p.URL
		if compat == "origin" {
			wantIssuer = "http://" + host
		}
		if doc["issuer"] != wantIssuer || doc["authorization_endpoint"] != p.URL+"/auth" || doc["token_endpoint"] != p.URL+"/token" || doc["jwks_uri"] != p.URL+"/jwks" {
			t.Errorf("%s: %v", path, doc)
		}
	}
	for _, path := range []string{"/register", "/authorize", "/token"} { // this server has none of the endpoints
		resp, err := http.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestWithoutAPublicURLTheAddressComesFromTheRequest(t *testing.T) {
	p := newIdP(t)
	api := oidcEndpointAt(t, p, "", "")
	host := strings.TrimPrefix(api.URL, "http://")

	req, _ := http.NewRequest("POST", api.URL+"/mcp", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := `resource_metadata="http://` + host + `/.well-known/oauth-protected-resource/mcp"`
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), want) {
		t.Errorf("%d %q, want it to hold %s", resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
	}

	for name, c := range map[string]struct {
		path, proto, host, want string
	}{
		"as the client reached it": {"/.well-known/oauth-protected-resource/mcp", "", "", "http://" + host + "/mcp"},
		"the path without /mcp":    {"/.well-known/oauth-protected-resource", "", "", "http://" + host + "/mcp"},
		"behind a proxy":           {"/.well-known/oauth-protected-resource/mcp", "https", "eas-mcp.example.com:8445, 10.0.0.1", "https://eas-mcp.example.com:8445/mcp"},
	} {
		req, _ := http.NewRequest("GET", api.URL+c.path, nil)
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
			req.Header.Set("X-Forwarded-Host", c.host)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", name, resp, err)
		}
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if doc.Resource != c.want || len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != p.URL {
			t.Errorf("%s: %+v, want the resource %s", name, doc, c.want)
		}
	}
}

// A request that is not let in says why in the log: that is what a person who sets this up has to go on. The tokens are never in it.
func TestARefusedRequestSaysWhyInTheLog(t *testing.T) {
	p := newIdP(t)
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := oidcEndpointLog(t, p, "", "", []string{"email"}, log)
	read := func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
	reset := func() { mu.Lock(); buf.Reset(); mu.Unlock() }

	good := p.tokenWith(t, map[string]any{"aud": resourceURL, "email": "alice@example.com", "scope": "email"})
	for name, c := range map[string]struct {
		authorization string
		want          []string
		notWant       []string
	}{
		"no token":                  {"", []string{"level=INFO", "a request was not let in", "no Authorization header", "status=401"}, nil},
		"not a bearer token":        {"Basic dXNlcjpwYXNz", []string{"level=WARN", "not a Bearer token"}, []string{"dXNlcjpwYXNz"}},
		"an opaque token":           {"Bearer opaque-token-of-the-provider", []string{"level=WARN", "not a JWT", "opaque"}, []string{"opaque-token-of-the-provider"}},
		"another audience":          {"Bearer " + p.token(t, "https://other.example.com/mcp", "alice@example.com"), []string{"level=WARN", "the JWT is refused", "not for this server", "https://other.example.com/mcp"}, nil},
		"a token without the scope": {"Bearer " + p.tokenWith(t, map[string]any{"aud": resourceURL, "email": "alice@example.com"}), []string{"level=WARN", "status=403", "lacks a scope", "email"}, nil},
		"a user who is not allowed": {"Bearer " + p.tokenWith(t, map[string]any{"aud": resourceURL, "email": "mallory@evil.org", "scope": "email"}), []string{"level=WARN", "is not allowed", "mallory@evil.org"}, nil},
	} {
		reset()
		post(t, api.URL+"/mcp", c.authorization)
		out := read()
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: the log has no %q:\n%s", name, w, out)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(out, w) {
				t.Errorf("%s: the log has %q, which is a secret:\n%s", name, w, out)
			}
		}
	}
	reset() // a request that is let in is at the debug level, with the user
	post(t, api.URL+"/mcp", "Bearer "+good)
	if out := read(); !strings.Contains(out, "a request was let in") || !strings.Contains(out, "alice@example.com") || strings.Contains(out, good) {
		t.Errorf("a request that is let in:\n%s", out)
	}
}

type lockedWriter struct {
	b  *strings.Builder
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// tokenWith is a token of the provider with exactly these claims (the issuer, the expiry and the subject are added).
func (p *idp) tokenWith(t *testing.T, claims map[string]any) string {
	t.Helper()
	c := map[string]any{"iss": p.URL, "sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: p.key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(c)
	obj, _ := sig.Sign(payload)
	raw, _ := obj.CompactSerialize()
	return raw
}
