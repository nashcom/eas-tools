package client_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func api(t *testing.T, write bool, token string, accounts ...string) (*easmockserver.Server, *client.Manager, *httptest.Server) {
	t.Helper()
	srv, m := setup(t, write, accounts...)
	ts := httptest.NewServer(m.Handler(token))
	t.Cleanup(ts.Close)
	return srv, m, ts
}

func do(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func encoded(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	if err := client.Encode(&b, v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// What the REST API answers is what the command prints: the same bytes.
func TestRESTAnswersWithTheSameJSONAsTheCommand(t *testing.T) {
	srv, m, ts := api(t, false, "", "bob")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "same", Body: "text"})

	cases := map[string]func() (any, error){
		"/accounts":                                  func() (any, error) { return m.Accounts(), nil },
		"/accounts/bob/folders":                      func() (any, error) { return m.Folders(t.Context(), "bob") },
		"/accounts/bob/folders/inbox/messages":       func() (any, error) { return m.List(t.Context(), "bob", "inbox", client.ListParams{}) },
		"/accounts/bob/folders/inbox/messages/" + id: func() (any, error) { return m.Get(t.Context(), "bob", "inbox", id) },
	}
	for path, f := range cases {
		want, err := f()
		if err != nil {
			t.Fatal(path, err)
		}
		status, got := do(t, "GET", ts.URL+path, "", "")
		if status != 200 || got != encoded(t, want) {
			t.Errorf("GET %s = %d\n%s\nwant\n%s", path, status, got, encoded(t, want))
		}
	}
}

func TestRESTErrorsHaveTheSameBodyAndAStatus(t *testing.T) {
	_, m, ts := api(t, false, "", "bob")

	_, err := m.List(t.Context(), "bob", "nothing", client.ListParams{})
	status, body := do(t, "GET", ts.URL+"/accounts/bob/folders/nothing/messages", "", "")
	if status != 404 || body != encoded(t, client.ErrorBody{Error: client.AsError(err)}) {
		t.Errorf("unknown folder: %d %s", status, body)
	}

	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/accounts/nobody/folders", "", 404, "unknown_account"},
		{"GET", "/nothing", "", 404, "not_found"},
		{"POST", "/accounts/bob/folders", "", 405, "method_not_allowed"},
		{"GET", "/accounts/bob/messages", "", 405, "method_not_allowed"},
		{"POST", "/accounts/bob/messages", `{"to":["a@b.c"],"subject":"s"}`, 403, "send_disabled"},
		{"POST", "/accounts/bob/messages", `{"to":`, 400, "bad_request"},
		{"POST", "/accounts/bob/messages", `{"unknown":1}`, 400, "bad_request"},
		{"GET", "/accounts/bob/folders/inbox/messages?limit=zero", "", 400, "bad_request"},
		{"PATCH", "/accounts/bob/folders/inbox/messages/5:1", `{}`, 400, "bad_request"},
		{"PATCH", "/accounts/bob/folders/inbox/messages/5:1", `{"read":true,"move_to":"sent"}`, 400, "bad_request"},
	} {
		status, body := do(t, c.method, ts.URL+c.path, "", c.body)
		var e client.ErrorBody
		if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error == nil {
			t.Errorf("%s %s: %d, not an error body: %s", c.method, c.path, status, body)
			continue
		}
		if status != c.status || e.Error.Code != c.code {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, status, e.Error.Code, c.status, c.code)
		}
	}
}

func TestRESTTokenIsABearerHeaderOnly(t *testing.T) {
	_, _, ts := api(t, false, "s3cret", "bob")

	if status, _ := do(t, "GET", ts.URL+"/health", "", ""); status != 200 {
		t.Errorf("/health needs no token: %d", status)
	}
	if status, _ := do(t, "GET", ts.URL+"/accounts", "", ""); status != 401 {
		t.Errorf("no token: %d", status)
	}
	if status, _ := do(t, "GET", ts.URL+"/accounts", "wrong", ""); status != 401 {
		t.Errorf("wrong token: %d", status)
	}
	if status, _ := do(t, "GET", ts.URL+"/accounts?auth=s3cret", "", ""); status != 401 {
		t.Errorf("a token in the URL was accepted: %d", status)
	}
	if status, _ := do(t, "GET", ts.URL+"/accounts", "s3cret", ""); status != 200 {
		t.Errorf("right token: %d", status)
	}
}

