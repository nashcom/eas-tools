// Package listenaddr holds the rules for the narrow listener that runs next to the main listener of the exporter and of the client:
// the metrics port of the exporter, and the health port of the client.
//
// The main listener serves everything, HTTPS with a certificate. The narrow one serves plain HTTP, but only what needs no protection
// (the programs decide what that is): it is for the health check of a container, for a local agent and for a scraper that you name.
// Its address is on the loopback interface unless it is set to something else on purpose, and then it needs a list of the peers that
// may connect.
package listenaddr

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Loopback reports whether the address listens on the loopback interface only.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// DefaultNext is the address of the second listener when none is given: the port after the one of the main listener (":9116" gives
// ":9117"). It is open (the interfaces of the main listener) when open is true, as the metrics port is, and the loopback interface otherwise
// ("127.0.0.1:9117"), as the health port of the client is. Port 0 stays 0, so that a test or a try gets two free ports.
func DefaultNext(listen string, open bool) (string, error) {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 0 || port > 65534 {
		return "", fmt.Errorf("listen address %q: the second listener uses the next port, %q has none", listen, p)
	}
	if port > 0 {
		port++
	}
	host := "127.0.0.1"
	if open {
		host, _, _ = net.SplitHostPort(listen)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// Second returns the address of the second listener (the default when addr is empty) and checks it: it must differ from the main
// address. When it is not open (the health port of the client), an address that is not loopback needs a list of peers (allowed), since
// the traffic is not encrypted and the listener would otherwise be open to everyone who can reach the port. An open one (the metrics
// port) is open by default and the list restricts it. listName is the setting of the list, for the message.
func Second(listen, addr string, allowed []string, listName string, open bool) (string, error) {
	if addr == "" {
		return DefaultNext(listen, open)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("listen address %q: %w", addr, err)
	}
	if addr == listen {
		return "", fmt.Errorf("the address %q is the one of the main listener: it needs a port of its own", addr)
	}
	if !open && !Loopback(addr) && len(allowed) == 0 {
		return "", fmt.Errorf("the address %s is not loopback: it needs the list of the peers that may connect (%s), "+
			"for example the address of Prometheus; without it everyone who reaches the port reads unencrypted", addr, listName)
	}
	return addr, nil
}

// NeedsHTTPSwitch is the rule for the main listener without a certificate: serving everything over plain HTTP has to be chosen
// (allow_http). A loopback address needs no choice, it never leaves the machine.
func NeedsHTTPSwitch(listen string, haveCert, allowHTTP bool) error {
	if haveCert || allowHTTP || Loopback(listen) {
		return nil
	}
	return fmt.Errorf("%s is not a loopback address and there is no certificate: everything would be served over plain HTTP. "+
		"Give a certificate (tls_cert and tls_key), or choose plain HTTP on purpose (allow_http), for example behind a proxy that terminates TLS", listen)
}

// PlainWhy says why a main listener has no TLS, for the line of the start: the choice (allow_http), or the address (loopback). Empty when
// there is a reason that is neither, which the checks of the start do not allow.
func PlainWhy(listen string, allowHTTP bool) string {
	switch {
	case allowHTTP:
		return "allow_http"
	case Loopback(listen):
		return "listen is loopback"
	}
	return ""
}

// HealthPort is the port where the running program answers /health in plain HTTP on the loopback interface, for the health check of a
// container (the option -health-check of the client and of eas-mcp): with TLS it is the port of the health listener (healthAddr, or the
// port after the main one), without TLS it is the main port, which is plain HTTP. TLS is there when there is a certificate, or when the
// address is not loopback and plain HTTP was not chosen (the certificate of /run/secrets/tls is used then).
func HealthPort(listen, healthAddr string, haveCert, allowHTTP bool) (string, error) {
	addr := listen
	if haveCert || (!allowHTTP && !Loopback(listen)) {
		addr = healthAddr
		if addr == "" {
			var err error
			if addr, err = DefaultNext(listen, false); err != nil {
				return "", err
			}
		}
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("address %q: %w", addr, err)
	}
	return port, nil
}

// CheckHealth asks http://127.0.0.1:PORT/health and returns the exit code of a health check: 0 for 200, and 1 otherwise, with the reason on w.
func CheckHealth(port string, w io.Writer) int {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintln(w, "health-check:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(w, "health-check: HTTP", resp.Status)
		return 1
	}
	return 0
}
