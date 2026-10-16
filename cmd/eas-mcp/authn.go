package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/nashcom/eas-tools/internal/oidcauth"
)

// authn is how the MCP endpoint knows who is calling. Two ways, and both can be on:
//
//   - OIDC (the preferred): the client sends a JWT access token that an OpenID Connect provider issued for this server. It is checked with the
//     keys of the provider (internal/oidcauth), and the user of the token has to match the users that are allowed. A client without a token
//     gets the answer 401 with a pointer to the protected resource metadata of this server (RFC 9728, the MCP authorization of the
//     specification): the metadata names the provider, and the client does the login with it and registers there. There is no registration here.
//   - The local token: one shared secret (EASCLIENT_AUTH_TOKEN, -auth-token-file) that is the same for every client. It is tried when the token
//     is not a JWT of the provider, or when the provider says no.
//
// Without either the endpoint is open, and the network has to protect it.
type authn struct {
	token       string
	oidc        *oidcauth.Verifier
	resource    string // the public URL of the endpoint: the "resource" of the metadata
	metadataURL string // where the metadata is, for the answer 401
	metaPaths   []string
	scopes      []string
	log         *slog.Logger
	// compat says what the legacy addresses of the login server answer: "provider" (the default) the document of the provider as it is, "origin" the
	// document with the issuer changed to the address of this server, "off" nothing (404).
	compat string
	audit  *auditor // the audit log; nil means none
}

// newAuthn makes the authentication. publicURL is the address that the clients use for the endpoint (the "resource" of the metadata). It is optional:
// without it the address is taken from each request, the host and port that the client used (X-Forwarded-Host and X-Forwarded-Proto behind a proxy),
// which is what a client can reach by definition.
func newAuthn(token string, v *oidcauth.Verifier, publicURL string, scopes []string, log *slog.Logger) (*authn, error) {
	a := &authn{token: token, oidc: v, scopes: scopes, log: log}
	if v == nil {
		return a, nil
	}
	a.metaPaths = []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"}
	if publicURL == "" {
		return a, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the public URL %q is not an http(s) URL: the protected resource metadata names it, and the clients use it", publicURL)
	}
	a.resource = strings.TrimSuffix(publicURL, "/")
	rest := strings.TrimSuffix(u.Path, "/")
	a.metadataURL = u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + rest
	if rest != "/mcp" && rest != "" {
		a.metaPaths = append(a.metaPaths, "/.well-known/oauth-protected-resource"+rest)
	}
	return a, nil
}

// enabled says whether the endpoint asks for credentials.
func (a *authn) enabled() bool { return a != nil && (a.token != "" || a.oidc != nil) }

// describe is a line for the log: what the endpoint accepts.
func (a *authn) describe() (auth, detail string) {
	switch {
	case a != nil && a.oidc != nil && a.token != "":
		return "oidc+token", "a JWT of " + a.oidc.Issuer() + ", or the local token; both as Authorization: Bearer"
	case a != nil && a.oidc != nil:
		return "oidc", "a JWT of " + a.oidc.Issuer() + " as Authorization: Bearer"
	case a != nil && a.token != "":
		return "token", "the local token as Authorization: Bearer"
	}
	return "none", "no token is set: every peer that -allow-from lets in may use it"
}

// verify is the check of the bearer middleware of the SDK.
func (a *authn) verify(ctx context.Context, raw string, r *http.Request) (*auth.TokenInfo, error) {
	tr, _ := r.Context().Value(authKey{}).(*authTrace)
	note := func(reason string) {
		if tr != nil && tr.reason == "" {
			tr.reason = reason
		}
	}
	isJWT := strings.Count(raw, ".") == 2
	if a.oidc != nil {
		if isJWT {
			id, err := a.oidc.Verify(ctx, raw)
			if err == nil {
				return &auth.TokenInfo{Scopes: id.Scopes, Expiration: id.Expiry, UserID: id.User, Extra: map[string]any{"method": "oidc"}}, nil
			}
			note("the JWT is refused: " + err.Error())
		} else {
			note("the token is not a JWT (three parts with dots): an opaque token of the provider cannot be checked here")
		}
	}
	if a.token != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(a.token)) == 1 {
		// the shared secret has no user and no end: it may do what the scopes ask for, and it is checked again with every request
		return &auth.TokenInfo{Scopes: a.scopes, Expiration: time.Now().Add(time.Hour), UserID: "local-token", Extra: map[string]any{"method": "token"}}, nil
	}
	if a.oidc == nil {
		note("the token is not the local token")
	}
	return nil, auth.ErrInvalidToken
}

// authKey and authTrace carry what happened in the check of one request from verify to the log of the answer.
type authKey struct{}

type authTrace struct {
	reason string
	logged bool
	remote string // where the request came from, for the audit log of the tool call
	agent  string
}

