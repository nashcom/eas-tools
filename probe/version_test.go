package probe

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// push-release.sh and the release workflow read the version from this file with sed, and the tag of a release is "v" and the version.
func TestTheVersionIsThreeNumbers(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(Version) {
		t.Errorf("version %q: use X.Y.Z, the scripts and the tag depend on it", Version)
	}
	raw, err := os.ReadFile("profile.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\nconst Version = \""+Version+"\"\n") {
		t.Error("the line 'const Version = \"X.Y.Z\"' of profile.go is what push-release.sh and the release workflow read: keep that form")
	}
}

// All the programs of the toolbox have the same version. None of them has a version of its own: each takes probe.Version.
func TestEveryProgramReportsTheVersionOfTheToolbox(t *testing.T) {
	mains, err := filepath.Glob("../cmd/*/main.go")
	if err != nil || len(mains) < 5 {
		t.Fatalf("the programs of ../cmd were not found: %v %v", mains, err)
	}
	own := regexp.MustCompile(`(?m)^\s*(const|var)?\s*[vV]ersion\s*=\s*"`)
	for _, m := range mains {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, "probe.Version") {
			t.Errorf("%s does not use probe.Version", m)
		}
		if own.MatchString(src) {
			t.Errorf("%s has a version of its own: all the tools have the version of probe.Version", m)
		}
	}
}
