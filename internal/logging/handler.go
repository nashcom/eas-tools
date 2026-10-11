package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Formats are the values of the log_format setting.
var Formats = []string{"text", "json"}

// State is what can change while a program runs: the level and the format of the log. Every logger made with New reads
// it for each message, so a reloaded configuration can change both without a new logger.
type State struct {
	level slog.LevelVar
	json  atomic.Bool
}

// NewState returns a state at the given level in text format.
func NewState(level slog.Level) *State {
	s := &State{}
	s.level.Set(level)
	return s
}

func (s *State) SetLevel(l slog.Level) { s.level.Set(l) }
func (s *State) Level() slog.Level     { return s.level.Level() }

// SetFormat sets "text" or "json"; an empty name is text.
func (s *State) SetFormat(name string) error {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "text":
		s.json.Store(false)
	case "json":
		s.json.Store(true)
	default:
		return fmt.Errorf("log_format %q: must be %s", name, strings.Join(Formats, ", "))
	}
	return nil
}

// Format returns "text" or "json".
func (s *State) Format() string {
	if s.json.Load() {
		return "json"
	}
	return "text"
}

// ParseFormat checks a log_format value.
func ParseFormat(name string) error { return (&State{}).SetFormat(name) }

// New returns a logger that writes to w in the format of the state. The text format is one line per message:
//
//	2026-10-10T15:17:14.251Z INFO configuration reloaded source=https://example.com/c.json targets=[lab]
//
// the time in UTC, the level, the message and then the attributes as key=value, with no prefix for the first three. The
// json format is one object per line in the shape of the log records of the Domino tools: the field names of OpenTelemetry's log
// data model (as in the protobuf definition), the program as the "scope" and "resource", and the attributes of the message in
// "attributes":
//
//	{"time_unix_nano":"1791645434251000000","observed_time_unix_nano":"1791645434251000000","severity_number":9,
//	 "severity_text":"INFO","body":"configuration reloaded","scope":{"name":"easprobe","version":"0.9.0"},
//	 "resource":{"service.name":"eas-blackbox-exporter","service.version":"0.9.0","host.name":"h","os.type":"linux",...},
//	 "attributes":{"source":"https://example.com/c.json","targets":["lab"]}}
func New(w io.Writer, state *State, service, version string) *slog.Logger {
	return slog.New(&handler{out: &output{w: w}, state: state, version: version, resource: resourceJSON(service, version)})
}

type output struct {
	mu sync.Mutex
	w  io.Writer
}

type handler struct {
	out      *output
	state    *State
	version  string
	resource string      // the resource of the json format: the program, the host and the process
	attrs    []slog.Attr // from WithAttrs, with the group prefix applied
	prefix   string      // "group." for WithGroup
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.state.level.Level() }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), prefixed(h.prefix, as)...)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

// prefixed puts the group prefix before the keys and flattens the groups.
func prefixed(prefix string, as []slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range as {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			p := prefix
			if a.Key != "" {
				p += a.Key + "."
			}
			out = append(out, prefixed(p, v.Group())...)
			continue
		}
		if a.Key == "" {
			continue
		}
		out = append(out, slog.Attr{Key: prefix + a.Key, Value: v})
	}
	return out
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, prefixed(h.prefix, []slog.Attr{a})...)
		return true
	})

	var b strings.Builder
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	t = t.UTC()
	if h.state.json.Load() {
		writeJSON(&b, t, r, h, attrs)
	} else {
		writeText(&b, t, r, attrs)
	}
	b.WriteByte('\n')

	h.out.mu.Lock()
	defer h.out.mu.Unlock()
	_, err := io.WriteString(h.out.w, b.String())
	return err
}

// levelText is the name of a level in the log, five characters wide in the text format.
func levelText(l slog.Level) string {
	switch {
	case l <= LevelTrace:
		return "TRACE"
	case l <= slog.LevelDebug:
		return "DEBUG"
	case l <= slog.LevelInfo:
		return "INFO"
	case l <= slog.LevelWarn:
		return "WARN"
	}
	return "ERROR"
}

