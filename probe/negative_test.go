package probe

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// The negative cases tell the mock server how to misbehave with the X-Mock-Sim
// header of the request (-sim mode), the same way as the command line option
// -header does it against the real binaries.
func TestNegativeCasesWithSimulatedServerBehavior(t *testing.T) {
	cases := []struct {
		name    string
		sim     string
		failed  string // step expected to fail ("" = the whole run must succeed)
		class   string // expected error class of the failed step
		timeout time.Duration
		mail    bool
	}{
		{name: "proxy answers 502 on OPTIONS", sim: "cmd=OPTIONS,http=502", failed: StepOptions, class: "server_error"},
		{name: "server says 403", sim: "cmd=OPTIONS,http=403", failed: StepOptions, class: "forbidden"},
		{name: "wrong path 404", sim: "cmd=OPTIONS,http=404", failed: StepOptions, class: "not_found"},
		{name: "401 without a challenge", sim: "cmd=OPTIONS,http=401", failed: StepOptions, class: "auth"},
		{name: "server closes the connection on FolderSync", sim: "cmd=FolderSync,close=1", failed: StepFolderSync, class: "network"},
		{name: "garbage instead of WBXML on FolderSync", sim: "cmd=FolderSync,garbage=1", failed: StepFolderSync},
		{name: "slow Sync beyond the step timeout", sim: "cmd=Sync,delay=3s", failed: StepSync, class: "timeout", timeout: 800 * time.Millisecond},
		{name: "Sync status 3 once: recovers", sim: "cmd=Sync,status=3,count=1"},
		{name: "FolderSync status 9 once: recovers", sim: "cmd=FolderSync,status=9,count=1"},
		{name: "409 on Sync once: the request is repeated", sim: "cmd=Sync,http=409,count=1"},
		{name: "409 on Sync always: gives up", sim: "cmd=Sync,http=409", failed: StepSync, class: "http", timeout: 20 * time.Second},
		{name: "FolderSync status 6 stays a failure", sim: "cmd=FolderSync,status=6", failed: StepFolderSync, class: "eas_status"},
		{name: "mail refused with status 118", sim: "cmd=SendMail,status=118", failed: StepMail, mail: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := easmockserver.NewWithOptions(easmockserver.Options{Sim: true})
			srv := httptest.NewServer(mock)
			defer srv.Close()

			cfg := baseCfg(srv.URL, nil)
			cfg.Username = "neg@example.com"
			cfg.Headers = http.Header{SimHeader: {tc.sim}}
			cfg.Directory = "off"
			if tc.timeout > 0 {
				cfg.StepTimeout = tc.timeout
			}
			if tc.mail {
				cfg.Mail = true
				cfg.MailWait = 3 * time.Second
			}

			res, err := Run(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if mock.SimHits() == 0 {
				t.Fatal("the simulated behavior never happened: the directive was not applied")
			}
			if tc.failed == "" {
				if !res.OK {
					t.Fatalf("the run must recover: %+v", failedStep(res))
				}
				return
			}
			if res.OK {
				t.Fatalf("the run must fail in step %s", tc.failed)
			}
			st := failedStep(res)
			if st == nil || st.Name != tc.failed {
				t.Fatalf("expected the step %s to fail, got %+v", tc.failed, st)
			}
			if tc.class != "" && st.ErrorClass != tc.class {
				t.Errorf("error class = %q, want %q (%s)", st.ErrorClass, tc.class, st.Error)
			}
			if st.Error == "" {
				t.Error("a failed step must carry an error text")
			}
		})
	}
}

func failedStep(r *Result) *Step {
	for i := range r.Steps {
		if !r.Steps[i].OK && !r.Steps[i].Skipped {
			return &r.Steps[i]
		}
	}
	return nil
}

func TestHeadersAreCheckedAndSetByTheTool(t *testing.T) {
	if _, err := ParseHeaders([]string{"Authorization: Basic abc"}); err == nil {
		t.Error("Authorization must be refused")
	}
	if _, err := ParseHeaders([]string{"no colon"}); err == nil {
		t.Error("a header without a colon must be refused")
	}
	if _, err := ParseHeaders([]string{"Bad Name: x"}); err == nil {
		t.Error("a name with a space must be refused")
	}
	h, err := ParseHeaders([]string{"X-Mock-Sim: cmd=Sync,status=3", "X-Other: 1"})
	if err != nil || h.Get("X-Mock-Sim") != "cmd=Sync,status=3" || h.Get("X-Other") != "1" {
		t.Errorf("parsed: %v, %v", h, err)
	}
}
