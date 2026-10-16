package main

import (
	"os"
	"strings"
	"testing"
)

// The documentation is part of the program: a tool or an option that is not described is not finished. These tests read the pages and fail for
// every tool and every option that they do not mention.

func page(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEveryToolIsInTheDocumentationAndTheUsageText(t *testing.T) {
	mcpPage := page(t, "MCP.md")
	cs, _ := sessionWith(t, serverOptions{Send: true, Write: true})
	names := toolNames(t, cs)
	if len(names) < 20 {
		t.Fatalf("only %d tools: %v", len(names), names)
	}
	for _, name := range names {
		if !strings.Contains(mcpPage, "`"+name+"`") {
			t.Errorf("the tool %s is not in docs/MCP.md", name)
		}
		if !strings.Contains(usageText, name) {
			t.Errorf("the tool %s is not in the usage text of eas-mcp -h", name)
		}
	}
}

func TestEveryOptionIsInTheDocumentation(t *testing.T) {
	pages := page(t, "MCP.md") + page(t, "AUTH.md") + page(t, "CLIENT.md") + page(t, "TLS.md")
	for _, name := range allOptions() {
		if notSettings[name] {
			continue
		}
		if !strings.Contains(pages, "-"+name) {
			t.Errorf("the option -%s is not in docs/MCP.md, AUTH.md, CLIENT.md or TLS.md", name)
		}
	}
}

func TestTheSecretsAndTheSwitchesAreExplained(t *testing.T) {
	auth := page(t, "AUTH.md")
	for _, want := range []string{"EASCLIENT_AUTH_TOKEN", "EASCLIENT_PASSWORD", "oidc_users", "-send-domains", "-send", "-write", "RFC 9728", "401", "403"} {
		if !strings.Contains(auth+page(t, "MCP.md"), want) {
			t.Errorf("%q is not explained in docs/AUTH.md or docs/MCP.md", want)
		}
	}
}
