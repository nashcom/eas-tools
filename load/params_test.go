package load

import (
	"testing"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

func TestParseSyncParams(t *testing.T) {
	p, err := parseSyncParams("")
	if err != nil || p.ThinkMin != 10*time.Second || p.ThinkMax != 30*time.Second || p.Ping != 0 {
		t.Errorf("defaults: %+v, %v", p, err)
	}

	p, err = parseSyncParams("think=5-15&ping=60&duration=300")
	if err != nil || p.ThinkMin != 5*time.Second || p.ThinkMax != 15*time.Second || p.Ping != 60 || p.Duration != 300*time.Second {
		t.Errorf("full: %+v, %v", p, err)
	}

	p, err = parseSyncParams("think=7")
	if err != nil || p.ThinkMin != 7*time.Second || p.ThinkMax != 7*time.Second {
		t.Errorf("single value: %+v, %v", p, err)
	}

	for _, bad := range []string{"think=20-10", "think=x", "think=-5", "ping=-1", "ping=99999", "duration=x", "other=1", "think"} {
		if _, err := parseSyncParams(bad); err == nil && bad != "think" {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestThinkStaysInRange(t *testing.T) {
	p := syncParams{ThinkMin: time.Second, ThinkMax: 3 * time.Second}
	for i := 0; i < 200; i++ {
		if d := p.think(); d < p.ThinkMin || d > p.ThinkMax {
			t.Fatalf("think = %v", d)
		}
	}
}

func TestQuantiles(t *testing.T) {
	s := NewStats()
	for i := 0; i < 90; i++ {
		s.Observe(probe.Exchange{Cmd: "Sync", Status: 200, Duration: 40 * time.Millisecond})
	}
	for i := 0; i < 10; i++ {
		s.Observe(probe.Exchange{Cmd: "Sync", Status: 200, Duration: 900 * time.Millisecond})
	}
	s.Observe(probe.Exchange{Cmd: "Sync", Status: 401, Duration: time.Millisecond})

	o := s.ops["Sync"]
	if o.errs != 1 || o.ok != 100 {
		t.Errorf("ok=%d errs=%d", o.ok, o.errs)
	}
	if p50 := o.quantile(0.5); p50 > 0.05 {
		t.Errorf("p50 = %v", p50)
	}
	if p95 := o.quantile(0.95); p95 < 0.5 || p95 > 1 {
		t.Errorf("p95 = %v", p95)
	}
}

func TestParseGalParams(t *testing.T) {
	p, err := parseGalParams("")
	if err != nil || p.Mode != "self" || p.ThinkMin != 10*time.Second {
		t.Errorf("defaults: %+v, %v", p, err)
	}
	p, err = parseGalParams("mode=typeahead&think=1-2&duration=30&query=smith")
	if err != nil || p.Mode != "typeahead" || p.Query != "smith" || p.Duration != 30*time.Second || p.ThinkMax != 2*time.Second {
		t.Errorf("full: %+v, %v", p, err)
	}
	for _, bad := range []string{"mode=x", "query=", "think=-1", "duration=x", "other=1"} {
		if _, err := parseGalParams(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParseMailParams(t *testing.T) {
	p, err := parseMailParams("")
	if err != nil || p.To != "self" || !p.Cleanup || p.Arrival != "poll" || p.Wait != 60*time.Second || p.ThinkMin != 30*time.Second {
		t.Errorf("defaults: %+v, %v", p, err)
	}
	p, err = parseMailParams("to=a@x.de,b@y.de&think=1-2&size=2-4&wait=30&arrival=poll&cleanup=no&count=5&duration=60")
	if err != nil || p.To != "list" || len(p.List) != 2 || p.List[1] != "b@y.de" || p.SizeMinKB != 2 || p.SizeMaxKB != 4 ||
		p.Wait != 30*time.Second || p.Arrival != "poll" || p.Cleanup || p.Count != 5 || p.Duration != 60*time.Second {
		t.Errorf("full: %+v, %v", p, err)
	}
	for _, bad := range []string{"to=x", "to=a@b.c,y", "size=99999", "wait=5000", "arrival=x", "cleanup=x", "count=-1", "other=1"} {
		if _, err := parseMailParams(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	if n := p.sizeBytes(); n < 2048 || n > 4096 {
		t.Errorf("size = %d bytes", n)
	}
}

func TestQuantileNeverAboveMax(t *testing.T) {
	s := NewStats()
	// All values sit in the wide 2.5-5 s bucket; the interpolated p95 would be above the real maximum.
	for _, d := range []float64{2.6, 2.7, 2.8, 3.0, 3.7} {
		s.Observe(probe.Exchange{Cmd: "Settings", Status: 200, Duration: time.Duration(d * float64(time.Second))})
	}

	o := s.ops["Settings"]
	if p95 := o.quantile(0.95); p95 > o.max {
		t.Errorf("p95 %v above max %v", p95, o.max)
	}
}

func TestParseMailAttachParams(t *testing.T) {
	p, err := parseMailParams("")
	if err != nil || p.AttachMaxKB != 0 || p.Files != 1 || p.attachBytes() != 0 {
		t.Errorf("default is no attachment: %+v, %v", p, err)
	}
	p, err = parseMailParams("size=1-2&attach=100-200&files=3")
	if err != nil || p.Files != 3 || p.AttachMinKB != 100 || p.AttachMaxKB != 200 {
		t.Errorf("attach: %+v, %v", p, err)
	}
	if n := p.attachBytes(); n < 100*1024 || n > 200*1024 {
		t.Errorf("attachment size = %d bytes", n)
	}
	for _, bad := range []string{"attach=x", "attach=-5", "files=0", "files=11", "files=x", "attach=5000&files=3", "size=10000&attach=1000"} {
		if _, err := parseMailParams(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParseMailAttachType(t *testing.T) {
	p, err := parseMailParams("attach=1-2")
	if err != nil || p.AttachBinary {
		t.Errorf("text is the default: %+v, %v", p, err)
	}
	p, err = parseMailParams("attach=1-2&attach_type=bin")
	if err != nil || !p.AttachBinary {
		t.Errorf("bin: %+v, %v", p, err)
	}
	p, err = parseMailParams("attach_type=txt")
	if err != nil || p.AttachBinary {
		t.Errorf("txt: %+v, %v", p, err)
	}
	if _, err := parseMailParams("attach_type=pdf"); err == nil {
		t.Error("attach_type=pdf must be rejected")
	}
}

func TestParseMailSaveSent(t *testing.T) {
	p, err := parseMailParams("")
	if err != nil || p.SaveSent {
		t.Errorf("no copy in Sent Items is the default: %+v, %v", p, err)
	}
	p, err = parseMailParams("cleanup=no")
	if err != nil || p.Cleanup || p.SaveSent {
		t.Errorf("cleanup=no must not save a copy in Sent Items by itself: %+v, %v", p, err)
	}
	p, err = parseMailParams("save_sent=yes")
	if err != nil || !p.SaveSent || !p.Cleanup {
		t.Errorf("save_sent=yes: %+v, %v", p, err)
	}
	if _, err := parseMailParams("save_sent=maybe"); err == nil {
		t.Error("save_sent=maybe must be rejected")
	}
}
