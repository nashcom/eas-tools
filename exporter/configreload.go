package exporter

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReloadFunc checks the configuration source now. It returns what to send back and the HTTP status.
type ReloadFunc func(ctx context.Context) (any, int)

// reloadMinGap is the shortest time between two requests to /config/reload, so that a script cannot hammer the source.
const reloadMinGap = 5 * time.Second

// SetReloader sets what POST /config/reload does. Without one the request answers 409.
func (s *Server) SetReloader(f ReloadFunc) {
	s.reloaderMu.Lock()
	s.reloader = f
	s.reloaderMu.Unlock()
}

// handleConfigReload is the request to read the configuration source now, so that a change does not wait for the next
// check. It is a POST, because it does something; it has no body. It follows the rule of /probe: with an auth_token
// configured it needs it, without one it is open. It only reads the source that is configured, nothing can be sent with it.
func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	l := s.cur.Load()
	if !s.authorized(l, r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="eas-probe"`)
		http.Error(w, "Unauthorized\n", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use POST: this request does something\n", http.StatusMethodNotAllowed)
		return
	}

	s.reloaderMu.Lock()
	f := s.reloader
	s.reloaderMu.Unlock()
	if f == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"result": "failed", "error": "there is nothing to check"})
		return
	}

	now := time.Now().UnixNano()
	last := s.reloadAsked.Load()
	if now-last < int64(reloadMinGap) || !s.reloadAsked.CompareAndSwap(last, now) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"result": "failed", "error": "the configuration was checked a moment ago, try again in a few seconds"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	body, status := f(ctx)
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}
