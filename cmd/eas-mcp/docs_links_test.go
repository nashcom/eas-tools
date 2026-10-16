package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// anchor is the id that GitHub makes of a heading: lower case, only letters, digits, spaces and hyphens, the spaces as hyphens.
func anchor(heading string) string {
	h := strings.ToLower(strings.TrimSpace(heading))
	h = regexp.MustCompile(`[^a-z0-9 -]`).ReplaceAllString(h, "")
	return strings.ReplaceAll(h, " ", "-")
}

func anchorsOf(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fenced := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "#") {
			out[anchor(strings.TrimLeft(line, "# "))] = true
		}
	}
	return out
}

// Every link from a page to another page, and to a heading of a page, leads somewhere.
func TestEveryLinkBetweenThePagesLeadsSomewhere(t *testing.T) {
	files, _ := filepath.Glob("../../docs/*.md")
	files = append(files, "../../README.md", "../../CHANGES.md")
	link := regexp.MustCompile(`\]\(([^)\s#]*)(#[^)\s]*)?\)`)
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fenced := false
		for n, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "```") {
				fenced = !fenced
			}
			if fenced {
				continue
			}
			for _, m := range link.FindAllStringSubmatch(line, -1) {
				target, frag := m[1], strings.TrimPrefix(m[2], "#")
				if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path := file
				if target != "" {
					path = filepath.Join(filepath.Dir(file), target)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Errorf("%s:%d links to %q, which is not there", filepath.Base(file), n+1, target)
					continue
				}
				if !exactCase(path) {
					t.Errorf("%s:%d links to %q, which is there only if the case of the name does not matter (Windows, macOS): on Linux, in CI and in the image it is not found", filepath.Base(file), n+1, target)
				}
				checked++
				if frag != "" && !info.IsDir() && strings.HasSuffix(path, ".md") && !anchorsOf(t, path)[frag] {
					t.Errorf("%s:%d links to %s#%s, and that page has no such heading", filepath.Base(file), n+1, filepath.Base(path), frag)
				}
			}
		}
	}
	if checked < 50 {
		t.Errorf("only %d links were checked: the test does not see the pages", checked)
	}
}

// Every event of the audit log that the code writes is described in docs/LOGGING.md.
func TestEveryAuditEventIsDescribed(t *testing.T) {
	logging := page(t, "LOGGING.md")
	files, _ := filepath.Glob("*.go")
	event := regexp.MustCompile(`slog\.Level(?:Info|Warn),\s*"(mcp\.[a-z.]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, m := range event.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) < 5 {
		t.Fatalf("only %d audit events found in the code: %v", len(seen), seen)
	}
	for name := range seen {
		if !strings.Contains(logging, "`"+name+"`") {
			t.Errorf("the audit event %s is not in docs/LOGGING.md", name)
		}
	}
	for _, want := range []string{"-audit-log", "SIGHUP", "-log-format", "log_level", "OpenTelemetry"} {
		if !strings.Contains(logging, want) {
			t.Errorf("docs/LOGGING.md does not mention %s", want)
		}
	}
}

// exactCase says whether every part of the path has exactly the case that its folder has. Windows and macOS do not care, Linux (CI, the image) does.
func exactCase(path string) bool {
	cur := ""
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part == "" || part == "." || part == ".." {
			cur = filepath.Join(cur, part)
			continue
		}
		dir := cur
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		found := false
		for _, e := range entries {
			if e.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		cur = filepath.Join(cur, part)
	}
	return true
}

func TestTheCaseOfAPathIsChecked(t *testing.T) {
	if !exactCase("../../docs/MCP.md") || !exactCase("../../README.md") {
		t.Error("a path with the right case was refused")
	}
	if exactCase("../../docs/mcp.md") || exactCase("../../DOCS/MCP.md") {
		t.Error("a path with the wrong case was accepted: on Linux it is not there")
	}
}
