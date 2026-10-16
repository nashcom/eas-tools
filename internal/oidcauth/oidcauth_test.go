package oidcauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// provider is a fake OIDC provider: the discovery document and the keys, and tokens that it signs.
type provider struct {
	*httptest.Server
	mu   sync.Mutex
	keys map[string]*ecdsa.PrivateKey // by key id
	hits int                          // requests for the keys
}

func newProvider(t *testing.T, kids ...string) *provider {
	t.Helper()
	p := &provider{keys: map[string]*ecdsa.PrivateKey{}}
	for _, kid := range kids {
		p.addKey(t, kid)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": p.URL, "jwks_uri": p.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.hits++
		var set jose.JSONWebKeySet
		for kid, k := range p.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

func (p *provider) addKey(t *testing.T, kid string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys[kid] = k
	p.mu.Unlock()
}

// token signs claims with the key kid.
func (p *provider) token(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	p.mu.Lock()
	k := p.keys[kid]
	p.mu.Unlock()
	return sign(t, jose.ES256, jose.JSONWebKey{Key: k, KeyID: kid}, claims)
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, claims map[string]any) string {
	t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := sig.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (p *provider) claims(over map[string]any) map[string]any {
	c := map[string]any{"iss": p.URL, "aud": "https://eas-mcp.example.com/mcp", "email": "Alice@Example.com", "sub": "u1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range over {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (p *provider) config(cache string) Config {
	return Config{Issuer: p.URL, Audience: "https://eas-mcp.example.com/mcp", Users: []string{"*@example.com"}, CacheFile: cache, MinRefresh: time.Millisecond}
}

func TestAValidTokenGivesTheUser(t *testing.T) {
	p := newProvider(t, "k1")
	v, err := New(context.Background(), p.config(""))
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(context.Background(), p.token(t, "k1", p.claims(map[string]any{"scope": "mail.read mail.send", "scp": []string{"a"}})))
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "alice@example.com" || strings.Join(id.Scopes, ",") != "mail.read,mail.send,a" || id.Expiry.Before(time.Now()) {
		t.Errorf("identity = %+v", id)
	}
	if aud := p.claims(map[string]any{"aud": []string{"other", "https://eas-mcp.example.com/mcp"}}); true {
		if _, err := v.Verify(context.Background(), p.token(t, "k1", aud)); err != nil {
			t.Errorf("an audience that is a list with ours in it: %v", err)
		}
	}
}

func TestATokenThatIsNotRightIsRefused(t *testing.T) {
	p := newProvider(t, "k1")
	v, err := New(context.Background(), p.config(""))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	hmac := sign(t, jose.HS256, []byte("0123456789abcdef0123456789abcdef"), p.claims(nil))
	for name, raw := range map[string]string{
		"another audience": p.token(t, "k1", p.claims(map[string]any{"aud": "https://other.example.com"})),
		"no audience":      p.token(t, "k1", p.claims(map[string]any{"aud": nil})),
		"another issuer":   p.token(t, "k1", p.claims(map[string]any{"iss": "https://evil.example.com"})),
		"expired":          p.token(t, "k1", p.claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})),
		"no expiry":        p.token(t, "k1", p.claims(map[string]any{"exp": nil})),
		"not valid yet":    p.token(t, "k1", p.claims(map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})),
		"no user":          p.token(t, "k1", p.claims(map[string]any{"email": nil, "sub": nil})),
		"an unknown key":   sign(t, jose.ES256, jose.JSONWebKey{Key: other, KeyID: "k1"}, p.claims(nil)),
		"a shared secret":  hmac,
		"garbage":          "not.a.jwt",
		"nothing":          "",
	} {
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Errorf("%s: the token was accepted", name)
		}
	}
}

