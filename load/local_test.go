package load

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/herd"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func standaloneRun(t *testing.T, local *Local, clients int, timeout time.Duration) {
	t.Helper()

	srv := httptest.NewServer(easmockserver.New())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := Run(ctx, Config{
		Herd:       local,
		Name:       "local",
		Clients:    clients,
		Server:     srv.URL,
		Poll:       20 * time.Millisecond,
		RetryDelay: 20 * time.Millisecond,
		Stats:      NewStats(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("the standalone run did not end by itself")
	}
}

func accounts(t *testing.T, n int, password string) []herd.Reply {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("FirstName,LastName,Password,Shortname,InternetAddress\n")
	for i := 1; i <= n; i++ {
		sb.WriteString("Load,User," + password + ",user" + string(rune('0'+i)) + ",user" + string(rune('0'+i)) + "@example.com\n")
	}
	acc, err := ParseAccounts(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestStandaloneJobEndsByItself(t *testing.T) {
	local := NewLocal(accounts(t, 2, easmockserver.Password), 2, JobProbe, "", 0)
	standaloneRun(t, local, 2, 30*time.Second)

	r := local.Result()
	if r.OK != 2 || !r.Success() {
		t.Errorf("result = %+v", r)
	}
}

func TestStandaloneDurationStopsTheJob(t *testing.T) {
	local := NewLocal(accounts(t, 2, easmockserver.Password), 2, JobSync, "think=0.05", 600*time.Millisecond)
	start := time.Now()
	standaloneRun(t, local, 2, 30*time.Second)

	r := local.Result()
	if r.Stopped != 2 || !r.Success() {
		t.Errorf("a stop by duration is not a failure: %+v", r)
	}
	if time.Since(start) > 15*time.Second {
		t.Error("the duration did not stop the run")
	}
}

func TestStandaloneWrongPasswordFails(t *testing.T) {
	local := NewLocal(accounts(t, 1, "wrong"), 1, JobProbe, "", 0)
	standaloneRun(t, local, 1, 30*time.Second)

	r := local.Result()
	if r.Failed != 1 || r.Success() {
		t.Errorf("a refused login must fail the run: %+v", r)
	}
}

func TestStandaloneNeedsAnAccountPerClient(t *testing.T) {
	local := NewLocal(accounts(t, 1, "x"), 2, JobProbe, "", 0)
	if _, err := local.Register(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	_, err := local.Register(context.Background(), "b")
	if !herd.WaitForAccount(err) {
		t.Errorf("a second client without an account must wait, got %v", err)
	}
}

func TestParseAccounts(t *testing.T) {
	acc, err := ParseAccounts(strings.NewReader("FirstName,LastName,Password,Shortname,InternetAddress\n" +
		"\"Anna\",\"Lee\",\"pw,1\",anna,anna@example.com\n# a comment\nBo,Ray,pw2,bo,bo@example.com\n"))
	if err != nil || len(acc) != 2 || acc[0].Password != "pw,1" || acc[1].InternetAddress != "bo@example.com" {
		t.Errorf("accounts = %+v, %v", acc, err)
	}
	for _, bad := range []string{"", "a,b,c\n", "A,B,,short,a@b.c\n", "A,B,pw,short,noaddress\n"} {
		if _, err := ParseAccounts(strings.NewReader(bad)); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
