// Package oidcauth checks the JWT access tokens that an OpenID Connect provider issues, for a resource server (eas-mcp).
//
// The provider is found by its issuer URL (OIDC discovery) and its signing keys are fetched from the jwks_uri. The keys are kept in memory
// and saved to a file, so that a program that cannot reach the provider (it is down, or the network is) goes on with the keys that it
// has: a token that was signed with a known key is checked as before, and a new key is fetched as soon as the provider is there again.
//
// What is checked is the signature, the issuer, the audience (the token must be for this server) and the time of the token (exp). Who
// may use the server is decided by the list of users: the e-mail address (or another claim) of the token against patterns with wildcards.
// There is no registration of clients on this side: the client registers with the provider itself.
package oidcauth

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// signingAlgs are the algorithms of a token that are accepted. "none" and the shared secret ones (HS256 and the others) are not: the keys of the
// provider are public keys.
var signingAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512, jose.ES256, jose.ES384, jose.ES512, jose.EdDSA}

// Config is what a Verifier needs.
type Config struct {
	// Issuer is the issuer URL of the provider, exactly as the provider writes it in its tokens ("iss") and in its discovery document.
	Issuer string
	// Audience is what the token must be issued for: the "aud" claim has to contain it. It is the address of this server, or a name that the
	// provider knows it by.
	Audience string
	// Users are the patterns of the users that are allowed: a user is the value of UserClaim, in lower case, and a pattern is matched with
	// "*" as the wildcard: "*" is everybody, "*@example.com" everybody of a domain, "alice@example.com" one user. Empty means nobody.
	Users []string
	// UserClaim is the claim that names the user. Default: the first of email, preferred_username, upn and sub that the token has.
	UserClaim string
	// CacheFile saves the signing keys; empty means they are kept in memory only.
	CacheFile string
	// HTTPClient is for the discovery and the keys; default: a client with the timeout of ten seconds.
	HTTPClient *http.Client
	// MinRefresh is the least time between two attempts to fetch the keys (default 30 seconds): a flood of tokens with a key that nobody
	// has must not become a flood of requests to the provider.
	MinRefresh time.Duration
	Log        *slog.Logger
}

// Identity is what a valid token says.
type Identity struct {
	User   string
	Scopes []string
	Expiry time.Time
	Claims map[string]any
}

// Verifier checks tokens.
type Verifier struct {
	cfg Config
	ks  *keySet
}

// New makes the verifier. It does not fail when the provider cannot be reached: it uses the keys of the cache file, and when there are
// none it says so, and the tokens are refused until the keys can be fetched.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("the issuer and the audience are needed")
	}
	if len(cfg.Users) == 0 {
		return nil, errors.New("the users that are allowed are needed: a pattern such as alice@example.com, *@example.com or * for everybody")
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return nil, fmt.Errorf("the issuer %q is not an https URL", cfg.Issuer)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.MinRefresh <= 0 {
		cfg.MinRefresh = 30 * time.Second
	}
	for i, p := range cfg.Users {
		cfg.Users[i] = strings.ToLower(strings.TrimSpace(p))
		if _, err := path.Match(cfg.Users[i], ""); err != nil {
			return nil, fmt.Errorf("the user pattern %q: %w", p, err)
		}
	}

	ks := &keySet{cfg: cfg}
	v := &Verifier{cfg: cfg, ks: ks}

	ks.loadCache()
	if err := ks.refresh(ctx, true); err != nil {
		if ks.count() > 0 {
			cfg.Log.Warn("the OIDC provider cannot be reached: the signing keys that were saved are used", "issuer", cfg.Issuer, "keys", ks.count(),
				"saved", ks.saved.Format(time.RFC3339), "error", err.Error())
		} else {
			cfg.Log.Warn("the OIDC provider cannot be reached and no signing keys were saved: tokens are refused until it can be", "issuer", cfg.Issuer, "error", err.Error())
		}
	} else {
		cfg.Log.Info("OIDC provider found", "issuer", cfg.Issuer, "audience", cfg.Audience, "keys", ks.count(), "users", cfg.Users)
	}
	return v, nil
}

