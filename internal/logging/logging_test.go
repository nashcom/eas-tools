package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "warn": slog.LevelWarn, "WARN": slog.LevelWarn, " info ": slog.LevelInfo,
		"debug": slog.LevelDebug, "trace": LevelTrace,
	} {
		if got, err := Parse(in, slog.LevelInfo); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"verbose", "notice", "error", "3"} {
		if _, err := Parse(bad, slog.LevelInfo); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheLevelsAreOrdered(t *testing.T) {
	if !(LevelTrace < slog.LevelDebug && slog.LevelDebug < slog.LevelInfo && slog.LevelInfo < slog.LevelWarn) {
		t.Fatal("trace < debug < info < warn is broken")
	}
	for _, n := range Names {
		l, err := Parse(n, slog.LevelWarn)
		if err != nil || Name(l) != n {
			t.Errorf("%s: %v %s", n, err, Name(l))
		}
	}
}

func TestCommandLineWinsOverTheSetting(t *testing.T) {
	for _, c := range []struct {
		setting        string
		verbose, debug bool
		want           slog.Level
	}{
		{"", false, false, slog.LevelInfo},
		{"warn", false, false, slog.LevelWarn},
		{"trace", false, false, LevelTrace},
		{"warn", true, false, slog.LevelDebug}, // -verbose is the command line: it wins
		{"warn", false, true, LevelTrace},      // -debug is trace
		{"warn", true, true, LevelTrace},       // both: the more detailed one
	} {
		got, err := Pick(c.setting, c.verbose, c.debug, slog.LevelInfo)
		if err != nil || got != c.want {
			t.Errorf("%+v: %v %v", c, got, err)
		}
	}
	if _, err := Pick("loud", false, false, slog.LevelInfo); err == nil {
		t.Error("a bad setting is accepted")
	}
}

// What each level lets through.
func TestWhatEachLevelShows(t *testing.T) {
	emit := func(level slog.Level) []string {
		var b bytes.Buffer
		log := New(&b, NewState(level), "test", "1")
		Trace(log, "a trace")
		log.Debug("a debug")
		log.Info("an info")
		log.Warn("a warn")
		log.Error("an error")
		var msgs []string
		for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
			if line != "" {
				f := strings.SplitN(line, " ", 3)
				msgs = append(msgs, strings.TrimSpace(f[2]))
			}
		}
		return msgs
	}
	for level, want := range map[slog.Level]string{
		slog.LevelWarn:  "a warn,an error",
		slog.LevelInfo:  "an info,a warn,an error",
		slog.LevelDebug: "a debug,an info,a warn,an error",
		LevelTrace:      "a trace,a debug,an info,a warn,an error",
	} {
		if got := strings.Join(emit(level), ","); got != want {
			t.Errorf("%s shows %q, want %q", Name(level), got, want)
		}
	}
}

var textLine = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z (TRACE|DEBUG|INFO |WARN |ERROR) `)

// The text format: the time in UTC, the level and the message, with no prefix for them, then key=value.
func TestTextFormat(t *testing.T) {
	var b bytes.Buffer
	log := New(&b, NewState(LevelTrace), "svc", "1.2")
	log.Info("configuration reloaded", "source", "https://example.com/c.json", "targets", []string{"lab"}, "n", 3,
		"took", 2890*time.Millisecond, "why", "the time is the same", "empty", "", "err", errors.New("bad: x"))
	Trace(log, "very detailed")
	log.Warn("warn", slog.Group("g", slog.String("a", "1"), slog.Group("h", slog.Int("b", 2))))
	log.With("device", 7).WithGroup("job").Info("status\nline two", "ok", true)

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines:\n%s", len(lines), b.String())
	}
	for _, l := range []string{lines[0], lines[1], lines[2], lines[3]} {
		if !textLine.MatchString(l) {
			t.Errorf("not time level message: %q", l)
		}
		for _, bad := range []string{"time=", "level=", "msg="} {
			if strings.Contains(l, bad) {
				t.Errorf("a prefix %q in %q", bad, l)
			}
		}
	}
	want := `INFO  configuration reloaded source=https://example.com/c.json targets=[lab] n=3 took=2.89s why="the time is the same" empty="" err="bad: x"`
	if !strings.HasSuffix(lines[0], want) {
		t.Errorf("line 1: %q\nwant suffix %q", lines[0], want)
	}
	if !strings.Contains(lines[1], "Z TRACE very detailed") {
		t.Errorf("trace: %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "WARN  warn g.a=1 g.h.b=2") {
		t.Errorf("groups: %q", lines[2])
	}
	// a message of several lines stays readable; WithAttrs and WithGroup apply
	if !strings.HasSuffix(lines[3], "INFO  status") || lines[4] != `line two device=7 job.ok=true` {
		t.Errorf("multi-line: %q / %q", lines[3], lines[4])
	}
}

