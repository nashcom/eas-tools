package listenaddr

import (
	"log/slog"
	"net"
	"strings"
)

// Endpoint is one path of a listener with what it needs.
type Endpoint struct {
	Path   string
	What   string // what it is, in a few words (debug level)
	Auth   string // what a request needs, in a word: none, token
	Detail string // the detail of the Auth, for debug level: the header, where the token comes from
}

// Listener is what the start says about one listener: one short line, with the address, the protocol, who may connect, and what the
// paths need. The explanations (why it is as it is, the interfaces, the detail of the token, what each path is) are at debug level.
// It is written after the port is bound, so that the address is the real one.
type Listener struct {
	Name       string   // main, metrics, health
	Configured string   // the address as it was given or derived
	Bound      net.Addr // the address of the socket
	TLS        bool
	Allow      []string // the list of the peers; empty: no restriction
	Endpoints  []Endpoint
	Note       string
	PlainWhy   string // why there is no TLS, when there is none: allow_http, or a loopback address (shown on the line of the start)
}

// Scope says which interfaces an address listens on.
func Scope(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	switch {
	case Loopback(addr):
		return "loopback only: no other machine can connect"
	case host == "" || (ip != nil && ip.IsUnspecified()):
		return "all interfaces"
	}
	return "the interface " + host
}

func (l Listener) bound() string {
	if l.Bound != nil {
		return l.Bound.String()
	}
	return l.Configured
}

// From says who may connect, in a few characters: open, loopback, or the list (and loopback, which is always in). Everybody else gets 403.
func (l Listener) From() string {
	switch {
	case len(l.Allow) > 0:
		return strings.Join(l.Allow, ",")
	case Loopback(l.bound()):
		return "loopback"
	}
	return "open"
}

// Proto is https, or http for plain HTTP.
func (l Listener) Proto() string {
	if l.TLS {
		return "https"
	}
	return "http"
}

// AuthByPath says what the paths need, grouped: none(/metrics /health) token(/probe /debug/state).
func (l Listener) AuthByPath() string {
	var order []string
	groups := map[string][]string{}
	for _, e := range l.Endpoints {
		if _, ok := groups[e.Auth]; !ok {
			order = append(order, e.Auth)
		}
		groups[e.Auth] = append(groups[e.Auth], e.Path)
	}
	parts := make([]string, 0, len(order))
	for _, a := range order {
		parts = append(parts, a+"("+strings.Join(groups[a], " ")+")")
	}
	return strings.Join(parts, " ")
}

// Log writes the line of the listener, short: one line for each port. At debug level there is more: the interfaces, why the listener
// is as it is, and a line for each endpoint with what it is and the detail of what it needs.
func (l Listener) Log(log *slog.Logger) {
	line := []any{"name", l.Name, "addr", l.bound(), "proto", l.Proto(), "from", l.From(), "auth", l.AuthByPath()}
	if !l.TLS && l.PlainWhy != "" {
		line = append(line, "tls_off", l.PlainWhy)
	}
	log.Info("listener", line...)
	attrs := []any{"name", l.Name, "interfaces", Scope(l.bound())}
	if l.TLS {
		attrs = append(attrs, "tls", "1.2 and newer, h2 and http/1.1")
	}
	if l.Configured != "" && l.Configured != l.bound() {
		attrs = append(attrs, "configured", l.Configured)
	}
	if l.Note != "" {
		attrs = append(attrs, "note", l.Note)
	}
	log.Debug("listener detail", attrs...)
	for _, e := range l.Endpoints {
		log.Debug("endpoint", "listener", l.Name, "path", e.Path, "auth", e.Auth, "detail", e.Detail, "what", e.What)
	}
}