// Verify checks a token and returns who it is. The error says why a token is refused; it is for the log, not for the client. The signature
// is checked with the keys of the provider, then the claims: the issuer, the audience and the time (exp is required, nbf is checked when it is
// there, with 30 seconds of tolerance for the clocks).
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	payload, err := v.ks.VerifySignature(ctx, raw)
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("the claims of the token are not JSON: %w", err)
	}
	if iss, _ := claims["iss"].(string); iss != v.cfg.Issuer {
		return nil, fmt.Errorf("the token is from the issuer %q, not %q", iss, v.cfg.Issuer)
	}
	if !hasAudience(claims["aud"], v.cfg.Audience) {
		return nil, fmt.Errorf("the token is not for this server: its audience is %v, not %q", claims["aud"], v.cfg.Audience)
	}
	const leeway = 30 * time.Second
	now := time.Now()
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return nil, errors.New("the token has no expiry (exp)")
	}
	if now.After(exp.Add(leeway)) {
		return nil, fmt.Errorf("the token expired at %s", exp.Format(time.RFC3339))
	}
	if nbf, ok := numericDate(claims["nbf"]); ok && now.Add(leeway).Before(nbf) {
		return nil, fmt.Errorf("the token is not valid before %s", nbf.Format(time.RFC3339))
	}
	user := v.userOf(claims)
	if user == "" {
		return nil, errors.New("the token names no user (claim " + v.claimName() + ")")
	}
	return &Identity{User: user, Scopes: scopesOf(claims), Expiry: exp, Claims: claims}, nil
}