// The json format: one object per line with OpenTelemetry's names, flat, the time in nanoseconds.
func TestJSONFormat(t *testing.T) {
	var b bytes.Buffer
	st := NewState(LevelTrace)
	if err := st.SetFormat("json"); err != nil {
		t.Fatal(err)
	}
	log := New(&b, st, "eas-blackbox-exporter", "0.1.0")
	before := time.Now().UnixNano()
	log.Info("configuration reloaded", "source", "https://example.com/c.json", "targets", []string{"lab"}, "n", 3,
		"ok", true, "took", 2890*time.Millisecond, "ratio", 0.5, "err", errors.New("bad"))
	after := time.Now().UnixNano()

	var got map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b.String())
	}
	ns, err := strconv.ParseInt(got["time_unix_nano"].(string), 10, 64)
	if err != nil || ns < before || ns > after {
		t.Errorf("time_unix_nano %v (%d..%d)", got["time_unix_nano"], before, after)
	}
	if got["observed_time_unix_nano"] != got["time_unix_nano"] {
		t.Errorf("observed_time_unix_nano %v", got["observed_time_unix_nano"])
	}
	for k, want := range map[string]any{"severity_text": "INFO", "severity_number": float64(9), "body": "configuration reloaded"} {
		if got[k] != want {
			t.Errorf("%s = %#v, want %#v", k, got[k], want)
		}
	}
	scope, _ := got["scope"].(map[string]any)
	if scope["name"] != "easprobe" || scope["version"] != "0.1.0" {
		t.Errorf("scope = %#v", got["scope"])
	}
	res, _ := got["resource"].(map[string]any)
	for k, want := range map[string]any{"service.name": "eas-blackbox-exporter", "service.version": "0.1.0", "os.type": runtime.GOOS, "process.pid": float64(os.Getpid())} {
		if res[k] != want {
			t.Errorf("resource %s = %#v, want %#v", k, res[k], want)
		}
	}
	for _, k := range []string{"host.name", "service.instance.id", "process.executable.name"} {
		if s, _ := res[k].(string); s == "" {
			t.Errorf("resource %s is missing", k)
		}
	}
	attrs, _ := got["attributes"].(map[string]any)
	for k, want := range map[string]any{"source": "https://example.com/c.json", "n": float64(3), "ok": true, "took": "2.89s", "ratio": 0.5, "err": "bad"} {
		if attrs[k] != want {
			t.Errorf("attribute %s = %#v, want %#v", k, attrs[k], want)
		}
	}
	if ts, ok := attrs["targets"].([]any); !ok || len(ts) != 1 || ts[0] != "lab" {
		t.Errorf("targets = %#v", attrs["targets"])
	}
	if strings.Count(strings.TrimSpace(b.String()), "\n") != 0 {
		t.Error("more than one line")
	}

	// the severity numbers of OpenTelemetry, a message of several lines is escaped
	b.Reset()
	Trace(log, "t")
	log.Debug("d")
	log.Warn("w")
	log.Error("e\nsecond line")
	var nums []float64
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %q", err, line)
		}
		nums = append(nums, m["severity_number"].(float64))
		if m["body"] == "e\nsecond line" && m["severity_text"] != "ERROR" {
			t.Error("severity of the error")
		}
	}
	if fmt.Sprint(nums) != "[1 5 13 17]" {
		t.Errorf("severity numbers %v", nums)
	}
}

