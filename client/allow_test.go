package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/ipacl"
)

func TestAllowGuardsEverythingOfTheAPI(t *testing.T) {
	_, m := setup(t, false, "bob")
	acl, err := ipacl.Parse([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	h := m.HandlerWith(client.HandlerOptions{Token: "tok", Docs: true, Allow: acl, OnDenied: func(remote string, r *http.Request) { refused = append(refused, r.URL.Path) }})

	call := func(path, remote, token string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// from outside nothing is served, not even what needs no token: the check comes before everything
	for _, p := range []string{"/health", "/docs", "/docs/docs-init.js", "/openapi.json", "/accounts", "/accounts/bob/folders", "/nothing"} {
		if code := call(p, "203.0.113.9:1", "tok"); code != 403 {
			t.Errorf("%s from outside = %d, want 403 (also with the token)", p, code)
		}
	}
	if len(refused) != 7 || refused[0] != "/health" {
		t.Errorf("the refusals that were reported: %v", refused)
	}
	// from the list the rules of the API go on: the token is still needed
	if code := call("/health", "10.1.2.3:1", ""); code != 200 {
		t.Errorf("/health from the list = %d", code)
	}
	if code := call("/accounts", "10.1.2.3:1", ""); code != 401 {
		t.Errorf("/accounts from the list without the token = %d, want 401", code)
	}
	if code := call("/accounts", "10.1.2.3:1", "tok"); code != 200 {
		t.Errorf("/accounts from the list with the token = %d", code)
	}
	// loopback is always allowed
	if code := call("/health", "127.0.0.1:1", ""); code != 200 {
		t.Errorf("/health from loopback = %d", code)
	}
}

func TestNoAllowMeansNoRestriction(t *testing.T) {
	_, m := setup(t, false, "bob")
	h := m.HandlerWith(client.HandlerOptions{})
	req := httptest.NewRequest("GET", "/health", nil)
	req.RemoteAddr = "203.0.113.9:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("no list: %d %s", rec.Code, rec.Body.String())
	}
}