// hasAudience says whether the "aud" claim, a string or a list of strings, has the audience.
func hasAudience(aud any, want string) bool {
	switch x := aud.(type) {
	case string:
		return x == want
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// numericDate reads a time claim: the seconds since 1970, a JSON number.
func numericDate(v any) (time.Time, bool) {
	f, ok := v.(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// UserAllowed says whether the user matches one of the patterns of the allowed users.
func (v *Verifier) UserAllowed(user string) bool {
	user = strings.ToLower(user)
	for _, p := range v.cfg.Users {
		if ok, _ := path.Match(p, user); ok || p == "*" {
			return true
		}
	}
	return false
}

// Discovery is the discovery document of the provider (a copy), as it was fetched or saved; nil when there is none.
func (v *Verifier) Discovery() map[string]any {
	v.ks.mu.Lock()
	defer v.ks.mu.Unlock()
	if v.ks.discovery == nil {
		return nil
	}
	out := make(map[string]any, len(v.ks.discovery))
	for k, val := range v.ks.discovery {
		out[k] = val
	}
	return out
}

// Issuer is the issuer that tokens are accepted from.
func (v *Verifier) Issuer() string { return v.cfg.Issuer }

func (v *Verifier) claimName() string {
	if v.cfg.UserClaim != "" {
		return v.cfg.UserClaim
	}
	return "email"
}

func (v *Verifier) userOf(claims map[string]any) string {
	names := []string{"email", "preferred_username", "upn", "sub"}
	if v.cfg.UserClaim != "" {
		names = []string{v.cfg.UserClaim}
	}
	for _, n := range names {
		if s, ok := claims[n].(string); ok && strings.TrimSpace(s) != "" {
			return strings.ToLower(strings.TrimSpace(s))
		}
	}
	return ""
}

// scopesOf are the scopes of a token: "scope" is a string with spaces (RFC 9068), "scp" is a list or a string (Entra ID and others).
func scopesOf(claims map[string]any) []string {
	var out []string
	for _, name := range []string{"scope", "scp"} {
		switch x := claims[name].(type) {
		case string:
			out = append(out, strings.Fields(x)...)
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// errRecently is the answer of refresh that was asked for again too soon.
var errRecently = errors.New("the keys were asked for a short time ago")

// keySet holds the signing keys of the provider: in memory, in the cache file, and fetched from the provider when a token has a key that is
// not known. It is the KeySet that the verifier of go-oidc asks.
type keySet struct {
	cfg Config

	mu        sync.Mutex
	keys      jose.JSONWebKeySet
	jwksURI   string
	discovery map[string]any // the discovery document of the provider, as it was fetched
	saved     time.Time      // when the keys were fetched, or saved
	lastTry   time.Time
}

func (k *keySet) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.keys.Keys)
}

// VerifySignature is the KeySet of go-oidc: it returns the payload of the token when a key of the provider has signed it. An unknown key
// (a provider that has rotated its keys) makes one attempt to fetch the keys, not more often than MinRefresh.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, signingAlgs)
	if err != nil {
		return nil, fmt.Errorf("the token is not a signed JWT with an accepted algorithm: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("the token must have exactly one signature")
	}
	kid := jws.Signatures[0].Header.KeyID
	if payload, ok := k.try(jws, kid); ok {
		return payload, nil
	}
	err = k.refresh(ctx, false)
	switch {
	case err == nil:
		k.cfg.Log.Info("the signing keys of the OIDC provider were fetched: a token has a key that was not known", "key_id", kid, "keys", k.count())
		if payload, ok := k.try(jws, kid); ok {
			return payload, nil
		}
	case !errors.Is(err, errRecently):
		k.cfg.Log.Warn("the signing keys of the OIDC provider could not be fetched", "key_id", kid, "keys_known", k.count(), "error", err.Error())
	}
	return nil, fmt.Errorf("no signing key of the provider verifies the token (key id %q)", kid)
}

func (k *keySet) try(jws *jose.JSONWebSignature, kid string) ([]byte, bool) {
	k.mu.Lock()
	var candidates []jose.JSONWebKey
	if kid != "" {
		candidates = k.keys.Key(kid)
	} else {
		candidates = append(candidates, k.keys.Keys...)
	}
	k.mu.Unlock()
	for _, c := range candidates {
		if c.Use != "" && c.Use != "sig" {
			continue
		}
		if _, ok := c.Key.(crypto.PublicKey); !ok || c.IsPublic() == false {
			continue
		}
		if payload, err := jws.Verify(c); err == nil {
			return payload, true
		}
	}
	return nil, false
}

// refresh fetches the discovery document (when the address of the keys is not known) and the keys. Without force it is done at most once
// in MinRefresh. The keys that are fetched are saved to the cache file.
func (k *keySet) refresh(ctx context.Context, force bool) error {
	k.mu.Lock()
	if !force && time.Since(k.lastTry) < k.cfg.MinRefresh {
		k.mu.Unlock()
		return errRecently
	}
	k.lastTry = time.Now()
	uri := k.jwksURI
	k.mu.Unlock()

	var disc map[string]any
	if uri == "" || force {
		if err := k.getJSON(ctx, strings.TrimSuffix(k.cfg.Issuer, "/")+"/.well-known/openid-configuration", &disc); err != nil {
			return fmt.Errorf("discovery: %w", err)
		}
		issuer, _ := disc["issuer"].(string)
		jwks, _ := disc["jwks_uri"].(string)
		if issuer != k.cfg.Issuer {
			return fmt.Errorf("discovery: the issuer of the provider is %q, not %q", issuer, k.cfg.Issuer)
		}
		u, err := url.Parse(jwks)
		if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
			return fmt.Errorf("discovery: the address of the keys %q is not an https URL", jwks)
		}
		uri = jwks
	}
	var set jose.JSONWebKeySet
	if err := k.getJSON(ctx, uri, &set); err != nil {
		return fmt.Errorf("signing keys: %w", err)
	}
	if len(set.Keys) == 0 {
		return errors.New("signing keys: the provider has none")
	}
	k.mu.Lock()
	k.keys, k.jwksURI, k.saved = set, uri, time.Now()
	if disc != nil {
		k.discovery = disc
	}
	k.mu.Unlock()
	k.saveCache()
	return nil
}

func (k *keySet) getJSON(ctx context.Context, address string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	resp, err := k.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %s", address, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dest)
}

// cacheDoc is the cache file.
type cacheDoc struct {
	Issuer  string          `json:"issuer"`
	JWKSURI string          `json:"jwks_uri"`
	Saved   time.Time       `json:"saved"`
	Keys    json.RawMessage `json:"jwks"`
	// Discovery is the discovery document of the provider.
	Discovery map[string]any `json:"discovery,omitempty"`
}

func (k *keySet) loadCache() {
	if k.cfg.CacheFile == "" {
		return
	}
	raw, err := os.ReadFile(k.cfg.CacheFile)
	if err != nil {
		return
	}
	var doc cacheDoc
	var set jose.JSONWebKeySet
	if json.Unmarshal(raw, &doc) != nil || doc.Issuer != k.cfg.Issuer || json.Unmarshal(doc.Keys, &set) != nil || len(set.Keys) == 0 {
		k.cfg.Log.Warn("the saved OIDC keys are not usable and are ignored", "file", k.cfg.CacheFile)
		return
	}
	k.mu.Lock()
	k.keys, k.jwksURI, k.saved, k.discovery = set, doc.JWKSURI, doc.Saved, doc.Discovery
	k.mu.Unlock()
}

func (k *keySet) saveCache() {
	if k.cfg.CacheFile == "" {
		return
	}
	k.mu.Lock()
	keys, err := json.Marshal(k.keys)
	doc := cacheDoc{Issuer: k.cfg.Issuer, JWKSURI: k.jwksURI, Saved: k.saved, Keys: keys, Discovery: k.discovery}
	k.mu.Unlock()
	if err != nil {
		return
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(k.cfg.CacheFile), ".tmp-oidc-keys-")
	if err == nil {
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr == nil && cerr == nil {
			err = os.Rename(tmp.Name(), k.cfg.CacheFile)
		} else {
			err = errors.Join(werr, cerr)
		}
	}
	if err != nil {
		k.cfg.Log.Warn("the OIDC keys could not be saved: after a restart without the provider there are none", "file", k.cfg.CacheFile, "error", err.Error())
	}
}
