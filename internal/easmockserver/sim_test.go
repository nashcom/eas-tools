package easmockserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func simGet(t *testing.T, url, sim, cmd, device string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"?Cmd="+cmd+"&DeviceId="+device+"&DeviceType=iPhone&User=u", strings.NewReader("x"))
	req.SetBasicAuth("u", Password)
	if sim != "" {
		req.Header.Set(SimHeader, sim)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

func TestSimIsOffUnlessEnabled(t *testing.T) {
	srv := httptest.NewServer(NewWithOptions(Options{}))
	defer srv.Close()

	resp, _, err := simGet(t, srv.URL, "http=502", "Sync", "D1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 502 {
		t.Error("the header must be ignored without -sim")
	}
}

func TestSimDirectives(t *testing.T) {
	m := NewWithOptions(Options{Sim: true})
	srv := httptest.NewServer(m)
	defer srv.Close()

	// http status for one command only
	resp, _, _ := simGet(t, srv.URL, "cmd=Sync,http=502", "Sync", "D1")
	if resp.StatusCode != 502 {
		t.Errorf("http=502 for Sync: %d", resp.StatusCode)
	}
	resp, _, _ = simGet(t, srv.URL, "cmd=Sync,http=502", "FolderSync", "D1")
	if resp.StatusCode == 502 {
		t.Error("a directive for Sync must not hit FolderSync")
	}

	// separator: a semicolon works as well as a comma
	resp, _, _ = simGet(t, srv.URL, "cmd=Sync;http=503", "Sync", "D1")
	if resp.StatusCode != 503 {
		t.Errorf("semicolon separated: %d", resp.StatusCode)
	}

	// EAS status for commands of different pages
	for _, cmd := range []string{"Sync", "FolderSync", "Ping", "Provision", "Settings", "Search", "SendMail"} {
		resp, body, _ := simGet(t, srv.URL, "cmd="+cmd+",status=7", cmd, "D1")
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Errorf("status=7 for %s: %d, %d bytes", cmd, resp.StatusCode, len(body))
		}
	}

	// garbage is not WBXML
	resp, body, _ := simGet(t, srv.URL, "garbage=1", "Sync", "D1")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "not a valid wbxml") {
		t.Errorf("garbage: %d %q", resp.StatusCode, body)
	}

	// the connection is closed without an answer
	if _, _, err := simGet(t, srv.URL, "close=1", "Sync", "D1"); err == nil {
		t.Error("close=1 must end without an answer")
	}

	// delay
	start := time.Now()
	simGet(t, srv.URL, "delay=300ms", "Sync", "D1")
	if time.Since(start) < 300*time.Millisecond {
		t.Error("delay=300ms was not applied")
	}
}

func TestSimCountIsPerDevice(t *testing.T) {
	srv := httptest.NewServer(NewWithOptions(Options{Sim: true}))
	defer srv.Close()

	codes := func(device string) (out []int) {
		for i := 0; i < 3; i++ {
			resp, _, _ := simGet(t, srv.URL, "cmd=Sync,http=502,count=2", "Sync", device)
			out = append(out, resp.StatusCode)
		}
		return out
	}
	a := codes("A")
	if a[0] != 502 || a[1] != 502 || a[2] == 502 {
		t.Errorf("count=2 for device A: %v", a)
	}
	b := codes("B")
	if b[0] != 502 || b[1] != 502 || b[2] == 502 {
		t.Errorf("device B has its own count: %v", b)
	}
}
