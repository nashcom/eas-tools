package testherd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func post(t *testing.T, base, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func get(t *testing.T, base, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestCommandEndpointAndSummary(t *testing.T) {
	s := New(2, "pw")
	ts := httptest.NewServer(s)
	defer ts.Close()

	post(t, ts.URL, "/register", url.Values{"request_key": {"a"}})
	post(t, ts.URL, "/register", url.Values{"request_key": {"b"}})

	code, m := post(t, ts.URL, "/command", url.Values{"target": {"all"}, "command": {"run"}, "job": {"eas-sync"}, "params": {"think=5"}})
	if code != 200 || m["updated"] != float64(2) || m["command_id"] != float64(1) {
		t.Fatalf("command: %d %v", code, m)
	}

	_, reply := post(t, ts.URL, "/status", url.Values{"test_id": {"1"}, "state": {"running"}, "ack_command_id": {"1"}})
	if reply["command"] != "run" || reply["job"] != "eas-sync" || reply["params"] != "think=5" {
		t.Errorf("instruction = %v", reply)
	}
	post(t, ts.URL, "/status", url.Values{"test_id": {"2"}, "state": {"idle"}, "ack_command_id": {"1"},
		"last_job_id": {"1"}, "last_job": {"eas-sync"}, "last_job_result": {"failed"}})
	// the same result again must not be counted twice
	post(t, ts.URL, "/status", url.Values{"test_id": {"2"}, "state": {"idle"}, "ack_command_id": {"1"},
		"last_job_id": {"1"}, "last_job": {"eas-sync"}, "last_job_result": {"failed"}})

	code, sum := get(t, ts.URL, "/status")
	if code != 200 || sum["clients_total"] != float64(2) || sum["clients_running"] != float64(1) ||
		sum["clients_idle"] != float64(1) || sum["jobs_failed"] != float64(1) {
		t.Errorf("summary = %d %v", code, sum)
	}
}

func TestCommandValidation(t *testing.T) {
	ts := httptest.NewServer(New(1, "pw"))
	defer ts.Close()

	for name, form := range map[string]url.Values{
		"run without job":    {"target": {"all"}, "command": {"run"}},
		"pause without time": {"target": {"all"}, "command": {"pause"}},
		"unknown command":    {"target": {"all"}, "command": {"explode"}},
		"single client":      {"test_id": {"1"}, "command": {"idle"}},
	} {
		if code, _ := post(t, ts.URL, "/command", form); code != 400 {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
}

func TestHealthIsOpenTokenProtectsTheRest(t *testing.T) {
	s := New(1, "pw")
	s.RequireToken("0123456789abcdef")
	ts := httptest.NewServer(s)
	defer ts.Close()

	if code, _ := get(t, ts.URL, "/health"); code != 200 {
		t.Errorf("health: %d", code)
	}
	if code, _ := get(t, ts.URL, "/status"); code != 401 {
		t.Errorf("status without token: %d", code)
	}
}

func TestReportHistoryCap(t *testing.T) {
	s := New(1, "pw")
	s.MaxReports = 5
	ts := httptest.NewServer(s)
	defer ts.Close()

	post(t, ts.URL, "/register", url.Values{"request_key": {"a"}})
	for i := 0; i < 20; i++ {
		post(t, ts.URL, "/status", url.Values{"test_id": {"1"}, "state": {"idle"}, "ack_command_id": {"0"}})
	}
	if n := len(s.Reports()); n != 5 {
		t.Errorf("kept %d reports, want 5", n)
	}
}

func TestAccountsFromCSV(t *testing.T) {
	list, err := ParseCSV("Firstname,Lastname,Password,Shortname,InternetAddress\r\nDaniel,Nashed,pw,nsh,nsh@lab.example\n\nAnna,Test,pw2,anna,anna@lab.example\n")
	if err != nil || len(list) != 2 || list[0].InternetAddress != "nsh@lab.example" || list[1].Shortname != "anna" {
		t.Fatalf("parse: %+v, %v", list, err)
	}
	for _, bad := range []string{"", "a,b,c", "a,b,,d,e@x", "a,b,pw,s,noaddress"} {
		if _, err := ParseCSV(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}

	ts := httptest.NewServer(NewWithAccounts(list))
	defer ts.Close()
	_, r := post(t, ts.URL, "/register", url.Values{"request_key": {"k"}})
	if r["internetaddress"] != "nsh@lab.example" || r["password"] != "pw" || r["shortname"] != "nsh" {
		t.Errorf("register reply = %v", r)
	}
	code, _ := post(t, ts.URL, "/register", url.Values{"request_key": {"k2"}})
	code2, _ := post(t, ts.URL, "/register", url.Values{"request_key": {"k3"}})
	if code != 201 || code2 != 409 {
		t.Errorf("two accounts in the pool: second %d, third %d", code, code2)
	}
}
