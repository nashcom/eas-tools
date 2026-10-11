package probe

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ParseHeaders turns "Name: value" lines (as given with -header) into HTTP
// headers. Authorization and Cookie cannot be set this way: the tools own them.
func ParseHeaders(lines []string) (http.Header, error) {
	h := http.Header{}
	for _, l := range lines {
		name, value, ok := strings.Cut(l, ":")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, fmt.Errorf("header %q: expected \"Name: value\"", l)
		}
		for _, c := range name {
			if !(c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				return nil, fmt.Errorf("header %q: invalid character in the name", l)
			}
		}
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "Cookie", "Host", "Content-Length", "Content-Type":
			return nil, fmt.Errorf("header %q: the %s header is set by the tool and cannot be overridden", l, name)
		}
		h.Add(name, value)
	}
	return h, nil
}

// headerSet holds extra request headers that are added to every request of a
// session. The set can change while the session runs (a load job sets its
// X-Mock-Sim header for the length of the job).
type headerSet struct {
	mu sync.Mutex
	h  http.Header
}

func newHeaderSet(h http.Header) *headerSet {
	return &headerSet{h: h.Clone()}
}

// set replaces the header; an empty value removes it.
func (s *headerSet) set(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.h == nil {
		s.h = http.Header{}
	}
	if value == "" {
		s.h.Del(name)
		return
	}
	s.h.Set(name, value)
}

func (s *headerSet) apply(req *http.Request) *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.h) == 0 {
		return req
	}
	out := req.Clone(req.Context())
	for k, vs := range s.h {
		out.Header.Del(k)
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	return out
}

// headerTransport adds the extra headers to every request. It is the outermost
// transport, so a retried request and the wire trace carry them too.
type headerTransport struct {
	next http.RoundTripper
	set  *headerSet
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(t.set.apply(req))
}

// SetHeader sets (or with an empty value removes) an extra request header of
// this session, for example a simulation directive for the mock server.
func (s *Session) SetHeader(name, value string) {
	s.rec.hdr.set(name, value)
}

// HeaderFlags collects repeated -header options ("Name: value"); it implements
// flag.Value.
type HeaderFlags []string

func (h *HeaderFlags) String() string { return strings.Join(*h, "; ") }

func (h *HeaderFlags) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// SimHeader is the request header that tells the mock server (eas-mock-server
// -sim) how to behave for a request, for example "cmd=Sync,status=3,count=1".
const SimHeader = "X-Mock-Sim"
