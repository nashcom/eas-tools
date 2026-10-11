package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

type env struct {
	srv *easmockserver.Server
	url string
}

func start(t *testing.T) env {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Setenv("EASCLIENT_PASSWORD", easmockserver.Password)
	return env{srv, ts.URL}
}

// eas runs the command line for the account "bob" of the mock, in its own state.
func (e env) eas(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	base := []string{"-server", e.url, "-email", "bob@example.com", "-user", "bob", "-no-state"}
	var out, errb bytes.Buffer
	code = run(append(base, args...), strings.NewReader(stdin), &out, &errb, func() []string { return os.Environ() })
	return code, out.String(), errb.String()
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// The command line and the REST API print the same bytes for the same operation.
func TestCommandLineAndRESTPrintTheSameJSON(t *testing.T) {
	e := start(t)
	id := e.srv.Seed("bob", easmockserver.Mail{Subject: "one", Body: "text"})

	m, err := client.NewManager([]client.Account{{Name: "default", Server: e.url, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	api := httptest.NewServer(m.Handler(""))
	defer api.Close()

	for _, c := range []struct {
		path string
		args []string
	}{
		{"/accounts", []string{"accounts"}},
		{"/accounts/default", []string{"info"}},
		{"/accounts/default/folders", []string{"folders"}},
		{"/accounts/default/folders/inbox/messages", []string{"list", "inbox"}},
		{"/accounts/default/folders/inbox/messages?limit=1&preview=2", []string{"list", "inbox", "-limit", "1", "-preview", "2"}},
		{"/accounts/default/folders/inbox/messages/" + id, []string{"get", "inbox", id}},
	} {
		code, out, errs := e.eas(t, "", c.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", c.args, code, errs)
		}
		if want := get(t, api.URL+c.path); out != want {
			t.Errorf("%v printed\n%s\nthe API answers\n%s", c.args, out, want)
		}
	}

	// an error has the same body in both
	code, out, errs := e.eas(t, "", "list", "nothing")
	want := get(t, api.URL+"/accounts/default/folders/nothing/messages")
	if code != 1 || out != "" || errs != want {
		t.Errorf("error: exit %d, stdout %q, stderr\n%s\nthe API answers\n%s", code, out, errs, want)
	}
}

func TestWritesNeedTheOption(t *testing.T) {
	e := start(t)
	code, _, errs := e.eas(t, "", "send", "-to", "x@example.com", "-subject", "s", "-body", "b")
	var body client.ErrorBody
	if json.Unmarshal([]byte(errs), &body) != nil || code != 1 || body.Error.Code != "send_disabled" {
		t.Errorf("send without -send: exit %d, %s", code, errs)
	}
	if e.srv.SentCount() != 0 {
		t.Error("a refused send reached the server")
	}
}

func TestSendReadMoveDeleteFromTheCommandLine(t *testing.T) {
	e := start(t)
	// bob sends to himself: he is a user of the mock after his first command
	code, out, errs := e.eas(t, "the text from stdin", "-send", "send", "-to", "bob@example.com", "-subject", "self", "-body-stdin")
	if code != 0 || !strings.Contains(out, `"sent": true`) {
		t.Fatalf("send: exit %d, %s %s", code, out, errs)
	}
	box := e.srv.Messages("bob", "5")
	if len(box) != 1 || !strings.Contains(box[0].Body, "the text from stdin") {
		t.Fatalf("Inbox = %+v", box)
	}

	_, out, _ = e.eas(t, "", "list", "inbox")
	var l client.ListResult
	if err := json.Unmarshal([]byte(out), &l); err != nil || l.Count != 1 {
		t.Fatalf("list: %v %s", err, out)
	}
	id := l.Messages[0].ID

	if code, out, errs := e.eas(t, "", "-write", "read", "inbox", id); code != 0 || !strings.Contains(out, `"action": "read"`) {
		t.Fatalf("read: %d %s %s", code, out, errs)
	}
	code, out, errs = e.eas(t, "", "-write", "move", "inbox", id, "archive")
	var moved client.ChangeResult
	if err := json.Unmarshal([]byte(out), &moved); err != nil || code != 0 || moved.NewID == "" {
		t.Fatalf("move: %d %s %s", code, out, errs)
	}
	if code, _, errs := e.eas(t, "", "-write", "delete", "archive", moved.NewID); code != 0 {
		t.Fatalf("delete: %d %s", code, errs)
	}
	if n := len(e.srv.Messages("bob", "7")); n != 1 {
		t.Errorf("%d mails in Deleted Items, want 1", n)
	}
}

func TestUsageErrorsExitWith2AndPrintNoJSON(t *testing.T) {
	e := start(t)
	for _, args := range [][]string{
		{},
		{"nothing"},
		{"list"},
		{"list", "inbox", "-limit", "0"},
		{"get"},
		{"get", "a", "b", "c"},
		{"move", "5:1"},
		{"-write", "send", "-to", "a@b.c", "-subject", "s", "-body", "x", "-body-file", "f"},
		{"-write", "send", "extra"},
		{"-nosuchoption"},
	} {
		code, out, errs := e.eas(t, "", args...)
		if code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 2, nothing, a text", args, code, out, errs)
		}
	}
	// no account at all
	var out, errb bytes.Buffer
	if code := run([]string{"folders"}, strings.NewReader(""), &out, &errb, func() []string { return nil }); code != 2 {
		t.Errorf("no account: exit %d, %s", code, errb.String())
	}
}

func TestAccountsFromTheConfigFile(t *testing.T) {
	e := start(t)
	cfg := filepath.Join(t.TempDir(), "client.json")
	doc := `{"no_state": true, "accounts": [
	  {"name": "bob",   "server": "` + e.url + `", "email": "bob@example.com",   "user": "bob",   "password_env": "EASCLIENT_PASSWORD"},
	  {"name": "carol", "server": "` + e.url + `", "email": "carol@example.com", "user": "carol", "password_env": "EASCLIENT_PASSWORD"}]}`
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.Seed("carol", easmockserver.Mail{Subject: "for carol"})

	run1 := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(append([]string{"-config", cfg}, args...), strings.NewReader(""), &out, &errb, os.Environ)
		return code, out.String(), errb.String()
	}
	code, out, _ := run1("accounts")
	if code != 0 || !strings.Contains(out, `"carol"`) || strings.Contains(out, "password") {
		t.Errorf("accounts: %d %s", code, out)
	}
	code, _, errs := run1("folders") // two accounts: one has to be named
	if code != 1 || !strings.Contains(errs, "account_required") {
		t.Errorf("no -account: %d %s", code, errs)
	}
	code, out, errs = run1("-account", "carol", "list", "inbox")
	if code != 0 || !strings.Contains(out, "for carol") {
		t.Errorf("-account carol: %d %s %s", code, out, errs)
	}
}

func TestServeNeedsATokenOffTheLoopback(t *testing.T) {
	e := start(t)
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0"} {
		code, _, errs := e.eas(t, "", "-listen", addr, "serve")
		_ = code
		if !strings.Contains(errs, "token") {
			t.Errorf("serve -listen %s: %s", addr, errs)
		}
	}
}

func TestReplyAndForwardFromTheCommandLine(t *testing.T) {
	e := start(t)
	// carol and dave have to be users of the mock to get mail
	for _, u := range []string{"carol", "dave"} {
		var out, errb bytes.Buffer
		run([]string{"-server", e.url, "-email", u + "@example.com", "-user", u, "-no-state", "info"}, strings.NewReader(""), &out, &errb, os.Environ)
	}
	id := e.srv.Seed("bob", easmockserver.Mail{Subject: "topic", From: "carol@example.com", To: "bob@example.com", Body: "orig"})

	if code, _, errs := e.eas(t, "", "-write", "reply", "inbox", id, "-body", "x"); code != 1 || !strings.Contains(errs, "send_disabled") {
		t.Errorf("reply without -send: %d %s", code, errs)
	}
	code, out, errs := e.eas(t, "my reply", "-send", "reply", "inbox", id, "-body-stdin")
	if code != 0 || !strings.Contains(out, `"action": "reply"`) || len(e.srv.Messages("carol", "5")) != 1 {
		t.Fatalf("reply: %d %s %s", code, out, errs)
	}
	code, out, errs = e.eas(t, "", "-send", "forward", "inbox", id, "-to", "dave@example.com", "-body", "fyi", "-no-sent-copy")
	if code != 0 || !strings.Contains(out, `"action": "forward"`) || len(e.srv.Messages("dave", "5")) != 1 {
		t.Fatalf("forward: %d %s %s", code, out, errs)
	}
	for _, args := range [][]string{{"reply"}, {"forward"}, {"-send", "forward", "inbox", id, "extra", "-to", "a@b.c"}} {
		if code, out, errs := e.eas(t, "", args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, %q %q; want a usage error", args, code, out, errs)
		}
	}
}

func TestCommandsWithoutAFolder(t *testing.T) {
	e := start(t)
	id := e.srv.Seed("bob", easmockserver.Mail{Subject: "short", From: "bob@example.com", To: "bob@example.com", Body: "t"})

	with, _, _ := e.eas(t, "", "get", "inbox", id)
	code, out, errs := e.eas(t, "", "get", id)
	if with != 0 || code != 0 || !strings.Contains(out, `"subject": "short"`) {
		t.Fatalf("get ID: %d %s %s", code, out, errs)
	}
	if code, out, errs := e.eas(t, "", "-write", "move", id, "archive"); code != 0 || !strings.Contains(out, `"action": "move"`) {
		t.Fatalf("move ID DEST: %d %s %s", code, out, errs)
	}
	if n := len(e.srv.Messages("bob", "9")); n != 1 {
		t.Errorf("%d mails in Archive, want 1", n)
	}
	if code, _, errs := e.eas(t, "", "get", "nocolon"); code != 1 || !strings.Contains(errs, "folder_required") {
		t.Errorf("get nocolon: %d %s", code, errs)
	}
}

func TestEMLPrintsTheRawMessage(t *testing.T) {
	e := start(t)
	id := e.srv.Seed("bob", easmockserver.Mail{Subject: "to a file", From: "carol@example.com", Body: "the stream"})
	code, out, errs := e.eas(t, "", "eml", id)
	if code != 0 || !strings.Contains(out, "Subject: to a file") || !strings.HasSuffix(out, "the stream") || strings.HasPrefix(out, "{") {
		t.Fatalf("eml: %d %q %s", code, out, errs)
	}
	if code, out, errs := e.eas(t, "", "eml", "5:99999"); code != 1 || out != "" || !strings.Contains(errs, "mail_not_found") {
		t.Errorf("unknown mail: %d %q %s", code, out, errs)
	}
}

func TestCalendarFromTheCommandLineMatchesREST(t *testing.T) {
	e := start(t)
	now := time.Now().Truncate(time.Second)
	id := e.srv.SeedEvent("bob", easmockserver.Event{UID: "u", Subject: "cli event", Location: "here", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)})

	m, err := client.NewManager([]client.Account{{Name: "default", Server: e.url, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	api := httptest.NewServer(m.Handler(""))
	defer api.Close()

	for _, c := range []struct {
		path string
		args []string
	}{
		{"/accounts/default/calendar/events?from=2000-01-01&limit=all", []string{"calendar", "list", "-from", "2000-01-01", "-limit", "all"}},
		{"/accounts/default/calendar/events/" + id, []string{"calendar", "get", id}},
		{"/accounts/default/folders/8/events/" + id, []string{"calendar", "get", "8", id}},
	} {
		code, out, errs := e.eas(t, "", c.args...)
		if code != 0 || !strings.Contains(out, "cli event") {
			t.Fatalf("%v: %d %s %s", c.args, code, out, errs)
		}
		if want := get(t, api.URL+c.path); out != want {
			t.Errorf("%v printed\n%s\nthe API answers\n%s", c.args, out, want)
		}
	}
	for _, args := range [][]string{{"calendar"}, {"calendar", "nothing"}, {"calendar", "list", "a", "b"}, {"calendar", "get"}, {"calendar", "list", "-limit", "0"}} {
		if code, out, errs := e.eas(t, "", args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, %q %q; want a usage error", args, code, out, errs)
		}
	}
}

func TestCalendarICSPrintsTheFile(t *testing.T) {
	e := start(t)
	now := time.Now().Truncate(time.Second)
	id := e.srv.SeedEvent("bob", easmockserver.Event{UID: "u-cli", Subject: "cli ics", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)})
	for _, args := range [][]string{{"calendar", "ics", id}, {"calendar", "ics", "8", id}} {
		code, out, errs := e.eas(t, "", args...)
		if code != 0 || !strings.HasPrefix(out, "BEGIN:VCALENDAR\r\n") || !strings.Contains(out, "SUMMARY:cli ics\r\n") || !strings.HasSuffix(out, "END:VCALENDAR\r\n") {
			t.Errorf("%v: %d %q %s", args, code, out, errs)
		}
	}
	if code, out, errs := e.eas(t, "", "calendar", "ics", "8:99999"); code != 1 || out != "" || !strings.Contains(errs, "event_not_found") {
		t.Errorf("unknown event: %d %q %s", code, out, errs)
	}
}