func TestRESTSendPatchDelete(t *testing.T) {
	srv, m, ts := api(t, true, "", "bob", "carol")
	if _, err := m.Info(t.Context(), "carol"); err != nil {
		t.Fatal(err)
	}

	status, body := do(t, "POST", ts.URL+"/accounts/bob/messages", "", `{"to":["carol@example.com"],"subject":"rest","body":"b"}`)
	if status != 200 || !strings.Contains(body, `"sent": true`) {
		t.Fatalf("send = %d %s", status, body)
	}
	box := srv.Messages("carol", "5")
	if len(box) != 1 {
		t.Fatalf("carol has %d mails", len(box))
	}
	l, _ := m.List(t.Context(), "carol", "inbox", client.ListParams{})
	id := l.Messages[0].ID

	if status, body := do(t, "PATCH", ts.URL+"/accounts/carol/folders/inbox/messages/"+id, "", `{"read":true}`); status != 200 || !strings.Contains(body, `"action": "read"`) {
		t.Errorf("read = %d %s", status, body)
	}
	status, body = do(t, "PATCH", ts.URL+"/accounts/carol/folders/inbox/messages/"+id, "", `{"move_to":"archive"}`)
	var moved client.ChangeResult
	if err := json.Unmarshal([]byte(body), &moved); err != nil || status != 200 || moved.NewID == "" {
		t.Fatalf("move = %d %s", status, body)
	}
	if status, body := do(t, "DELETE", ts.URL+"/accounts/carol/folders/archive/messages/"+moved.NewID, "", ""); status != 200 || !strings.Contains(body, `"action": "delete"`) {
		t.Errorf("delete = %d %s", status, body)
	}
	if n := len(srv.Messages("carol", "7")); n != 1 {
		t.Errorf("%d mails in Deleted Items, want 1", n)
	}
}

func TestRESTReplyAndForward(t *testing.T) {
	srv, m, ts := api(t, true, "", "bob", "carol", "dave")
	for _, n := range []string{"carol", "dave"} {
		if _, err := m.Info(t.Context(), n); err != nil {
			t.Fatal(err)
		}
	}
	id := srv.Seed("bob", easmockserver.Mail{Subject: "rest", From: "carol@example.com", Body: "orig"})

	status, body := do(t, "POST", ts.URL+"/accounts/bob/folders/inbox/messages/"+id+"/reply", "", `{"body":"yes","all":false}`)
	if status != 200 || !strings.Contains(body, `"action": "reply"`) || len(srv.Messages("carol", "5")) != 1 {
		t.Errorf("reply = %d %s", status, body)
	}
	status, body = do(t, "POST", ts.URL+"/accounts/bob/folders/inbox/messages/"+id+"/forward", "", `{"to":["dave@example.com"],"body":"fyi"}`)
	if status != 200 || !strings.Contains(body, `"action": "forward"`) || len(srv.Messages("dave", "5")) != 1 {
		t.Errorf("forward = %d %s", status, body)
	}
	if status, _ := do(t, "GET", ts.URL+"/accounts/bob/folders/inbox/messages/"+id+"/reply", "", ""); status != 405 {
		t.Errorf("GET reply = %d, want 405", status)
	}
	status, body = do(t, "POST", ts.URL+"/accounts/bob/folders/inbox/messages/"+id+"/forward", "", `{"body":"nobody"}`)
	if status != 400 || !strings.Contains(body, "recipient_required") {
		t.Errorf("forward without recipient = %d %s", status, body)
	}
}

func TestRESTPathsWithoutAFolder(t *testing.T) {
	srv, m, ts := api(t, true, "", "bob", "carol")
	if _, err := m.Info(t.Context(), "carol"); err != nil {
		t.Fatal(err)
	}
	id := srv.Seed("bob", easmockserver.Mail{Subject: "nf", From: "carol@example.com", Body: "b"})

	want, _ := m.Get(t.Context(), "bob", "inbox", id)
	status, got := do(t, "GET", ts.URL+"/accounts/bob/messages/"+id, "", "")
	if status != 200 || got != encoded(t, want) {
		t.Errorf("GET = %d\n%s\nwant\n%s", status, got, encoded(t, want))
	}
	if status, body := do(t, "POST", ts.URL+"/accounts/bob/messages/"+id+"/reply", "", `{"body":"yes"}`); status != 200 || !strings.Contains(body, `"action": "reply"`) {
		t.Errorf("reply = %d %s", status, body)
	}
	if status, body := do(t, "POST", ts.URL+"/accounts/bob/messages/"+id+"/forward", "", `{"to":["carol@example.com"],"body":"f"}`); status != 200 {
		t.Errorf("forward = %d %s", status, body)
	}
	if status, body := do(t, "PATCH", ts.URL+"/accounts/bob/messages/"+id, "", `{"read":true}`); status != 200 || !strings.Contains(body, `"action": "read"`) {
		t.Errorf("patch = %d %s", status, body)
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/messages/nocolon", "", ""); status != 400 || !strings.Contains(body, "folder_required") {
		t.Errorf("an id without a folder = %d %s", status, body)
	}
	if status, _ := do(t, "DELETE", ts.URL+"/accounts/bob/messages/"+id, "", ""); status != 200 {
		t.Errorf("delete = %d", status)
	}
}

