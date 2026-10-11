package exporter

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String(), resp.Header
}

func TestConfigReloadRequest(t *testing.T) {
	srv, front, _ := newTestServer(t, nil)

	// no source to check: 409
	if code, body, _ := post(t, front.URL+"/config/reload"); code != 409 || !strings.Contains(body, "nothing to check") {
		t.Errorf("no reloader: %d %s", code, body)
	}

	calls := 0
	srv.SetReloader(func(ctx context.Context) (any, int) {
		calls++
		return map[string]string{"result": "applied"}, http.StatusOK
	})
	// a GET does not reload; the answer says what to use
	code, _ := get(t, front.URL+"/config/reload")
	if code != 405 || calls != 0 {
		t.Errorf("GET: %d, %d calls", code, calls)
	}
	if code, body, h := post(t, front.URL+"/config/reload"); code != 200 || !strings.Contains(body, `"applied"`) || calls != 1 || h.Get("Content-Type") == "" {
		t.Errorf("POST: %d %s (%d calls)", code, body, calls)
	}
	// a second request right away is refused, the source is not hit again
	if code, _, h := post(t, front.URL+"/config/reload"); code != 429 || h.Get("Retry-After") == "" || calls != 1 {
		t.Errorf("second POST: %d, %d calls", code, calls)
	}
}

func TestConfigReloadRequestFollowsTheTokenOfProbe(t *testing.T) {
	srv, front, _ := newTestServer(t, func(c *Config) { c.AuthToken = "s3cret" })
	calls := 0
	srv.SetReloader(func(ctx context.Context) (any, int) {
		calls++
		return map[string]string{"result": "unchanged"}, http.StatusOK
	})

	if code, _, _ := post(t, front.URL+"/config/reload"); code != 401 || calls != 0 {
		t.Errorf("no token: %d, %d calls", code, calls)
	}
	if code, _, _ := post(t, front.URL+"/config/reload", "Authorization", "Bearer wrong"); code != 401 || calls != 0 {
		t.Errorf("wrong token: %d, %d calls", code, calls)
	}
	if code, _, _ := post(t, front.URL+"/config/reload", "Authorization", "Bearer s3cret"); code != 200 || calls != 1 {
		t.Errorf("token: %d, %d calls", code, calls)
	}
}

func TestConfigReloadFailureIsAnError(t *testing.T) {
	srv, front, _ := newTestServer(t, nil)
	srv.SetReloader(func(ctx context.Context) (any, int) {
		return map[string]string{"result": "failed", "error": "the document is invalid"}, http.StatusInternalServerError
	})
	if code, body, _ := post(t, front.URL+"/config/reload"); code != 500 || !strings.Contains(body, "invalid") {
		t.Errorf("%d %s", code, body)
	}
}

func TestConfigStatusGoesTo1OnAFailureAndBackTo0(t *testing.T) {
	srv, front, _ := newTestServer(t, nil)
	status := func() string {
		_, body := get(t, front.URL+"/metrics")
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "eas_probe_config_status ") {
				return strings.TrimPrefix(line, "eas_probe_config_status ")
			}
		}
		t.Fatalf("no eas_probe_config_status in\n%s", body)
		return ""
	}
	errors := func() string {
		_, body := get(t, front.URL+"/metrics")
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "eas_probe_config_reload_errors_total ") {
				return strings.TrimPrefix(line, "eas_probe_config_reload_errors_total ")
			}
		}
		return ""
	}

	if status() != "0" {
		t.Fatalf("at the start: %s", status())
	}
	srv.ReloadFailed(errors2("the document is invalid"))
	if status() != "1" || errors() != "1" {
		t.Errorf("after a failure: status %s, errors %s", status(), errors())
	}
	srv.ReloadFailed(errors2("again"))
	if status() != "1" || errors() != "2" {
		t.Errorf("after a second failure: status %s, errors %s", status(), errors())
	}
	srv.ConfigChecked() // the next check works
	if status() != "0" {
		t.Errorf("after a good check: %s", status())
	}
	if errors() != "2" {
		t.Errorf("the counter must not be reset: %s", errors())
	}

	// the saved copy at the start: a failure that is not a reload
	srv.ConfigFailing(errors2("the location was down"))
	if status() != "1" || errors() != "2" {
		t.Errorf("saved copy: status %s, errors %s", status(), errors())
	}
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errors2(s string) error { return simpleError(s) }
