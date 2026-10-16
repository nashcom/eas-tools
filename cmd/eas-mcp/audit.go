package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/internal/logging"
)

// The audit log is a record of what the server was asked to do and what it did, for the people who have to answer for it: who (the user of the
// login, or the local token), what (the tool, the account, the item, the recipients of a mail or of an invitation), when, and with what result.
// It is optional (-audit-log), separate from the log of the program, and always one JSON object per line in the log data model of OpenTelemetry
// (the format of -log-format json), so that a collector, Alloy or otelfwd can read the file as it is. The attributes are named after the
// semantic conventions where there is one (event.name, enduser.id, client.address, user_agent.original).
//
// What is never in it: the text of a mail, the subject, the description of an event, notes, passwords, tokens. The recipients are: a mail that
// leaves the house is what an audit has to show.

// auditor writes the audit records; a nil auditor writes nothing.
type auditor struct {
	log *slog.Logger
	w   *auditWriter
}

// auditWriter is the destination: a file that can be opened again (after the rotation of the log), or a stream.
type auditWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	out  io.Writer
}

func (w *auditWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// reopen opens the file again, for the rotation of the log: the tool that rotates renames the file, and a SIGHUP makes the program write to the
// new one. A destination that is a stream has nothing to reopen.
func (w *auditWriter) reopen() error {
	if w.path == "" {
		return nil
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.mu.Lock()
	old := w.f
	w.f, w.out = f, f
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// newAuditor opens the audit log. target is a file (appended to, only the owner may read it), or "stderr", or "stdout" (not with the transport stdio:
// stdout is the channel of the protocol); empty means no audit log, and the auditor is nil.
func newAuditor(target, version string, stdio bool) (*auditor, error) {
	if target == "" {
		return nil, nil
	}
	w := &auditWriter{}
	switch target {
	case "stderr":
		w.out = os.Stderr
	case "stdout":
		if stdio {
			return nil, errors.New("-audit-log stdout cannot be used with -transport stdio: stdout is the channel of the protocol; use a file or stderr")
		}
		w.out = os.Stdout
	default:
		w.path = target
		if err := w.reopen(); err != nil {
			return nil, fmt.Errorf("-audit-log: %w", err)
		}
	}
	state := logging.NewState(slog.LevelInfo)
	_ = state.SetFormat("json")
	return &auditor{log: logging.New(w, state, "eas-mcp-audit", version), w: w}, nil
}

// event writes one record. name is the event.name; level says what it was: info for what was done, warn for what was refused or failed.
func (a *auditor) event(ctx context.Context, level slog.Level, name, body string, attrs ...any) {
	if a == nil {
		return
	}
	a.log.Log(ctx, level, body, append([]any{"event.name", name}, attrs...)...)
}

// close closes the file; the records after it go nowhere. It is for the end of the program, and for the tests.
func (a *auditor) close() {
	if a == nil {
		return
	}
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.w.f != nil {
		_ = a.w.f.Close()
		a.w.f = nil
		a.w.out = io.Discard
	}
}

// reopen is called on SIGHUP.
func (a *auditor) reopen() error {
	if a == nil {
		return nil
	}
	return a.w.reopen()
}

// recipientsOf are the arguments of a tool call that name the people that a mail or an invitation goes to, and the items that are touched: the
// fields that an audit needs. Everything else, the text above all, stays out.
func auditArguments(tool string, raw json.RawMessage) []any {
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return nil
	}
	var out []any
	for _, k := range []string{"account", "folder", "id", "destination"} {
		if s, ok := args[k].(string); ok && s != "" {
			out = append(out, "mcp."+k, s)
		}
	}
	switch tool {
	case "send_message", "forward_message":
		if to := stringList(args["to"]); len(to) > 0 {
			out = append(out, "mail.to", to)
		}
		if cc := stringList(args["cc"]); len(cc) > 0 {
			out = append(out, "mail.cc", cc)
		}
	case "reply_message":
		if all, _ := args["all"].(bool); all {
			out = append(out, "mail.reply_all", true)
		}
	case "create_event":
		var who []string
		if list, ok := args["attendees"].([]any); ok {
			for _, e := range list {
				if m, ok := e.(map[string]any); ok {
					if s, ok := m["email"].(string); ok && s != "" {
						who = append(who, s)
					}
				}
			}
		}
		if len(who) > 0 {
			out = append(out, "event.attendees", who)
		}
	}
	return out
}

func stringList(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// toolCall records a call of a tool: the result is success or failure, and a failure has the error.
func (a *auditor) toolCall(ctx context.Context, req mcp.Request, tool string, failure string, took int64, who []any) {
	if a == nil {
		return
	}
	attrs := append([]any{"rpc.method", "tools/call", "mcp.tool", tool, "duration_ms", took}, who...)
	if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
		attrs = append(attrs, auditArguments(tool, p.Arguments)...)
	}
	if failure != "" {
		a.event(ctx, slog.LevelWarn, "mcp.tool.call", "a tool call failed", append(attrs, "outcome", "failure", "error.message", failure)...)
		return
	}
	a.event(ctx, slog.LevelInfo, "mcp.tool.call", "a tool was called", append(attrs, "outcome", "success")...)
}
