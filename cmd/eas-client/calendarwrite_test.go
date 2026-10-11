package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// mailbox starts a mock mailbox that mails invitations and where carol is known.
func writeEnv(t *testing.T) env {
	t.Helper()
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true, RequireOptions: true, EchoClientID: true})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Setenv("EASCLIENT_PASSWORD", easmockserver.Password)
	e := env{srv, ts.URL}
	var out, errb bytes.Buffer
	run([]string{"-server", e.url, "-email", "carol@example.com", "-user", "carol", "-no-state", "info"}, strings.NewReader(""), &out, &errb, os.Environ)
	return e
}

func TestCalendarCreateAndDeleteFromTheCommandLine(t *testing.T) {
	e := writeEnv(t)
	start := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)

	if code, _, errs := e.eas(t, "", "calendar", "create", "-subject", "x", "-start", start); code != 1 || !strings.Contains(errs, "write_disabled") {
		t.Errorf("without -write: %d %s", code, errs)
	}
	code, out, errs := e.eas(t, "the agenda", "-write", "calendar", "create", "-subject", "Review", "-location", "Room 4", "-start", start, "-duration", "45m",
		"-reminder", "10", "-body-stdin")
	if code != 0 || !strings.Contains(out, `"action": "create"`) {
		t.Fatalf("create: %d %s %s", code, out, errs)
	}
	code, out, errs = e.eas(t, "", "calendar", "list", "-limit", "all")
	if code != 0 || !strings.Contains(out, `"subject": "Review"`) || !strings.Contains(out, `"reminder_minutes": 10`) || !strings.Contains(out, "the agenda") {
		t.Fatalf("list: %d %s %s", code, out, errs)
	}
	events := e.srv.Messages("bob", "8")
	_ = events

	// all-day
	if code, out, errs := e.eas(t, "", "-write", "calendar", "create", "-subject", "Holiday", "-all-day", "-start", "2030-12-24", "-end", "2030-12-26"); code != 0 {
		t.Errorf("all-day: %d %s %s", code, out, errs)
	}
}

func TestCalendarMeetingFromTheCommandLine(t *testing.T) {
	e := writeEnv(t)
	start := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	args := []string{"calendar", "create", "-subject", "Kickoff", "-start", start, "-attendee", "Carol <carol@example.com>", "-optional", "dave@example.com", "-resource", "room1@example.com"}

	code, _, errs := e.eas(t, "", append([]string{"-write"}, args...)...)
	if code != 1 || !strings.Contains(errs, "send_disabled") {
		t.Errorf("a meeting with -write only: %d %s", code, errs)
	}
	code, out, errs := e.eas(t, "", append([]string{"-write", "-send"}, args...)...)
	if code != 0 || !strings.Contains(out, "carol@example.com") || !strings.Contains(out, "room1@example.com") {
		t.Fatalf("meeting: %d %s %s", code, out, errs)
	}
	if got := e.srv.Messages("carol", "5"); len(got) != 1 || got[0].Subject != "Kickoff" {
		t.Errorf("carol's Inbox = %+v", got)
	}
	_, out, _ = e.eas(t, "", "calendar", "list", "-limit", "all")
	for _, want := range []string{`"meeting": "meeting"`, `"name": "Carol"`, `"type": "optional"`, `"type": "resource"`, `"email": "bob@example.com"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the event lacks %s:\n%s", want, out)
		}
	}

	// delete: a meeting needs -send as well
	_, list, _ := e.eas(t, "", "calendar", "list", "-limit", "all")
	id := "8:" + strings.Split(strings.Split(list, `"id": "8:`)[1], `"`)[0] // the first id is the one of the folder
	if code, _, errs := e.eas(t, "", "-write", "calendar", "delete", id); code != 1 || !strings.Contains(errs, "send_disabled") {
		t.Errorf("delete a meeting with -write only: %d %s", code, errs)
	}
	if code, out, errs := e.eas(t, "", "-write", "-send", "calendar", "delete", id); code != 0 || !strings.Contains(out, `"action": "delete"`) {
		t.Errorf("delete: %d %s %s", code, out, errs)
	}
}

func TestCalendarCreateUsage(t *testing.T) {
	e := writeEnv(t)
	for _, args := range [][]string{
		{"-write", "calendar", "create", "a", "b", "-subject", "x", "-start", "2030-01-01T10:00:00Z"},
		{"-write", "calendar", "delete"},
		{"-write", "calendar", "create", "-subject", "x", "-start", "2030-01-01T10:00:00Z", "-body", "a", "-body-file", "f"},
	} {
		if code, out, errs := e.eas(t, "", args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, %q %q; want a usage error", args, code, out, errs)
		}
	}
	// a request that the server side refuses is JSON, not usage
	if code, _, errs := e.eas(t, "", "-write", "calendar", "create", "-subject", "x"); code != 1 || !strings.Contains(errs, "bad_request") {
		t.Errorf("no start: %d %s", code, errs)
	}
}

func TestTimezoneOptionAndTheZoneOfTheMachine(t *testing.T) {
	e := writeEnv(t)
	// -timezone: a time without an offset is a time in that zone: 11:00 in Berlin is 10:00 UTC in November
	if code, out, errs := e.eas(t, "", "-timezone", "Europe/Berlin", "-write", "calendar", "create", "-subject", "berlin", "-start", "2030-11-20T11:00:00"); code != 0 {
		t.Fatalf("create: %d %s %s", code, out, errs)
	}
	_, out, _ := e.eas(t, "", "-timezone", "Europe/Berlin", "calendar", "list", "-from", "2030-01-01", "-limit", "all")
	if !strings.Contains(out, `"time": "2030-11-20T10:00:00Z"`) {
		t.Errorf("the event starts at 10:00 UTC:\n%s", out)
	}
	_, out, _ = e.eas(t, "", "-timezone", "Europe/Berlin", "info")
	if !strings.Contains(out, `"time_zone": "Europe/Berlin"`) {
		t.Errorf("info lacks the zone:\n%s", out)
	}

	// no option: the machine
	t.Setenv("TZ", "America/New_York")
	_, out, _ = e.eas(t, "", "info")
	if !strings.Contains(out, `"time_zone": "America/New_York"`) {
		t.Errorf("the zone of the machine is not used:\n%s", out)
	}
	if code, _, errs := e.eas(t, "", "-timezone", "Mars/Base", "info"); code != 2 || !strings.Contains(errs, "time zone") {
		t.Errorf("a wrong zone: %d %s", code, errs)
	}
}
