package load

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

func TestResultWriteAndOutcome(t *testing.T) {
	s := NewStats()
	s.Observe(probe.Exchange{Cmd: "Sync", Status: 200, Duration: 80 * time.Millisecond})
	s.Observe(probe.Exchange{Cmd: "Sync", Status: 200, Duration: 300 * time.Millisecond})
	s.Observe(probe.Exchange{Cmd: "Ping", Status: 409, Duration: 10 * time.Millisecond})

	r := Result{Tool: "eas-load", Mode: "standalone", Jobs: JobsResult{Stopped: 1}}
	s.Snapshot(&r)
	r.SetOutcome()
	r.SetTimes(time.Date(2026, 10, 10, 7, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 7, 5, 0, 0, time.UTC))

	if "ok" != r.Outcome {
		t.Fatalf("a job stopped by the duration is not a failure: %q", r.Outcome)
	}
	if 1 != r.Errors || 300 != r.Seconds {
		t.Fatalf("errors %d seconds %d", r.Errors, r.Seconds)
	}

	path := filepath.Join(t.TempDir(), "result.json")
	if err := r.Write(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if 2 != len(back.Requests) || "Ping" != back.Requests[0].Op || "Sync" != back.Requests[1].Op {
		t.Fatalf("requests: %+v", back.Requests)
	}
	sync := back.Requests[1]
	if 2 != sync.Count || 0 != sync.Errors || 0.3 != sync.MaxS || 2 != sync.Status["200"] {
		t.Fatalf("sync: %+v", sync)
	}
	if 1 != back.Requests[0].Errors {
		t.Fatalf("ping errors: %+v", back.Requests[0])
	}

	r.Jobs.Failed = 1
	r.SetOutcome()
	if "failed" != r.Outcome {
		t.Fatalf("failed job: %q", r.Outcome)
	}
}

func TestResultWriteToMissingFolder(t *testing.T) {
	r := Result{}
	if err := r.Write(filepath.Join(t.TempDir(), "nope", "result.json")); err == nil {
		t.Fatal("expected an error for a missing folder")
	}
}

func TestResultHasLostMails(t *testing.T) {
	s := NewStats()
	s.MailSent("self")
	s.MailMissed(MissedMail{Subject: "[easload a d2 #1]", Device: 2, Late: true, LateSeconds: 75, Polls: 60, SeenMails: 12})

	r := Result{}
	s.Snapshot(&r)
	if nil == r.Mail || 0 != r.Mail.Lost || 1 != r.Mail.Late || 1 != len(r.Mail.NotOnTime) {
		t.Fatalf("mail result %+v", r.Mail)
	}
	b, err := json.Marshal(r.Mail)
	if err != nil || !strings.Contains(string(b), `"subject":"[easload a d2 #1]"`) || !strings.Contains(string(b), `"late_seconds":75`) {
		t.Errorf("json %s, %v", b, err)
	}
}

func TestResultHasCleanupFailures(t *testing.T) {
	s := NewStats()
	s.MailSent("self")
	s.MailCleanupFailed("[easload a d3 #2]")

	r := Result{}
	s.Snapshot(&r)
	if nil == r.Mail || 1 != r.Mail.CleanupFailed || 1 != len(r.Mail.LeftBehind) || "[easload a d3 #2]" != r.Mail.LeftBehind[0] {
		t.Fatalf("mail result %+v", r.Mail)
	}
	if !strings.Contains(s.Summary(), "cleanup_failed=1") {
		t.Errorf("summary: %s", s.Summary())
	}
}

func TestMetricsHaveMailCleanupCounters(t *testing.T) {
	s := NewStats()
	s.MailSent("self")
	s.MailMissed(MissedMail{Late: true})
	s.MailCleanupFailed("x")
	s.SentCopies(7, 1)

	var b strings.Builder
	s.WritePrometheus(&b)
	for _, want := range []string{"eas_load_mail_late_total 1\n", "eas_load_mail_cleanup_failed_total 1\n",
		`eas_load_mail_sent_copies_total{result="deleted"} 7`, `eas_load_mail_sent_copies_total{result="failed"} 1`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if !strings.Contains(s.Summary(), "sent_copies_deleted=7 sent_copies_failed=1") {
		t.Errorf("summary: %s", s.Summary())
	}
}
