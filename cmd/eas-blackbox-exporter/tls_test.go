package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// The health check of the container asks /health of the metrics port on loopback: no certificate is involved, also with TLS.
func TestHealthCheckAsksTheMetricsPort(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	plainLn, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = http.Serve(plainLn, mux) }()
	defer plainLn.Close()

	free, _ := net.Listen("tcp", "127.0.0.1:0") // a port where nothing listens
	gone := free.Addr().String()
	free.Close()
	if code := checkHealth(gone); code != 1 {
		t.Errorf("nothing listens: %d, want 1", code)
	}
	if code := checkHealth(plainLn.Addr().String()); code != 0 {
		t.Errorf("plain: %d", code)
	}
}

func TestHealthCheckKnowsFromTheConfigWhereToAsk(t *testing.T) {
	dir := t.TempDir()
	targets := `"targets": [{"name": "lab", "server": "x.example.com", "user": "u", "password": "p"}]`
	cfg := filepath.Join(dir, "config.json")
	writeFile(t, cfg, `{"listen": ":9443", "tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", `+targets+`}`)
	if got := healthCheckTarget(cfg); got != ":9100" {
		t.Errorf("TLS config: %q, want the metrics port", got)
	}
	named := filepath.Join(dir, "named.json")
	writeFile(t, named, `{"listen": ":9443", "tls_cert": "/c/tls.crt", "tls_key": "/c/tls.key", "listen_metrics": "0.0.0.0:9000", "allow_from_metrics": ["10.1.2.0/24"], `+targets+`}`)
	if got := healthCheckTarget(named); got != "0.0.0.0:9000" {
		t.Errorf("named metrics port: %q", got)
	}
	plain := filepath.Join(dir, "plain.json")
	writeFile(t, plain, `{"listen": ":9116", `+targets+`}`)
	if got := healthCheckTarget(plain); got != ":9100" {
		t.Errorf("config without TLS: %q, the metrics port is there as well", got)
	}
	t.Setenv("EASEXPORTER_TLS_CERT", "/c/tls.crt")
	t.Setenv("EASEXPORTER_LISTEN", ":9443")
	if got := healthCheckTarget(filepath.Join(dir, "missing.json")); got != ":9100" {
		t.Errorf("the environment says TLS: %q", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A check of the configuration, by the timer or on request, looks at the certificate files as well, also when the configuration itself
// did not change.
func TestConfigCheckAlsoChecksTheCertificate(t *testing.T) {
	t.Setenv("EASEXPORTER_CONFIG_CHECK_INTERVAL_SECONDS", "1")
	dir := t.TempDir()
	o := certreload.GenerateOptions{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	if _, err := certreload.Generate(o); err != nil {
		t.Fatal(err)
	}
	store, err := certreload.New(o.CertFile, o.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.Settle = 10 * time.Millisecond
	first := store.Info().SHA256

	path := filepath.Join(dir, "config.json")
	writeFile(t, path, docOne)
	cs, err := startConfig(path, quiet)
	if err != nil || cs.source == nil {
		t.Fatalf("start: %v %+v", err, cs)
	}
	srv := startServer(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reloadLoop(ctx, cs, srv, store, path, quiet) // no Watch: only the configuration check looks at the certificate

	// a new certificate in the same places; the configuration file stays as it is
	for _, f := range []string{o.CertFile, o.KeyFile, filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")} {
		_ = os.Remove(f)
	}
	if _, err := certreload.Generate(certreload.GenerateOptions{CertFile: o.CertFile, KeyFile: o.KeyFile, Names: []string{"other.example.com"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, srv, "the new certificate", func() bool { return store.Info().SHA256 != first })
	if !strings.Contains(metrics(srv), "eas_probe_config_reloads_total 0") {
		t.Error("the configuration was reloaded although only the certificate changed")
	}
}