func TestTheUsersThatAreAllowed(t *testing.T) {
	p := newProvider(t, "k1")
	cfg := p.config("")
	cfg.Users = []string{"alice@example.com", "*@nashcom.de", "Bob*"}
	v, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for user, want := range map[string]bool{
		"alice@example.com": true, "ALICE@EXAMPLE.COM": true, "carol@example.com": false, "daniel@nashcom.de": true,
		"daniel@nashcom.de.evil.org": false, "bob.smith@x.org": true, "": false,
	} {
		if got := v.UserAllowed(user); got != want {
			t.Errorf("%q: %v, want %v", user, got, want)
		}
	}
	cfg.Users = []string{"*"}
	if all, _ := New(context.Background(), cfg); !all.UserAllowed("anybody@anywhere.org") {
		t.Error("* does not allow everybody")
	}
	cfg.Users = nil
	if _, err := New(context.Background(), cfg); err == nil {
		t.Error("without users the verifier was made: nobody would be allowed, which is a mistake")
	}
}

func TestTheClaimThatNamesTheUser(t *testing.T) {
	p := newProvider(t, "k1")
	cfg := p.config("")
	cfg.UserClaim = "preferred_username"
	v, _ := New(context.Background(), cfg)
	id, err := v.Verify(context.Background(), p.token(t, "k1", p.claims(map[string]any{"preferred_username": "alice"})))
	if err != nil || id.User != "alice" {
		t.Errorf("%+v %v", id, err)
	}
	if _, err := v.Verify(context.Background(), p.token(t, "k1", p.claims(nil))); err == nil {
		t.Error("a token without the named claim was accepted: the e-mail address must not stand in for it")
	}
}

func TestAKeyThatTheProviderHasAddedIsFetched(t *testing.T) {
	p := newProvider(t, "k1")
	v, _ := New(context.Background(), p.config(""))
	p.addKey(t, "k2") // the provider has rotated its keys

	// more than MinRefresh: a fast clock (Linux) would otherwise still throttle the fetch
	time.Sleep(5 * time.Millisecond)
	if _, err := v.Verify(context.Background(), p.token(t, "k2", p.claims(nil))); err != nil {
		t.Errorf("a token of the new key: %v", err)
	}
}

func TestWithoutTheProviderTheSavedKeysAreUsed(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "oidc-keys.json")
	p := newProvider(t, "k1")
	raw := p.token(t, "k1", p.claims(nil))
	if _, err := New(context.Background(), p.config(cache)); err != nil { // fetches and saves the keys
		t.Fatal(err)
	}
	cfg := p.config(cache)
	p.Close() // the provider is gone

	v, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the start without the provider: %v", err)
	}
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Errorf("a token of a saved key without the provider: %v", err)
	}
	if v2, _ := New(context.Background(), Config{Issuer: cfg.Issuer, Audience: cfg.Audience, Users: cfg.Users, CacheFile: filepath.Join(t.TempDir(), "none.json"), MinRefresh: time.Millisecond}); true {
		if _, err := v2.Verify(context.Background(), raw); err == nil {
			t.Error("without the provider and without saved keys a token was accepted")
		}
	}
}

func TestTheIssuerHasToBeHTTPS(t *testing.T) {
	for _, bad := range []string{"", "http://idp.example.com", "idp.example.com", "ftp://idp.example.com"} {
		if _, err := New(context.Background(), Config{Issuer: bad, Audience: "a", Users: []string{"*"}}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestTheDiscoveryDocumentAndTheIssuerAreThereForTheServer(t *testing.T) {
	p := newProvider(t, "k1")
	v, err := New(context.Background(), p.config(""))
	if err != nil {
		t.Fatal(err)
	}
	if v.Issuer() != p.URL {
		t.Errorf("issuer %q", v.Issuer())
	}
	d := v.Discovery()
	if d["issuer"] != p.URL || d["jwks_uri"] != p.URL+"/jwks" {
		t.Errorf("discovery %v", d)
	}
	d["issuer"] = "changed" // a copy: the document of the verifier stays
	if v.Discovery()["issuer"] != p.URL {
		t.Error("the discovery document was changed through the copy")
	}
	// a provider that was never reached has no document
	cfg := p.config("")
	p.Close()
	cfg.Issuer = p.URL
	if gone, err := New(context.Background(), cfg); err != nil || gone.Discovery() != nil {
		t.Errorf("a provider that cannot be reached: %v %v", gone, err)
	}
}
