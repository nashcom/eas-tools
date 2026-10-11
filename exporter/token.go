package exporter

import (
	"fmt"
	"net/http"
	"strings"
)

// A token travels in an HTTP header. By default that is "Authorization: Bearer <value>". When that header is already used
// for something else (a Domino server, a proxy with its own login), the token can have its own header name, and then the
// header holds just the value: "X-Auth-Token: <value>".
const defaultTokenHeader = "Authorization"

// tokenHeader returns the header name for a configured name; empty means the default. A name that cannot be a header
// name, or one that is used for other things, is refused.
func tokenHeader(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultTokenHeader, nil
	}
	for _, r := range name {
		ok := r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !ok {
			return "", fmt.Errorf("token name %q: only letters, digits, '-' and '_' are allowed", name)
		}
	}
	switch c := http.CanonicalHeaderKey(name); c {
	case "Cookie", "Set-Cookie", "Host", "Content-Length", "Content-Type", "Proxy-Authorization", "User-Agent", "Accept":
		return "", fmt.Errorf("token name %q: this header is used for other things", name)
	default:
		return c, nil
	}
}

// setToken puts the token in the header of an outgoing request.
func setToken(h http.Header, name, token string) {
	header, err := tokenHeader(name)
	if err != nil {
		return // the name was checked when the settings were loaded
	}
	if header == defaultTokenHeader {
		h.Set(header, "Bearer "+token)
		return
	}
	h.Set(header, token)
}

// getToken returns the token of an incoming request, "" if there is none in the header with this name.
func getToken(r *http.Request, name string) string {
	header, err := tokenHeader(name)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(r.Header.Get(header))
	if header != defaultTokenHeader {
		return v
	}
	if len(v) > len("Bearer ") && strings.EqualFold(v[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(v[len("Bearer "):])
	}
	return ""
}