func TestRESTEMLIsTheRawStream(t *testing.T) {
	srv, m, ts := api(t, false, "", "bob")
	id := srv.Seed("bob", easmockserver.Mail{Subject: "stream", From: "carol@example.com", Body: "bytes"})
	want, err := m.EML(t.Context(), "bob", "inbox", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/accounts/bob/folders/inbox/messages/" + id + "/eml", "/accounts/bob/messages/" + id + "/eml"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(body, want.Data) || resp.Header.Get("Content-Type") != "message/rfc822" {
			t.Errorf("GET %s = %d %q, type %q", path, resp.StatusCode, body, resp.Header.Get("Content-Type"))
		}
	}
	// an error is still the JSON error
	status, body := do(t, "GET", ts.URL+"/accounts/bob/messages/5:99999/eml", "", "")
	if status != 404 || !strings.Contains(body, "mail_not_found") {
		t.Errorf("unknown mail = %d %s", status, body)
	}
}

func TestRESTCalendar(t *testing.T) {
	srv, m, ts := api(t, false, "", "bob")
	now := time.Now().Truncate(time.Second)
	id := srv.SeedEvent("bob", easmockserver.Event{UID: "u", Subject: "rest event", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)})

	for path, f := range map[string]func() (any, error){
		"/accounts/bob/calendar/events?from=2000-01-01&limit=all": func() (any, error) {
			return m.Events(t.Context(), "bob", "", client.EventParams{From: "2000-01-01", Limit: -1})
		},
		"/accounts/bob/folders/calendar/events?from=2000-01-01&limit=all": func() (any, error) {
			return m.Events(t.Context(), "bob", "calendar", client.EventParams{From: "2000-01-01", Limit: -1})
		},
		"/accounts/bob/calendar/events/" + id:         func() (any, error) { return m.Event(t.Context(), "bob", "", id) },
		"/accounts/bob/folders/calendar/events/" + id: func() (any, error) { return m.Event(t.Context(), "bob", "calendar", id) },
	} {
		want, err := f()
		if err != nil {
			t.Fatal(path, err)
		}
		status, got := do(t, "GET", ts.URL+path, "", "")
		if status != 200 || got != encoded(t, want) {
			t.Errorf("GET %s = %d\n%s\nwant\n%s", path, status, got, encoded(t, want))
		}
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/calendar/events?limit=0", "", ""); status != 400 || !strings.Contains(body, "bad_request") {
		t.Errorf("limit 0 = %d %s", status, body)
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/calendar/events/8:99999", "", ""); status != 404 || !strings.Contains(body, "event_not_found") {
		t.Errorf("unknown event = %d %s", status, body)
	}
	if status, body := do(t, "POST", ts.URL+"/accounts/bob/calendar/events", "", `{}`); status != 403 || !strings.Contains(body, "write_disabled") {
		t.Errorf("POST = %d %s, want 403: the manager may not write", status, body)
	}
}

func TestRESTICS(t *testing.T) {
	srv, m, ts := api(t, false, "", "bob")
	now := time.Now().Truncate(time.Second)
	id := srv.SeedEvent("bob", easmockserver.Event{UID: "u", Subject: "ics over rest", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)})
	want, err := m.ICS(t.Context(), "bob", "calendar", id)
	if err != nil {
		t.Fatal(err)
	}
	// the DTSTAMP is the time of the request: compare without it
	strip := func(b []byte) string {
		var out []string
		for _, l := range strings.Split(string(b), "\r\n") {
			if !strings.HasPrefix(l, "DTSTAMP:") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\r\n")
	}
	for _, path := range []string{"/accounts/bob/calendar/events/" + id + "/ics", "/accounts/bob/folders/calendar/events/" + id + "/ics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strip(body) != strip(want.Data) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/calendar") {
			t.Errorf("GET %s = %d, type %q:\n%s", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/calendar/events/8:99999/ics", "", ""); status != 404 || !strings.Contains(body, "event_not_found") {
		t.Errorf("unknown event = %d %s", status, body)
	}
}
