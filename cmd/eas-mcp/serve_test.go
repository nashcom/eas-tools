package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/internal/ipacl"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// withToken is an HTTP client that sends the bearer token.
type withToken struct{ token string }

func (w withToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if w.token != "" {
		r.Header.Set("Authorization", "Bearer "+w.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// endpoint serves the MCP endpoint of a manager on the mock ActiveSync server, the way the transport "http" does.
func endpoint(t *testing.T, token string, allow *ipacl.List) *httptest.Server {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{{Name: "default", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	api := httptest.NewServer(httpHandler(newServer(m, false, nil, 0), token, allow, false, quiet()))
	t.Cleanup(api.Close)
	return api
}

func TestTheHTTPEndpointServesTheTools(t *testing.T) {
	api := endpoint(t, "s3cret", nil)
	ctx := context.Background()
	tr := &mcp.StreamableClientTransport{Endpoint: api.URL + "/mcp", HTTPClient: &http.Client{Transport: withToken{"s3cret"}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_folders"})
	if err != nil || res.IsError {
		t.Fatalf("list_folders: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(strings.ToLower(text), "inbox") {
		t.Errorf("no inbox in %s", text)
	}
}

func TestTheHTTPEndpointNeedsTheToken(t *testing.T) {
	api := endpoint(t, "s3cret", nil)
	for name, auth := range map[string]string{"none": "", "wrong": "Bearer nope", "no scheme": "s3cret"} {
		req, _ := http.NewRequest("POST", api.URL+"/mcp", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, resp.StatusCode)
		}
	}
	// /health needs no token, and the token is not taken from the URL
	if resp, err := http.Get(api.URL + "/health"); err != nil || resp.StatusCode != 200 {
		t.Errorf("health: %v %v", resp, err)
	}
	if resp, err := http.Post(api.URL+"/mcp?token=s3cret", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token in the URL: %v %v", resp, err)
	}
}

func TestTheHealthListenerHasHealthOnly(t *testing.T) {
	h := httptest.NewServer(httpHandler(mcp.NewServer(&mcp.Implementation{Name: "x", Version: "1"}, nil), "", nil, true, quiet()))
	defer h.Close()
	if resp, err := http.Get(h.URL + "/health"); err != nil || resp.StatusCode != 200 {
		t.Errorf("health: %v %v", resp, err)
	}
	if resp, err := http.Post(h.URL+"/mcp", "application/json", strings.NewReader(`{}`)); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("/mcp on the health listener: %v %v", resp, err)
	}
}

func TestThePeersThatAreNotInTheListAreRefused(t *testing.T) {
	acl, err := ipacl.Parse([]string{"10.1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	api := endpoint(t, "", acl)
	// the test client is on the loopback interface, which is always allowed
	if resp, err := http.Get(api.URL + "/health"); err != nil || resp.StatusCode != 200 {
		t.Errorf("loopback: %v %v", resp, err)
	}
}

func TestTheDefaultPorts(t *testing.T) {
	for _, c := range []struct {
		given     string
		allowHTTP bool
		want      string
	}{{"", false, ":8445"}, {"", true, "127.0.0.1:8081"}, {":9999", false, ":9999"}, {"10.0.0.1:8081", true, "10.0.0.1:8081"}} {
		if got := defaultListen(c.given, c.allowHTTP); got != c.want {
			t.Errorf("%+v: %s, want %s", c, got, c.want)
		}
	}
}
