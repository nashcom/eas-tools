// Package logging holds the log level that all programs share, with the names and the order of OpenTelemetry. From the
// quietest: warn (problems only), info (the default: normal operational events such as the start and a changed
// configuration), debug (the routine activity) and trace (very detailed). Go's slog has the first three; trace is a
// level below debug, as in OpenTelemetry. On the command line -verbose selects debug and -debug selects trace; in a
// configuration file or the environment the setting is called log_level.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// LevelTrace is very detailed output: more detailed than debug, like TRACE in OpenTelemetry.
const LevelTrace = slog.Level(-8)

// Names are the values of the log_level setting, from the quietest.
var Names = []string{"warn", "info", "debug", "trace"}

// Parse reads a level name. An empty name is def.
func Parse(s string, def slog.Level) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	}
	return def, fmt.Errorf("log_level %q: must be %s", s, strings.Join(Names, ", "))
}

// Name is the name of a level that Parse accepts.
func Name(l slog.Level) string {
	switch {
	case l <= LevelTrace:
		return "trace"
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	}
	return "warn"
}

// Pick returns the level from the log_level setting (def when it is empty), where the command line wins: -debug selects
// trace, and -verbose selects debug.
func Pick(setting string, verbose, debug bool, def slog.Level) (slog.Level, error) {
	if debug {
		return LevelTrace, nil
	}
	if verbose {
		return slog.LevelDebug, nil
	}
	return Parse(setting, def)
}

// Trace logs very detailed output.
func Trace(log *slog.Logger, msg string, args ...any) {
	log.Log(context.Background(), LevelTrace, msg, args...)
}

// DebugAsTrace returns a logger for a library that logs its details as debug: those messages become trace, so that the
// debug level stays for our own routine messages. Everything else passes unchanged.
func DebugAsTrace(l *slog.Logger) *slog.Logger {
	if l == nil {
		return nil
	}
	return slog.New(&traceHandler{l.Handler()})
}

type traceHandler struct{ inner slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if l == slog.LevelDebug {
		l = LevelTrace
	}
	return h.inner.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelDebug {
		r.Level = LevelTrace
	}
	return h.inner.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &traceHandler{h.inner.WithAttrs(as)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{h.inner.WithGroup(name)}
}
