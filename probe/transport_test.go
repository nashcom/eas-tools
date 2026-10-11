package probe

import (
	"bytes"
	"context"
	"github.com/hstern/go-activesync/wbxml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConflictIsRetriedWithTheSameBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("attempt %d: body %q", calls.Load()+1, body)
		}
		if calls.Add(1) <= 2 {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := &http.Client{Transport: &conflictTransport{next: http.DefaultTransport,
		delays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}}
	resp, err := c.Post(srv.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 3 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls.Load())
	}
}

func TestConflictGivesUpAfterTheLastRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "conflict", http.StatusConflict)
	}))
	defer srv.Close()

	c := &http.Client{Transport: &conflictTransport{next: http.DefaultTransport,
		delays: []time.Duration{time.Millisecond, time.Millisecond}}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || calls.Load() != 3 {
		t.Fatalf("status %d after %d calls", resp.StatusCode, calls.Load())
	}
}

func TestTraceDecodesWBXMLAndLeavesSecretsOut(t *testing.T) {
	reg := wbxml.DefaultRegistry()
	reply, err := wbxml.Marshal(&wbxml.Document{Root: wbxml.E(wbxml.PageAirSync, "Sync",
		wbxml.E(wbxml.PageAirSync, "Status", wbxml.Text("3")))}, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SessionID=secret")
		w.Write(reply)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "trace.txt")
	tr, err := OpenTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	rec.setStep("sync")
	c := &http.Client{Transport: newTransport(http.DefaultTransport, rec, Config{Trace: tr})}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"?Cmd=Sync", bytes.NewReader(reply))
	req.Header.Set("Authorization", "Basic c2VjcmV0")
	req.Header.Set("Cookie", "SessionID=secret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	tr.Close()

	raw, _ := os.ReadFile(path)
	got := string(raw)
	for _, want := range []string{"step=sync POST", "Cmd=Sync", "<Status>3</Status>", "200 OK"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"c2VjcmV0", "SessionID"} {
		if strings.Contains(got, secret) {
			t.Errorf("trace must not contain %q:\n%s", secret, got)
		}
	}
}
