package herd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nashcom/eas-tools/internal/testherd"
)

func TestRegisterAndStatus(t *testing.T) {
	f := testherd.New(1, "pw")
	ts := httptest.NewServer(f)
	defer ts.Close()
	c := New(ts.URL, "")

	r, err := c.Register(t.Context(), "w-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.TestID != 1 || r.InternetAddress != "load000001@example.com" || r.Password != "pw" || r.Command != "idle" {
		t.Errorf("register reply = %+v", r)
	}

	// Same key, same account.
	again, err := c.Register(t.Context(), "w-1")
	if err != nil || again.TestID != 1 {
		t.Errorf("repeat register = %+v, %v", again, err)
	}

	// The pool has one account: the next key finds it exhausted.
	if _, err := c.Register(t.Context(), "w-2"); !WaitForAccount(err) {
		t.Errorf("exhausted pool must be a wait condition, got %v", err)
	}

	id := f.Command("run", "eas-sync", "think=5", 0)
	rep, err := c.Status(t.Context(), Report{TestID: 1, State: "idle", Ack: 0, LastJobID: 0})
	if err != nil || rep.Command != "run" || rep.CommandID != id || rep.Params != "think=5" {
		t.Errorf("status reply = %+v, %v", rep, err)
	}

	if _, err := c.Status(t.Context(), Report{TestID: 1, State: "idle", LastJobID: 3, LastJob: "x", LastJobResult: "ok"}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.Last(1)
	if got.LastJobID != 3 || got.LastJob != "x" || got.LastJobResult != "ok" {
		t.Errorf("job result not sent: %+v", got)
	}
}

func TestJobResultOnlyWhenPresent(t *testing.T) {
	f := testherd.New(1, "pw")
	ts := httptest.NewServer(f)
	defer ts.Close()
	c := New(ts.URL, "")
	if _, err := c.Register(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(t.Context(), Report{TestID: 1, State: "idle"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Last(1); got.LastJobID != 0 || got.LastJob != "" {
		t.Errorf("no job ended yet, got %+v", got)
	}
}

func TestErrors(t *testing.T) {
	f := testherd.New(1, "pw")
	f.RequireToken("tok")
	ts := httptest.NewServer(f)
	defer ts.Close()

	_, err := New(ts.URL, "wrong").Register(t.Context(), "k")
	ae, ok := err.(*APIError)
	if !ok || ae.Status != http.StatusUnauthorized || ae.Code != "unauthorized" {
		t.Errorf("wrong token: %v", err)
	}

	ok1 := New(ts.URL, "tok")
	if _, err := ok1.Register(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}
	f.Forget()
	if _, err := ok1.Status(t.Context(), Report{TestID: 1, State: "idle"}); !UnknownClient(err) {
		t.Errorf("forgotten client: %v", err)
	}
}

func TestCleanMessage(t *testing.T) {
	if got := cleanMessage("a\nb\tc"); got != "a b c" {
		t.Errorf("control characters: %q", got)
	}

	long := strings.Repeat("ä", MaxMessageBytes) // two bytes each
	got := cleanMessage(long)
	if len(got) > MaxMessageBytes || !utf8.ValidString(got) {
		t.Errorf("truncation broke the text: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}