// The level and the format can be changed while a logger exists, the way a reload does.
func TestLevelAndFormatChangeAtRuntime(t *testing.T) {
	var b bytes.Buffer
	st := NewState(slog.LevelWarn)
	log := New(&b, st, "s", "1")
	log.Info("hidden")
	st.SetLevel(slog.LevelInfo)
	log.Info("shown")
	if err := st.SetFormat("json"); err != nil {
		t.Fatal(err)
	}
	log.Info("as json")
	if err := st.SetFormat("xml"); err == nil {
		t.Error("a bad format is accepted")
	}
	if st.Format() != "json" {
		t.Error("a bad format changed the state")
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "INFO  shown") || !strings.HasPrefix(lines[1], `{"time_unix_nano"`) {
		t.Errorf("%q", lines)
	}
	if ParseFormat("") != nil || ParseFormat("text") != nil || ParseFormat("JSON") != nil || ParseFormat("yaml") == nil {
		t.Error("ParseFormat")
	}
}

func TestSeveralLoggersShareOneWriterSafely(t *testing.T) {
	var b bytes.Buffer
	log := New(&b, NewState(slog.LevelInfo), "s", "1")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := log.With("n", i)
			for j := 0; j < 50; j++ {
				l.Info("message", "j", j)
			}
		}(i)
	}
	wg.Wait()
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if !textLine.MatchString(line) || !strings.Contains(line, " message ") {
			t.Fatalf("a mixed up line: %q", line)
		}
	}
}

// A message without attributes still has the "attributes" object, and the attributes of With and groups end up in it.
func TestJSONAttributesObject(t *testing.T) {
	var b bytes.Buffer
	st := NewState(slog.LevelInfo)
	_ = st.SetFormat("json")
	log := New(&b, st, "s", "1")
	log.Info("plain")
	log.With("device", 7).Info("grouped", slog.Group("g", slog.String("a", "1")))

	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	var m1, m2 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m1); err != nil {
		t.Fatal(err)
	}
	if a, ok := m1["attributes"].(map[string]any); !ok || len(a) != 0 {
		t.Errorf("attributes of a plain message: %#v", m1["attributes"])
	}
	if err := json.Unmarshal([]byte(lines[1]), &m2); err != nil {
		t.Fatal(err)
	}
	a := m2["attributes"].(map[string]any)
	if a["device"] != float64(7) || a["g.a"] != "1" {
		t.Errorf("attributes: %#v", a)
	}
}

// A library that logs its details as debug is seen at the trace level only.
func TestDebugAsTrace(t *testing.T) {
	run := func(level slog.Level) string {
		var b bytes.Buffer
		lib := DebugAsTrace(New(&b, NewState(level), "s", "1")).With("cmd", "Sync")
		lib.Debug("eas request", "bodyBytes", 60)
		lib.Info("eas info")
		lib.Warn("eas warn")
		return b.String()
	}
	if out := run(slog.LevelDebug); strings.Contains(out, "eas request") || !strings.Contains(out, "eas info") || !strings.Contains(out, "eas warn") {
		t.Errorf("debug level:\n%s", out)
	}
	out := run(LevelTrace)
	if !strings.Contains(out, "TRACE eas request cmd=Sync bodyBytes=60") {
		t.Errorf("trace level:\n%s", out)
	}
	if DebugAsTrace(nil) != nil {
		t.Error("nil logger")
	}
}
