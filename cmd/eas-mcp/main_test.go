package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// session is an MCP client connected to the server of a manager that works on the mock ActiveSync server.
func session(t *testing.T, send bool, domains []string) (*mcp.ClientSession, *easmockserver.Server) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{{Name: "default", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}},
		client.Options{AllowSend: send, SendDomains: domains})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(m, send, domains, 0).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, srv
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (text string, isError bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestTheToolsThatAreThere(t *testing.T) {
	cs, _ := session(t, false, nil)
	if got := strings.Join(toolNames(t, cs), " "); got != "accounts get_message list_folders list_messages server_info" {
		t.Errorf("without -send: %s", got)
	}
	cs, _ = session(t, true, nil)
	if got := strings.Join(toolNames(t, cs), " "); got != "accounts get_message list_folders list_messages send_message server_info" {
		t.Errorf("with -send: %s", got)
	}
}

func TestTheReadingTools(t *testing.T) {
	cs, srv := session(t, false, nil)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "Hello from Alice", From: "alice@example.com", To: "bob@example.com", Body: "the text of the mail"})

	if out, bad := call(t, cs, "accounts", nil); bad || !strings.Contains(out, `"default"`) || !strings.Contains(out, "bob@example.com") {
		t.Errorf("accounts: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "server_info", nil); bad || !strings.Contains(out, "protocol_version") {
		t.Errorf("server_info: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "list_folders", nil); bad || !strings.Contains(strings.ToLower(out), "inbox") {
		t.Errorf("list_folders: %v %s", bad, out)
	}
	list, bad := call(t, cs, "list_messages", map[string]any{"preview": 100})
	if bad || !strings.Contains(list, "Hello from Alice") || !strings.Contains(list, id) {
		t.Errorf("list_messages: %v %s", bad, list)
	}
	if out, bad := call(t, cs, "get_message", map[string]any{"id": id}); bad || !strings.Contains(out, "the text of the mail") {
		t.Errorf("get_message: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "get_message", map[string]any{}); !bad {
		t.Errorf("get_message without an id: %s", out)
	}
}

func TestSendingIsLimitedToTheDomainsOnTheList(t *testing.T) {
	cs, _ := session(t, true, []string{"example.com"})
	if out, bad := call(t, cs, "send_message", map[string]any{"to": []string{"x@evil.org"}, "subject": "s", "body": "b"}); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("a domain that is not on the list: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "send_message", map[string]any{"to": []string{"carol@example.com"}, "subject": "hello", "body": "text"}); bad || !strings.Contains(out, `"sent":true`) {
		t.Errorf("a domain on the list: %v %s", bad, out)
	}
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "send_message" && !strings.Contains(tool.Description, "example.com") {
			t.Errorf("the description does not name the domains: %s", tool.Description)
		}
	}
}

// Some MCP clients refuse "type": ["null", "array"]: every property of every tool has one type.
func TestTheSchemasHaveASingleTypeForEveryProperty(t *testing.T) {
	cs, _ := session(t, true, nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Type any `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		for name, p := range schema.Properties {
			if _, ok := p.Type.(string); !ok {
				t.Errorf("%s.%s: type is %v, not a single type", tool.Name, name, p.Type)
			}
		}
	}
}