// severityNumber is the number of the level in OpenTelemetry's scale (TRACE 1, DEBUG 5, INFO 9, WARN 13, ERROR 17).
func severityNumber(l slog.Level) int {
	switch {
	case l <= LevelTrace:
		return 1
	case l <= slog.LevelDebug:
		return 5
	case l <= slog.LevelInfo:
		return 9
	case l <= slog.LevelWarn:
		return 13
	}
	return 17
}

func writeText(b *strings.Builder, t time.Time, r slog.Record, attrs []slog.Attr) {
	b.WriteString(t.Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte(' ')
	lt := levelText(r.Level)
	b.WriteString(lt)
	for i := len(lt); i < 5; i++ {
		b.WriteByte(' ')
	}
	b.WriteByte(' ')
	b.WriteString(r.Message) // as it is: a message of several lines stays readable
	for _, a := range attrs {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(quoteIfNeeded(valueText(a.Value)))
	}
}

func valueText(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return x.Error()
		case fmt.Stringer:
			return x.String()
		default:
			return fmt.Sprint(x)
		}
	}
	return v.String()
}

// quoteIfNeeded quotes a value that would break the key=value form: empty, with a space, a quote, an equals sign or a
// character that cannot be printed.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	for _, c := range s {
		if c == ' ' || c == '"' || c == '=' || !unicode.IsPrint(c) {
			return strconv.Quote(s)
		}
	}
	return s
}

func writeJSON(b *strings.Builder, t time.Time, r slog.Record, h *handler, attrs []slog.Attr) {
	ns := strconv.FormatInt(t.UnixNano(), 10)
	b.WriteString(`{"time_unix_nano":"`)
	b.WriteString(ns)
	b.WriteString(`","observed_time_unix_nano":"`)
	b.WriteString(ns) // the time the message was made is also the time it was observed
	b.WriteString(`","severity_number":`)
	b.WriteString(strconv.Itoa(severityNumber(r.Level)))
	b.WriteString(`,"severity_text":`)
	b.WriteString(jsonString(levelText(r.Level)))
	b.WriteString(`,"body":`)
	b.WriteString(jsonString(r.Message))
	b.WriteString(`,"scope":{"name":"easprobe","version":`)
	b.WriteString(jsonString(h.version))
	b.WriteString(`},"resource":`)
	b.WriteString(h.resource)
	b.WriteString(`,"attributes":{`)
	for i, a := range attrs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(a.Key))
		b.WriteByte(':')
		b.WriteString(jsonValue(a.Value))
	}
	b.WriteString("}}")
}

// resourceJSON describes the program that writes the log, with the attribute names of OpenTelemetry's resource conventions.
func resourceJSON(service, version string) string {
	host, _ := os.Hostname()
	exe, _ := os.Executable()
	var b strings.Builder
	b.WriteString(`{"service.name":` + jsonString(service))
	b.WriteString(`,"service.version":` + jsonString(version))
	b.WriteString(`,"service.instance.id":` + jsonString(host))
	b.WriteString(`,"host.name":` + jsonString(host))
	b.WriteString(`,"os.type":` + jsonString(runtime.GOOS))
	b.WriteString(`,"process.executable.name":` + jsonString(strings.TrimSuffix(filepath.Base(exe), ".exe")))
	b.WriteString(`,"process.pid":` + strconv.Itoa(os.Getpid()) + `}`)
	return b.String()
}

func jsonString(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func jsonValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return jsonString(v.String())
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool:
		out, err := json.Marshal(v.Any())
		if err != nil { // NaN and infinity
			return jsonString(v.String())
		}
		return string(out)
	case slog.KindTime, slog.KindDuration:
		return jsonString(valueText(v))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return jsonString(x.Error())
		case fmt.Stringer:
			return jsonString(x.String())
		default:
			if out, err := json.Marshal(x); err == nil {
				return string(out)
			}
			return jsonString(fmt.Sprint(x))
		}
	}
	return jsonString(v.String())
}
