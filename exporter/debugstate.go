package exporter

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// The debug request /debug/state shows the current values in memory as JSON: what the exporter does right now, which
// results it keeps, the state of each target and of the mail monitor with the single values behind its averages. It is
// not part of /metrics. It is always on and follows the rule of /probe: with an auth_token configured the request needs it,
// without one it is open. It never shows a password or a token.

// DebugSection adds a part to the answer of /debug/state, for what only the command knows, such as the log level.
func (s *Server) DebugSection(name string, f func() any) {
	s.debugMu.Lock()
	if s.debugSections == nil {
		s.debugSections = map[string]func() any{}
	}
	s.debugSections[name] = f
	s.debugMu.Unlock()
}

type targetDebug struct {
	Running      bool                `json:"running"`
	AuthBackoff  *string             `json:"auth_backoff_until,omitempty"`
	CachedProbes map[string]cacheAge `json:"cached_probes"`
}

type cacheAge struct {
	At         string  `json:"cached_at"`
	AgeSeconds float64 `json:"age_seconds"`
	Bytes      int     `json:"bytes"`
}

func kindName(k probeKind) string {
	name := "plain"
	if k.ping > 0 {
		name = "ping"
	}
	if k.mail {
		name = "mail"
		if k.ping > 0 {
			name = "mail+ping"
		}
	}
	return name
}

func (s *Server) handleDebugState(w http.ResponseWriter, r *http.Request) {
	l := s.cur.Load()
	if !s.authorized(l, r) { // open when no token is configured, like /probe
		w.Header().Set("WWW-Authenticate", `Bearer realm="eas-probe"`)
		http.Error(w, "Unauthorized\n", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Only GET is supported\n", http.StatusMethodNotAllowed)
		return
	}

	now := time.Now()
	targets := map[string]targetDebug{}
	for name, ts := range l.targets {
		d := targetDebug{CachedProbes: map[string]cacheAge{}, Running: len(ts.lock) > 0}
		ts.mu.Lock()
		for k, c := range ts.cache {
			d.CachedProbes[kindName(k)] = cacheAge{At: nanoTime(c.at), AgeSeconds: secondsOf(now.Sub(c.at)), Bytes: len(c.body)}
		}
		if now.Before(ts.blockedUntil) {
			v := nanoTime(ts.blockedUntil)
			d.AuthBackoff = &v
		}
		ts.mu.Unlock()
		targets[name] = d
	}

	configured := make([]string, 0, len(l.cfg.Targets))
	for n := range l.cfg.Targets {
		configured = append(configured, n)
	}
	sort.Strings(configured)

	out := map[string]any{
		"time": nanoTime(now),
		"exporter": map[string]any{
			"version":        probe.Version,
			"started":        nanoTime(s.started),
			"uptime_seconds": secondsOf(now.Sub(s.started)),
			"targets":        configured,
			"devices":        s.store.Len(),
			"counters": map[string]int64{
				"probes": s.probes.Load(), "probe_errors": s.probeErrors.Load(), "cache_hits": s.cacheHits.Load(),
				"queue_rejected": s.rejected.Load(), "denied_requests": s.denied.Load(), "config_reloads": s.reloads.Load(), "config_reload_errors": s.reloadErrors.Load(),
			},
			"max_concurrent": cap(l.sem), "probes_running": len(l.sem), "allow_from": l.acl.Entries(),
			"listen": l.cfg.Listen, "listen_metrics": l.cfg.ListenMetrics, "allow_from_metrics": l.aclMetrics.Entries(), "allow_http": l.cfg.AllowHTTP,
			"config_loaded":  nanoTime(l.loadedAt),
			"config_checked": unixOrNil(s.lastCheck.Load()),
			"config_status":  configStatus(s.configErr.Load()),
		},
		"targets":      targets,
		"mail_monitor": s.rm.Debug(),
	}
	if st := s.tls.Load(); st != nil {
		in := st.Info()
		out["tls"] = map[string]any{"certificate_file": in.CertFile, "key_file": in.KeyFile, "subject": in.Subject, "issuer": in.Issuer, "dns_names": in.DNSNames, "ips": in.IPs,
			"key_type": in.KeyType, "sha256": in.SHA256, "not_before": nanoTime(in.NotBefore), "not_after": nanoTime(in.NotAfter), "loaded": nanoTime(in.LoadedAt),
			"reloads": in.Reloads, "reload_errors": in.Failures,
			"listen": l.cfg.Listen, "listen_metrics": l.cfg.ListenMetrics, "allow_from_metrics": l.aclMetrics.Entries()}
	}
	s.debugMu.Lock()
	for name, f := range s.debugSections {
		out[name] = f()
	}
	s.debugMu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// configStatus is the current status of the configuration source: ok, or the reason of the failure and since when.
func configStatus(e *configError) map[string]any {
	if e == nil {
		return map[string]any{"status": 0, "ok": true}
	}
	return map[string]any{"status": 1, "ok": false, "error": e.text, "since": nanoTime(e.at)}
}

func unixOrNil(sec int64) any {
	if sec == 0 {
		return nil
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// nanoTime formats a time in UTC with all the digits it has (nanoseconds, where the clock has them); secondsOf gives a
// duration in seconds, unrounded.
func nanoTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func secondsOf(d time.Duration) float64 { return d.Seconds() }
