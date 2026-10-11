package easmockserver_test

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

func session(t *testing.T, url string, hook func(probe.Exchange)) *probe.Session {
	t.Helper()
	s, err := probe.NewSession(probe.Config{
		ServerURL:  url,
		Username:   "bob",
		Password:   easmockserver.Password,
		OnExchange: hook,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func setup(t *testing.T, s *probe.Session) {
	t.Helper()
	ctx := t.Context()
	if err := s.Options(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Provision(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FolderSync(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMailArrivesOverTime(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{MailEvery: 40 * time.Millisecond}))
	defer srv.Close()

	s := session(t, srv.URL, nil)
	setup(t, s)
	if _, err := s.Sync(t.Context(), nil); err != nil { // bootstrap
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	res, err := s.Sync(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added < 3 {
		t.Errorf("added = %d, want at least 3 after 200 ms", res.Added)
	}

	// Nothing new right away.
	res, err = s.Sync(t.Context(), nil)
	if err != nil || res.Added > 1 {
		t.Errorf("immediate second sync: %+v, %v", res, err)
	}
}

func TestMoreAvailablePaging(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{MailEvery: 10 * time.Millisecond}))
	defer srv.Close()

	s := session(t, srv.URL, nil)
	setup(t, s)
	_, _ = s.Sync(t.Context(), nil)

	time.Sleep(250 * time.Millisecond) // about 25 mails, window is 5
	res, err := s.Sync(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 5 || !res.MoreAvailable {
		t.Errorf("first page: %+v", res)
	}
}

func TestPingHoldsAndWakesOnMail(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true}))
	defer srv.Close()

	s := session(t, srv.URL, nil)
	setup(t, s)

	start := time.Now()
	res, err := s.Ping(t.Context(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held := time.Since(start); held < 900*time.Millisecond || res.Changes {
		t.Errorf("no mail: held %v, changes %v; want a full heartbeat and no changes", held, res.Changes)
	}

	srv2 := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{HoldPing: true, MailEvery: 150 * time.Millisecond}))
	defer srv2.Close()
	s2 := session(t, srv2.URL, nil)
	setup(t, s2)
	_, _ = s2.Sync(t.Context(), nil)

	start = time.Now()
	res, err = s2.Ping(t.Context(), 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held := time.Since(start); held > 3*time.Second || !res.Changes {
		t.Errorf("mail arriving: held %v, changes %v; want an early wake-up with changes", held, res.Changes)
	}
}

func TestLatencyIsApplied(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{Latency: 80 * time.Millisecond}))
	defer srv.Close()

	var mu sync.Mutex
	var min = time.Hour
	s := session(t, srv.URL, func(x probe.Exchange) {
		mu.Lock()
		defer mu.Unlock()
		if x.Duration < min {
			min = x.Duration
		}
	})
	setup(t, s)

	mu.Lock()
	defer mu.Unlock()
	if min < 80*time.Millisecond {
		t.Errorf("fastest request took %v, latency option not applied", min)
	}
}

func TestErrorInjection(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{ErrorRate: 1}))
	defer srv.Close()

	s := session(t, srv.URL, nil)
	if err := s.Options(t.Context(), nil); err != nil {
		t.Fatalf("OPTIONS is not subject to error injection: %v", err)
	}
	err := s.Provision(t.Context(), nil)
	if err == nil {
		t.Fatal("Provision must fail with injected errors")
	}
	if class := probe.Classify(t.Context(), err); class != "server_error" {
		t.Errorf("error class = %q", class)
	}
}

func TestPasswordOption(t *testing.T) {
	srv := httptest.NewServer(easmockserver.NewWithOptions(easmockserver.Options{Password: "other"}))
	defer srv.Close()

	s := session(t, srv.URL, nil) // uses "secret"
	err := s.Options(t.Context(), nil)
	if err == nil || probe.Classify(t.Context(), err) != "auth" {
		t.Errorf("wrong password must be an auth failure, got %v", err)
	}
}
