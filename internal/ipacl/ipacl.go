// Package ipacl restricts a listener to a list of addresses and networks. The list is optional: no entry means no restriction. It
// guards every request, before the router and before any token. The loopback interface is always allowed, so that the health check of a
// container, which comes from there, keeps working.
//
// The address that counts is the one of the connection (RemoteAddr). Behind a reverse proxy that is the address of the proxy, not of the
// client: put the proxy in the list, or restrict the proxy itself. (A proxy on the same host connects from loopback, which is always
// allowed, and so everything that it passes on is allowed too.)
package ipacl

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// rule is a range of addresses of one family, both ends included.
type rule struct{ lo, hi netip.Addr }

// List is the list of allowed addresses. A nil list allows everything.
type List struct {
	rules   []rule
	entries []string
}

// the networks that "private" stands for: the ones of RFC 1918 and the unique local addresses of IPv6
var privateNetworks = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// Parse reads the entries. An entry is
//
//	10.1.2.3                       one address (IPv4 or IPv6)
//	192.168.0.0/16, 2001:db8::/32  a network
//	10.0.0.5-10.0.0.20             a range, both ends included
//	private                        10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 and fc00::/7
//	loopback                       accepted and not needed: the loopback interface is always allowed
//
// Several entries can be in one text, separated by commas. Blank entries are skipped. A wrong entry is an error that names it.
func Parse(entries []string) (*List, error) {
	l := &List{}
	for _, e := range entries {
		for _, part := range strings.Split(e, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			rs, text, err := parseOne(part)
			if err != nil {
				return nil, fmt.Errorf("entry %q: %v", part, err)
			}
			l.rules = append(l.rules, rs...)
			l.entries = append(l.entries, text)
		}
	}
	return l, nil
}

func parseOne(s string) ([]rule, string, error) {
	switch strings.ToLower(s) {
	case "loopback":
		return nil, "loopback", nil
	case "private":
		var rs []rule
		for _, n := range privateNetworks {
			p := netip.MustParsePrefix(n)
			rs = append(rs, rule{p.Addr(), lastAddr(p)})
		}
		return rs, "private", nil
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, "", fmt.Errorf("not a network: %v", err)
		}
		p = p.Masked()
		return []rule{{p.Addr(), lastAddr(p)}}, p.String(), nil
	}
	if strings.Contains(s, "-") {
		lo, hi, ok := strings.Cut(s, "-")
		if !ok || strings.Contains(hi, "-") {
			return nil, "", fmt.Errorf("a range is two addresses with a dash")
		}
		a, err := netip.ParseAddr(strings.TrimSpace(lo))
		if err != nil {
			return nil, "", fmt.Errorf("not a range: %v", err)
		}
		b, err := netip.ParseAddr(strings.TrimSpace(hi))
		if err != nil {
			return nil, "", fmt.Errorf("not a range: %v", err)
		}
		a, b = a.Unmap().WithZone(""), b.Unmap().WithZone("")
		if a.Is4() != b.Is4() {
			return nil, "", fmt.Errorf("the two ends of a range have to be of the same kind (IPv4 or IPv6)")
		}
		if a.Compare(b) > 0 {
			return nil, "", fmt.Errorf("the range ends before it starts")
		}
		return []rule{{a, b}}, a.String() + "-" + b.String(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return nil, "", fmt.Errorf("not an address, a network or a range: %v", err)
	}
	a = a.Unmap().WithZone("")
	return []rule{{a, a}}, a.String(), nil
}

// lastAddr is the last address of a network.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// Empty reports whether there is no entry: then nothing is restricted. A list with the entry "loopback" is not empty: it allows the
// loopback interface and nothing else.
func (l *List) Empty() bool { return l == nil || len(l.entries) == 0 }

// Entries returns the entries as they are used, one form for each.
func (l *List) Entries() []string {
	if l == nil {
		return nil
	}
	return append([]string(nil), l.entries...)
}

// Allows reports whether a peer may connect: always for loopback, otherwise when an entry has it. An address of IPv4 that is written as
// IPv6 (::ffff:10.0.0.5) counts as IPv4. An empty list allows everything.
func (l *List) Allows(addr netip.Addr) bool {
	if l.Empty() {
		return true
	}
	addr = addr.Unmap().WithZone("")
	if addr.IsLoopback() {
		return true
	}
	for _, r := range l.rules {
		if addr.Is4() == r.lo.Is4() && addr.Compare(r.lo) >= 0 && addr.Compare(r.hi) <= 0 {
			return true
		}
	}
	return false
}

// remoteAddr reads the address of the peer of a request.
func remoteAddr(remote string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr(), true
	}
	if a, err := netip.ParseAddr(remote); err == nil {
		return a, true
	}
	return netip.Addr{}, false
}

// Wrap makes a handler that refuses a request whose peer is not allowed, with 403 and a text that says nothing about the list. denied
// is called with the address of the peer of every refused request (it may be nil). An empty list gives the handler itself.
func (l *List) Wrap(next http.Handler, denied func(remote string, r *http.Request)) http.Handler {
	if l.Empty() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a, ok := remoteAddr(r.RemoteAddr); ok && l.Allows(a) {
			next.ServeHTTP(w, r)
			return
		}
		if denied != nil {
			denied(r.RemoteAddr, r)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden\n"))
	})
}
