package client

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/nashcom/eas-tools/internal/ipacl"
)

// The REST API is the operations of the Manager on URLs. The answer of every request is the JSON that the command of the same name
// prints (see Encode), and an error is an ErrorBody with the HTTP status of the Error. The requests are listed in routes.go; GET
// /openapi.json describes them (OpenAPI 3) and GET /docs shows them in a page where they can be tried, when the API is started with
// that page.
//
// {folder} is a folder id or inbox, sent, drafts, deleted or the name of the folder. Path parts are URL-encoded, so a mail id with a
// slash goes as %2F.

const maxBody = 10 << 20

type restHandler struct {
	m      *Manager
	token  string
	routes []route
	docs   http.Handler
}

// HandlerOptions shape the REST API.
type HandlerOptions struct {
	// Token is the bearer token; with one every request but /health needs "Authorization: Bearer <token>". The token is never taken from
	// the URL, which ends up in logs. Without a token the API is open: the command that serves it only does that on a loopback address.
	Token string
	// Docs adds GET /docs, a page (Swagger UI) where the requests can be tried, and makes it and /openapi.json open: a browser cannot send the
	// token when it opens a page. The requests that the page makes still need the token.
	Docs bool
	// DocsUI is where the browser loads Swagger UI from: the URL of a directory with swagger-ui.css and swagger-ui-bundle.js (see ParseDocsUI).
	// Empty is the CDN, in the version that is pinned in the program, checked by integrity hashes.
	DocsUI string
	// Allow restricts the API to these addresses of peers: every request, also /health and /docs, is refused with 403 before anything else
	// when the peer is not in the list. The loopback interface is always allowed. nil or empty: no restriction.
	Allow *ipacl.List
	// OnDenied, when set, is told about every refused request.
	OnDenied func(remote string, r *http.Request)
}

// Handler returns the REST API with a token and without the page of the documentation.
func (m *Manager) Handler(token string) http.Handler {
	return m.HandlerWith(HandlerOptions{Token: token})
}

// HandlerWith returns the REST API.
func (m *Manager) HandlerWith(o HandlerOptions) http.Handler {
	h := &restHandler{m: m, token: o.Token, routes: routeTable()}
	if o.Docs {
		base, err := ParseDocsUI(o.DocsUI)
		if err != nil {
			base = defaultDocsUI // the command checks it with ParseDocsUI before
		}
		h.docs = docsHandler(base)
	}
	return o.Allow.Wrap(h, o.OnDenied)
}

func (h *restHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts, ok := pathParts(r)
	if !ok {
		writeError(w, newError(http.StatusBadRequest, "bad_path", "the path is not valid"))
		return
	}
	rt, v, allowed := match(h.routes, r.Method, parts)

	// The page of the documentation and its files are not in the table: they are not part of the API. A browser cannot send the bearer
	// token when it opens a page, so with the page switched on the page, its files and the description need no token: they say what the
	// API is and hold no data. Every request of the API itself still needs the token (the Authorize button of the page takes it).
	isDocs := h.docs != nil && len(parts) > 0 && parts[0] == "docs"
	public := (rt != nil && rt.Public) || isDocs || (h.docs != nil && rt != nil && rt.ID == "getOpenAPI")
	if !public && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="eas-client"`)
		writeError(w, newError(http.StatusUnauthorized, "unauthorized", "a valid bearer token is required"))
		return
	}
	if isDocs {
		h.docs.ServeHTTP(w, r)
		return
	}
	switch {
	case rt != nil:
		rt.Handle(h, w, r, v)
	case len(allowed) > 0:
		sort.Strings(allowed)
		writeError(w, notAllowed(strings.Join(allowed, ", ")))
	default:
		writeError(w, newError(http.StatusNotFound, "not_found", "no such path"))
	}
}

func (h *restHandler) authorized(r *http.Request) bool {
	if h.token == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) == 1
}

// writeRaw sends a file, or the error that kept it from being read.
func writeRaw(w http.ResponseWriter, raw *Raw, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", raw.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+raw.Filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw.Data)
}

// pathParts splits the path and decodes every part.
func pathParts(r *http.Request) ([]string, bool) {
	raw := strings.Trim(r.URL.EscapedPath(), "/")
	if raw == "" {
		return nil, true
	}
	var parts []string
	for _, p := range strings.Split(raw, "/") {
		v, err := url.PathUnescape(p)
		if err != nil || v == "" {
			return nil, false
		}
		parts = append(parts, v)
	}
	return parts, true
}

func listParams(q url.Values) (ListParams, error) {
	var p ListParams
	var err error
	if p.Limit, err = limitParam(q); err != nil {
		return p, err
	}
	p.Since = q.Get("since")
	if v := q.Get("preview"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, newError(http.StatusBadRequest, "bad_request", "preview is a number of bytes")
		}
		p.Preview = n
	}
	return p, nil
}

func readBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return newError(http.StatusBadRequest, "bad_request", "the body is not valid JSON of the expected form: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return newError(http.StatusBadRequest, "bad_request", "the body has more than one JSON value")
	}
	return nil
}

func notAllowed(allow string) error {
	return newError(http.StatusMethodNotAllowed, "method_not_allowed", "this path supports: "+allow)
}

func reply(w http.ResponseWriter, status int, res any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, res)
}

func writeError(w http.ResponseWriter, err error) {
	e := AsError(err)
	if e.Status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", strings.TrimPrefix(e.Message, "this path supports: "))
	}
	writeJSON(w, e.Status, ErrorBody{Error: e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = Encode(w, v)
}
