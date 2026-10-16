package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// record is one line of the audit log: a log record of OpenTelemetry's data model.
type record struct {
	Severity   string         `json:"severity_text"`
	Body       string         `json:"body"`
	Time       string         `json:"time_unix_nano"`
	Scope      map[string]any `json:"scope"`
	Resource   map[string]any `json:"resource"`
	Attributes map[string]any `json:"attributes"`
}

func records(t *testing.T, path string) []record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []record
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("a line of the audit log is not JSON: %v\n%s", err, line)
		}
		out = append(out, r)
	}
	return out
}

func find(rs []record, name, key, value string) *record {
	for i := range rs {
		if rs[i].Attributes["event.name"] == name && (key == "" || rs[i].Attributes[key] == value) {
			return &rs[i]
		}
	}
	return nil
}

func TestTheAuditLogHasWhatWasDoneAndNotTheText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	aud, err := newAuditor(path, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(aud.close)
	cs, _ := sessionWith(t, serverOptions{Send: true, SendDomains: []string{"example.com"}, Audit: aud})

	call(t, cs, "list_folders", nil)
	call(t, cs, "get_message", map[string]any{"id": "99:99999"})
	call(t, cs, "send_message", map[string]any{"to": []string{"x@evil.org"}, "cc": []string{"y@evil.org"}, "subject": "the subject line", "body": "the secret text of the mail"})
	call(t, cs, "send_message", map[string]any{"to": []string{"carol@example.com"}, "subject": "hello", "body": "text"})

	rs := records(t, path)
	if len(rs) != 4 {
		t.Fatalf("%d records, want 4: %+v", len(rs), rs)
	}
	for _, r := range rs { // the shape of the log data model of OpenTelemetry
		if r.Time == "" || r.Severity == "" || r.Body == "" || r.Resource["service.name"] != "eas-mcp-audit" || r.Resource["host.name"] == nil {
			t.Errorf("not a log record of OpenTelemetry: %+v", r)
		}
	}
	if r := find(rs, "mcp.tool.call", "mcp.tool", "list_folders"); r == nil || r.Attributes["outcome"] != "success" || r.Severity != "INFO" || r.Attributes["rpc.method"] != "tools/call" {
		t.Errorf("a tool that works: %+v", r)
	}
	if r := find(rs, "mcp.tool.call", "mcp.tool", "get_message"); r == nil || r.Attributes["outcome"] != "failure" || r.Severity != "WARN" || r.Attributes["error.message"] == nil || r.Attributes["mcp.id"] != "99:99999" {
		t.Errorf("a tool that fails: %+v", r)
	}
	var sendRefused *record
	for i := range rs {
		if rs[i].Attributes["mcp.tool"] == "send_message" && rs[i].Attributes["outcome"] == "failure" {
			sendRefused = &rs[i]
		}
	}
	if sendRefused == nil || !strings.Contains(sendRefused.Attributes["error.message"].(string), "not allowed") {
		t.Fatalf("a send that is refused: %+v", sendRefused)
	}
	if to, _ := sendRefused.Attributes["mail.to"].([]any); len(to) != 1 || to[0] != "x@evil.org" {
		t.Errorf("the recipients of a refused mail are not in the audit log: %+v", sendRefused.Attributes)
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"the secret text of the mail", "the subject line"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("%q is in the audit log", secret)
		}
	}
}

func TestTheAuditLogIsOpenedAgainAfterTheRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")
	aud, err := newAuditor(path, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(aud.close)
	aud.event(context.Background(), slog.LevelInfo, "t.one", "before")
	if err := os.Rename(path, path+".1"); err != nil { // what logrotate does
		t.Skip("the file cannot be renamed while it is open on this system")
	}
	if err := aud.reopen(); err != nil {
		t.Fatal(err)
	}
	aud.event(context.Background(), slog.LevelInfo, "t.two", "after")
	if find(records(t, path+".1"), "t.one", "", "") == nil || find(records(t, path), "t.two", "", "") == nil || find(records(t, path), "t.one", "", "") != nil {
		t.Error("the records are not where the rotation puts them")
	}
	var none *auditor
	none.event(context.Background(), slog.LevelInfo, "nothing", "no audit log") // an audit log that is not there is no error
	if err := none.reopen(); err != nil {
		t.Error(err)
	}
}

func TestTheAuditLogNeedsAPlaceThatCanBeWritten(t *testing.T) {
	if _, err := newAuditor(filepath.Join(t.TempDir(), "no", "such", "dir", "audit.json"), "t", false); err == nil {
		t.Error("an audit log in a folder that is not there was accepted")
	}
	if _, err := newAuditor("stdout", "t", true); err == nil {
		t.Error("stdout is the channel of the protocol with the transport stdio")
	}
	if a, err := newAuditor("", "t", false); a != nil || err != nil {
		t.Error("an empty target means no audit log")
	}
}

func TestTheAuditLogHasWhoCalledAndWhoWasRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	aud, err := newAuditor(path, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(aud.close)
	p := newIdP(t)
	api := oidcEndpointLog(t, p, "", resourceURL, nil, quiet(), func(a *authn) { a.audit = aud })

	// a user that is let in calls a tool: the user and where the request came from are in the record of the call
	tr := &mcp.StreamableClientTransport{Endpoint: api.URL + "/mcp", HTTPClient: &http.Client{Transport: withToken{p.token(t, resourceURL, "alice@example.com")}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders"}); err != nil || res.IsError {
		t.Fatalf("list_folders: %v %+v", err, res)
	}
	// a user that is not on the list, a token for another audience, and a request without a token
	post(t, api.URL+"/mcp", "Bearer "+p.token(t, resourceURL, "mallory@evil.org"))
	post(t, api.URL+"/mcp", "Bearer "+p.token(t, "https://other.example.com/mcp", "alice@example.com"))
	post(t, api.URL+"/mcp", "")
	time.Sleep(20 * time.Millisecond)

	rs := records(t, path)
	call := find(rs, "mcp.tool.call", "mcp.tool", "list_folders")
	if call == nil || call.Attributes["enduser.id"] != "alice@example.com" || call.Attributes["auth.method"] != "oidc" {
		t.Fatalf("the call of an OIDC user: %+v", call)
	}
	if addr, _ := call.Attributes["client.address"].(string); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("the address of the client is not in the record of the call: %+v", call.Attributes)
	}
	var denied []record
	for _, r := range rs {
		if r.Attributes["event.name"] == "mcp.auth.denied" {
			denied = append(denied, r)
		}
	}
	if len(denied) != 2 { // the request without a token is the normal first step of a login and is not audited
		t.Fatalf("%d denials, want 2: %+v", len(denied), denied)
	}
	if denied[0].Attributes["enduser.id"] != "mallory@evil.org" || denied[0].Attributes["outcome"] != "denied" || denied[0].Severity != "WARN" {
		t.Errorf("the user who is not allowed: %+v", denied[0])
	}
	if reason, _ := denied[1].Attributes["error.message"].(string); !strings.Contains(reason, "not for this server") {
		t.Errorf("the token for another audience: %+v", denied[1])
	}
}
