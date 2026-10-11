package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/client"
)

func get(t *testing.T, method, url, token string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestParseDocsUI(t *testing.T) {
	def, err := client.ParseDocsUI("")
	if err != nil || def != "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5" {
		t.Fatalf("the default = %q, %v", def, err)
	}
	for in, want := range map[string]string{
		"https://mirror.example.com/swagger-ui/": "https://mirror.example.com/swagger-ui",
		" http://localhost:8000 ":                "http://localhost:8000",
		"https://x.example.com/a/b///":           "https://x.example.com/a/b",
	} {
		if got, err := client.ParseDocsUI(in); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"swagger", "ftp://x/y", "https://", "javascript:alert(1)", "https://x/y?a=b", "https://x/y#z", "//x/y"} {
		if _, err := client.ParseDocsUI(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// By default the page loads Swagger UI from the CDN, from the stream of version 5. A moving location has no hashes; an exact version that the
// program knows has them.
func TestDocsPageLoadsSwaggerUIFromOutside(t *testing.T) {
	_, m := setup(t, false, "bob")
	ts := httptest.NewServer(m.HandlerWith(client.HandlerOptions{Token: "tok", Docs: true}))
	defer ts.Close()

	resp, page := get(t, "GET", ts.URL+"/docs", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("the page = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	text := string(page)
	for _, want := range []string{
		`href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">`,
		`src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>`,
		`src="/docs/docs-init.js"`, `id="swagger-ui"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "integrity=") || client.DocsUIHasIntegrity("https://cdn.jsdelivr.net/npm/swagger-ui-dist@5") {
		t.Error("a moving location cannot have integrity hashes")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self' https://cdn.jsdelivr.net;", "style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;", "connect-src 'self';", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy lacks %q: %s", want, csp)
		}
	}
	// no other place than the CDN may be named in the policy
	if strings.Count(csp, "https://") != 4 || strings.Contains(csp, "http://") || strings.Contains(csp, "*") {
		t.Errorf("the policy names more than the CDN: %s", csp)
	}

	// the script that starts it comes from the program, and it opens the description of the API
	r, init := get(t, "GET", ts.URL+"/docs/docs-init.js", "")
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") || !strings.Contains(string(init), `url: "/openapi.json"`) {
		t.Errorf("docs-init.js = %d %s: %s", r.StatusCode, r.Header.Get("Content-Type"), init)
	}
	if r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("docs-init.js has no nosniff")
	}

	// nothing else is served under /docs: the program holds no files of Swagger UI
	for _, p := range []string{"/docs/swagger-ui-bundle.js", "/docs/swagger-ui.css", "/docs/nothing", "/docs/..%2Fclient.go", "/docs/LICENSE"} {
		if resp, _ := get(t, "GET", ts.URL+p, ""); resp.StatusCode != 404 {
			t.Errorf("%s = %d, want 404", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/docs", "/docs/"} {
		if r, _ := get(t, "GET", ts.URL+p, ""); r.StatusCode != 200 {
			t.Errorf("%s = %d", p, r.StatusCode)
		}
	}
	if r, _ := get(t, "HEAD", ts.URL+"/docs", ""); r.StatusCode != 200 {
		t.Errorf("HEAD /docs = %d", r.StatusCode)
	}
	if r, _ := get(t, "POST", ts.URL+"/docs", ""); r.StatusCode != 405 {
		t.Errorf("POST /docs = %d", r.StatusCode)
	}
}

// An exact version that the program knows: the page carries the hashes, so the browser refuses a file that is not that version.
func TestDocsPageOfAnExactVersionHasIntegrity(t *testing.T) {
	_, m := setup(t, false, "bob")
	const base = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.33.1"
	if !client.DocsUIHasIntegrity(base) {
		t.Fatal("the exact version is not known")
	}
	ts := httptest.NewServer(m.HandlerWith(client.HandlerOptions{Docs: true, DocsUI: base + "/"}))
	defer ts.Close()
	_, page := get(t, "GET", ts.URL+"/docs", "")
	text := string(page)
	for _, want := range []string{
		`href="` + base + `/swagger-ui.css" integrity="sha384-`, `src="` + base + `/swagger-ui-bundle.js" integrity="sha384-`, `crossorigin="anonymous"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page lacks %s:\n%s", want, text)
		}
	}
	if strings.Count(text, "integrity=") != 2 {
		t.Errorf("%d integrity attributes, want 2", strings.Count(text, "integrity="))
	}
}

// With a location of its own the page loads from there, without hashes (the files are not known), and the policy names that place.
func TestDocsPageWithAnExternalLocation(t *testing.T) {
	_, m := setup(t, false, "bob")
	ts := httptest.NewServer(m.HandlerWith(client.HandlerOptions{Docs: true, DocsUI: "https://swagger.corp.example:8443/ui/"}))
	defer ts.Close()

	resp, page := get(t, "GET", ts.URL+"/docs", "")
	text := string(page)
	if !strings.Contains(text, `href="https://swagger.corp.example:8443/ui/swagger-ui.css"`) || !strings.Contains(text, `src="https://swagger.corp.example:8443/ui/swagger-ui-bundle.js"`) {
		t.Errorf("the page does not load from the given place:\n%s", text)
	}
	if strings.Contains(text, "integrity=") || strings.Contains(text, "jsdelivr") {
		t.Errorf("the page has hashes of the default files or names the CDN:\n%s", text)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://swagger.corp.example:8443;") || strings.Contains(csp, "jsdelivr") {
		t.Errorf("the policy = %s", csp)
	}
}

func TestDocsAreOpenButTheAPIIsNot(t *testing.T) {
	_, m := setup(t, false, "bob")
	on := httptest.NewServer(m.HandlerWith(client.HandlerOptions{Token: "tok", Docs: true}))
	defer on.Close()
	off := httptest.NewServer(m.HandlerWith(client.HandlerOptions{Token: "tok"}))
	defer off.Close()

	// switched on: a browser opens the page and the page loads the description, with no token
	for _, path := range []string{"/docs", "/docs/docs-init.js", "/openapi.json"} {
		if r, _ := get(t, "GET", on.URL+path, ""); r.StatusCode != 200 {
			t.Errorf("%s without a token = %d, want 200 with the page on", path, r.StatusCode)
		}
	}
	// but the requests of the API need the token, also the ones that the page makes
	for _, path := range []string{"/accounts", "/accounts/bob", "/accounts/bob/folders", "/accounts/bob/contacts.vcf"} {
		if r, _ := get(t, "GET", on.URL+path, ""); r.StatusCode != 401 {
			t.Errorf("%s without a token = %d, want 401", path, r.StatusCode)
		}
	}
	if r, _ := get(t, "GET", on.URL+"/accounts", "tok"); r.StatusCode != 200 {
		t.Errorf("/accounts with the token = %d", r.StatusCode)
	}

	// switched off: no page, and the description needs the token
	if r, b := get(t, "GET", off.URL+"/docs", "tok"); r.StatusCode != 404 || !strings.Contains(string(b), "not_found") {
		t.Errorf("/docs with the page off = %d %s", r.StatusCode, b)
	}
	if r, _ := get(t, "GET", off.URL+"/docs", ""); r.StatusCode != 401 {
		t.Errorf("/docs with the page off and no token = %d, want 401 (nothing is told)", r.StatusCode)
	}
	if r, _ := get(t, "GET", off.URL+"/openapi.json", ""); r.StatusCode != 401 {
		t.Errorf("/openapi.json with the page off and no token = %d", r.StatusCode)
	}
	r, body := get(t, "GET", off.URL+"/openapi.json", "tok")
	var spec map[string]any
	if r.StatusCode != 200 || json.Unmarshal(body, &spec) != nil || spec["openapi"] != "3.0.3" {
		t.Errorf("the description with the token = %d %.80s", r.StatusCode, body)
	}
	if string(body) != encoded(t, client.OpenAPI()) {
		t.Error("the description of the API differs from client.OpenAPI()")
	}
}