// statusWriter remembers the status of the answer. It passes Flush on: the answers of the MCP endpoint are streams.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRefused says why a request was not let in. A request without any token is the normal first step of the login (the client is sent to the
// provider), and is said at the info level; a token that is refused is a warning. The token itself is never logged.
func (a *authn) logRefused(r *http.Request, status int, tr *authTrace) {
	if tr.logged {
		return
	}
	header := r.Header.Get("Authorization")
	reason, level := tr.reason, slog.LevelWarn
	switch {
	case header == "":
		reason, level = "no Authorization header: the client has not logged in yet, or does not send the token", slog.LevelInfo
	case !strings.HasPrefix(header, "Bearer "):
		reason = "the Authorization header is not a Bearer token"
	case reason == "" && status == http.StatusForbidden:
		reason = "the token lacks a scope that the server requires: " + strings.Join(a.scopes, " ")
	case reason == "":
		reason = "the token is not valid"
	}
	a.log.Log(r.Context(), level, "a request was not let in", "status", status, "method", r.Method, "path", r.URL.Path, "host", r.Host, "remote", r.RemoteAddr,
		"user_agent", r.UserAgent(), "reason", reason)
	if header != "" {
		a.audit.event(r.Context(), slog.LevelWarn, "mcp.auth.denied", "a request was not let in", "outcome", "denied", "http.response.status_code", status, "client.address", r.RemoteAddr,
			"user_agent.original", r.UserAgent(), "url.path", r.URL.Path, "error.message", reason)
	}
}

// wrap protects a handler: a request without a valid token gets 401 (with the pointer to the metadata when there is an OIDC provider), and a
// user that is not allowed gets 403.
func (a *authn) wrap(next http.Handler) http.Handler {
	if !a.enabled() {
		return next
	}
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if info != nil && info.Extra["method"] == "oidc" && !a.oidc.UserAllowed(info.UserID) {
			if tr, _ := r.Context().Value(authKey{}).(*authTrace); tr != nil {
				tr.logged = true
			}
			a.log.Warn("a user of the OIDC provider is not allowed: it matches none of the oidc_users", "user", info.UserID, "remote", r.RemoteAddr, "path", r.URL.Path)
			a.audit.event(r.Context(), slog.LevelWarn, "mcp.auth.denied", "a user of the OIDC provider is not allowed", "outcome", "denied", "enduser.id", info.UserID,
				"auth.method", "oidc", "client.address", r.RemoteAddr, "user_agent.original", r.UserAgent(), "url.path", r.URL.Path, "error.message", "the user matches none of the oidc_users")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "error_description": "the user " + info.UserID + " is not allowed to use this server"})
			return
		}
		if info != nil {
			a.log.Debug("a request was let in", "method", info.Extra["method"], "user", info.UserID, "path", r.URL.Path, "remote", r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaURL := a.metadataURL
		if a.resource == "" { // no public URL: the address that this client used
			metaURL = requestOrigin(r) + "/.well-known/oauth-protected-resource/mcp"
		}
		tr := &authTrace{remote: r.RemoteAddr, agent: r.UserAgent()}
		r = r.WithContext(context.WithValue(r.Context(), authKey{}, tr))
		sw := &statusWriter{ResponseWriter: w}
		auth.RequireBearerToken(a.verify, &auth.RequireBearerTokenOptions{ResourceMetadataURL: metaURL, Scopes: a.scopes})(guarded).ServeHTTP(sw, r)
		if sw.status == http.StatusUnauthorized || sw.status == http.StatusForbidden {
			a.logRefused(r, sw.status, tr)
		}
	})
}

// metadata is the protected resource metadata (RFC 9728): the resource, and the provider that issues the tokens for it. It needs no token: a
// client reads it to find out where to log in.
func (a *authn) metadata() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resource := a.resource
		if resource == "" {
			resource = requestOrigin(r) + "/mcp"
		}
		auth.ProtectedResourceMetadataHandler(a.document(resource)).ServeHTTP(w, r)
	})
}

func (a *authn) document(resource string) *oauthex.ProtectedResourceMetadata {
	return &oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{a.oidc.Issuer()},
		ScopesSupported:        a.scopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "eas-mcp",
	}
}

// requestOrigin is the address that the client used to reach this server: scheme, host and port. Behind a proxy that is what the proxy says in
// X-Forwarded-Proto and X-Forwarded-Host (the first value of the list). It names nobody but the caller: the answer goes back to the one who asked.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstValue(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
		scheme = p
	}
	host := r.Host
	if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" {
		host = h
	}
	return scheme + "://" + host
}

func firstValue(list string) string {
	first, _, _ := strings.Cut(list, ",")
	return strings.TrimSpace(first)
}

// legacyPaths are the addresses where a client of the older MCP rule, and the fallback of the newer clients when they could not read the
// metadata of the resource, look for the login server: on the MCP server itself.
var legacyPaths = []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"}

// authServerMetadata answers the legacy addresses with the discovery document of the provider. Which issuer it has is the choice of compat, because
// the clients that ask here do not agree on what they check: a document with the issuer of the provider fails the check of the issuer against the
// address that the document came from (RFC 8414); a document with the address of this server as the issuer fails the check of the "iss" in the
// answer of the provider (RFC 9207), which Domino sends. Every endpoint in it (authorization, token, keys) is the provider's, so the client logs in
// there; nothing here issues or signs anything.
func (a *authn) authServerMetadata() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := a.oidc.Discovery()
		w.Header().Set("Access-Control-Allow-Origin", "*") // a public document, which a browser-based client reads
		w.Header().Set("Content-Type", "application/json")
		if doc == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "temporarily_unavailable", "error_description": "the discovery document of the provider has not been fetched yet"})
			return
		}
		if a.compat == "origin" { // a client that checks the issuer of a document against the address it was fetched from
			doc["issuer"] = requestOrigin(r)
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
}
