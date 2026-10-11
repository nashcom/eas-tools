package main

import (
	"os"
	"testing"

	"github.com/nashcom/eas-tools/internal/certreload"
)

// No test writes to /run/secrets/tls of the machine: the default place of the certificate is a directory of the test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "easprobe-default-tls-")
	if err != nil {
		panic(err)
	}
	certreload.DefaultDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
