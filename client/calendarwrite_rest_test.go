package client_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
)

func TestRESTCreateAndDeleteEvents(t *testing.T) {
	srv, m := writer(t, true, true)
	ts := httptest.NewServer(m.Handler(""))
	defer ts.Close()
	start := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	status, body := do(t, "POST", ts.URL+"/accounts/bob/calendar/events", "",
		`{"subject":"Over REST","start":"`+start+`","duration":"30m","attendees":[{"email":"carol@example.com","name":"Carol"}]}`)
	if status != 200 || !strings.Contains(body, `"action": "create"`) || !strings.Contains(body, "carol@example.com") {
		t.Fatalf("create = %d %s", status, body)
	}
	if got := srv.Messages("carol", "5"); len(got) != 1 || got[0].Subject != "Over REST" {
		t.Errorf("carol's Inbox = %+v", got)
	}
	l, _ := m.Events(t.Context(), "bob", "", clientEventParams())
	if l == nil || l.Count != 1 {
		t.Fatalf("events = %+v", l)
	}
	id := l.Events[0].ID

	if status, body := do(t, "POST", ts.URL+"/accounts/bob/folders/calendar/events", "", `{"subject":"In a folder","start":"`+start+`"}`); status != 200 {
		t.Errorf("create in a folder = %d %s", status, body)
	}
	if status, body := do(t, "POST", ts.URL+"/accounts/bob/calendar/events", "", `{"subject":"x"}`); status != 400 || !strings.Contains(body, "bad_request") {
		t.Errorf("create without a start = %d %s", status, body)
	}
	if status, body := do(t, "POST", ts.URL+"/accounts/bob/calendar/events", "", `{"subject":"x","start":"`+start+`","unknown":1}`); status != 400 {
		t.Errorf("create with an unknown key = %d %s", status, body)
	}
	if status, body := do(t, "DELETE", ts.URL+"/accounts/bob/calendar/events/"+id, "", ""); status != 200 || !strings.Contains(body, `"action": "delete"`) {
		t.Errorf("delete = %d %s", status, body)
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/calendar/events/"+id, "", ""); status != 404 || !strings.Contains(body, "event_not_found") {
		t.Errorf("the deleted event = %d %s", status, body)
	}
	if status, _ := do(t, "PATCH", ts.URL+"/accounts/bob/calendar/events/"+id, "", `{}`); status != 405 {
		t.Errorf("PATCH = %d, want 405: changing events is not there yet", status)
	}
}

func clientEventParams() client.EventParams { return client.EventParams{Limit: -1} }
