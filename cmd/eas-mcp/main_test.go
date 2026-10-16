package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// session is an MCP client connected to the server of a manager that works on the mock ActiveSync server.
func session(t *testing.T, send bool, domains []string) (*mcp.ClientSession, *easmockserver.Server) {
	t.Helper()
	return sessionWith(t, serverOptions{Send: send, SendDomains: domains})
}

func sessionWith(t *testing.T, o serverOptions) (*mcp.ClientSession, *easmockserver.Server) {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	m, err := client.NewManager([]client.Account{{Name: "default", Server: ts.URL, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}},
		client.Options{AllowSend: o.Send, AllowWrite: o.Write, SendDomains: o.SendDomains})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)

	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := newServer(m, o).Connect(ctx, st, nil)
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
	if got := strings.Join(toolNames(t, cs), " "); got != "accounts export_contacts get_contact get_contact_vcard get_event get_event_ics get_message get_message_eml list_contacts list_events list_folders list_messages server_info" {
		t.Errorf("without -send: %s", got)
	}
	cs, _ = session(t, true, nil)
	if got := strings.Join(toolNames(t, cs), " "); got != "accounts export_contacts forward_message get_contact get_contact_vcard get_event get_event_ics get_message get_message_eml list_contacts list_events list_folders list_messages reply_message send_message server_info" {
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
	cs, _ := sessionWith(t, serverOptions{Send: true, Write: true})
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

func TestTheToolsOfSendingAndWritingAreThereOnlyWithTheirSwitches(t *testing.T) {
	read := "accounts export_contacts get_contact get_contact_vcard get_event get_event_ics get_message get_message_eml list_contacts list_events list_folders list_messages server_info"
	for name, c := range map[string]struct {
		o    serverOptions
		want string
	}{
		"read only":      {serverOptions{}, read},
		"send":           {serverOptions{Send: true}, "forward_message reply_message send_message " + read},
		"write":          {serverOptions{Write: true}, "create_event delete_event delete_message mark_read mark_unread move_message " + read},
		"send and write": {serverOptions{Send: true, Write: true}, "create_event delete_event delete_message forward_message mark_read mark_unread move_message reply_message send_message " + read},
	} {
		cs, _ := sessionWith(t, c.o)
		got := toolNames(t, cs)
		want := strings.Fields(c.want)
		sort.Strings(want)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: %s\nwant %s", name, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}

func TestChangingAMailbox(t *testing.T) {
	cs, srv := sessionWith(t, serverOptions{Write: true})
	id := srv.Seed("bob", easmockserver.Mail{Subject: "to move", From: "alice@example.com", To: "bob@example.com", Body: "text"})
	if out, bad := call(t, cs, "mark_read", map[string]any{"id": id}); bad || !strings.Contains(out, `"read"`) {
		t.Errorf("mark_read: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "mark_unread", map[string]any{"id": id}); bad || !strings.Contains(out, `"unread"`) {
		t.Errorf("mark_unread: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "delete_message", map[string]any{"id": id}); bad || !strings.Contains(out, `"delete"`) {
		t.Errorf("delete_message: %v %s", bad, out)
	}
}

func TestReplyAndForwardAreLimitedToTheDomainsOnTheList(t *testing.T) {
	cs, srv := sessionWith(t, serverOptions{Send: true, SendDomains: []string{"example.com"}})
	inside := srv.Seed("bob", easmockserver.Mail{Subject: "from a friend", From: "alice@example.com", To: "bob@example.com", Body: "hello"})
	outside := srv.Seed("bob", easmockserver.Mail{Subject: "from outside", From: "mallory@evil.org", To: "bob@example.com", Body: "hello"})

	if out, bad := call(t, cs, "reply_message", map[string]any{"id": inside, "body": "thanks"}); bad || !strings.Contains(out, `"sent":true`) {
		t.Errorf("reply inside the list: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "reply_message", map[string]any{"id": outside, "body": "thanks"}); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("reply to a domain that is not on the list: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "forward_message", map[string]any{"id": inside, "to": []string{"x@evil.org"}}); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("forward to a domain that is not on the list: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "forward_message", map[string]any{"id": inside, "to": []string{"carol@example.com"}, "body": "see below"}); bad || !strings.Contains(out, `"sent":true`) {
		t.Errorf("forward inside the list: %v %s", bad, out)
	}
}

func TestTheMessageInEMLForm(t *testing.T) {
	cs, srv := session(t, false, nil)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "Headers please", From: "alice@example.com", To: "bob@example.com", Body: "the body text of the mail"})

	full, bad := call(t, cs, "get_message_eml", map[string]any{"id": id})
	if bad || !strings.Contains(full, "Subject: Headers please") || !strings.Contains(full, "alice@example.com") || !strings.Contains(full, "the body text of the mail") {
		t.Fatalf("the whole message: %v\n%s", bad, full)
	}
	head, bad := call(t, cs, "get_message_eml", map[string]any{"id": id, "headers_only": true})
	if bad || !strings.Contains(head, "Subject: Headers please") || strings.Contains(head, "the body text of the mail") {
		t.Errorf("the headers only: %v\n%s", bad, head)
	}
	cut, bad := call(t, cs, "get_message_eml", map[string]any{"id": id, "max_bytes": 30})
	if bad || !strings.Contains(cut, "the text is cut here: 30 of ") || strings.Contains(cut, "the body text of the mail") {
		t.Errorf("a short limit: %v\n%s", bad, cut)
	}
}

func TestEMLTextCutsAtTheLimitsAndKeepsUTF8(t *testing.T) {
	data := []byte("Subject: x\r\n\r\n" + strings.Repeat("ä", 10))
	if got := emlText(data, true, 0); got != "Subject: x\r\n\r\n" {
		t.Errorf("headers: %q", got)
	}
	if got := emlText(data, false, 0); got != string(data) {
		t.Errorf("whole: %q", got)
	}
	got := emlText(data, false, 15) // cuts in the middle of a two byte character
	if !strings.Contains(got, "cut here: 15 of 34 bytes") || !strings.HasPrefix(got, "Subject: x\r\n\r\n") {
		t.Errorf("cut: %q", got)
	}
	for _, r := range got {
		if r == '�' {
			return // the half character became the replacement character, the text is valid UTF-8
		}
	}
	t.Errorf("the half character was not replaced: %q", got)
}

// What a tool does is in the log: a failure with its error (it goes to the client otherwise, and nobody sees it on the server), a success as a
// line. The arguments, which hold the text of mails, are not.
func TestTheCallsOfTheToolsAreInTheLog(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{&buf, &mu}, nil))
	cs, _ := sessionWith(t, serverOptions{Send: true, SendDomains: []string{"example.com"}, Log: log})
	read := func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }

	call(t, cs, "list_folders", nil)
	if out := read(); !strings.Contains(out, "level=INFO") || !strings.Contains(out, "a tool was called") || !strings.Contains(out, "tool=list_folders") || !strings.Contains(out, "duration=") {
		t.Errorf("a tool that works:\n%s", out)
	}
	call(t, cs, "get_message", map[string]any{"id": "99:99999"})
	if out := read(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "a tool call failed") || !strings.Contains(out, "tool=get_message") || !strings.Contains(out, "error=") {
		t.Errorf("a tool that fails:\n%s", out)
	}
	call(t, cs, "send_message", map[string]any{"to": []string{"x@evil.org"}, "subject": "s", "body": "the secret text of the mail"})
	out := read()
	if !strings.Contains(out, "tool=send_message") || !strings.Contains(out, "not allowed") {
		t.Errorf("a send that is refused:\n%s", out)
	}
	if strings.Contains(out, "the secret text of the mail") || strings.Contains(out, "x@evil.org") && !strings.Contains(out, "mail to evil.org is not allowed") {
		t.Errorf("the arguments are in the log:\n%s", out)
	}
}
