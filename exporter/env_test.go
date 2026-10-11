package exporter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvironmentAloneDefinesOneTarget(t *testing.T) {
	env := []string{
		"EASEXPORTER_LISTEN=:9200", "EASEXPORTER_STATE_FILE=/data/state.json", "EASEXPORTER_MIN_INTERVAL_SECONDS=120",
		"EASEXPORTER_SERVER=traveler.example.com", "EASEXPORTER_USER=probe@example.com",
		"EASEXPORTER_PASSWORD_FILE=/run/secrets/pw", "EASEXPORTER_MAIL=true", "EASEXPORTER_MAIL_WAIT_SECONDS=45",
		"EASEXPORTER_INSECURE=yes",
	}
	// no file at all: the config file may be missing when the environment has the target
	c, err := LoadConfigEnv(filepath.Join(t.TempDir(), "missing.json"), env)
	if err != nil {
		t.Fatal(err)
	}
	if ":9200" != c.Listen || "/data/state.json" != c.StateFile || 120 != c.MinIntervalSeconds {
		t.Errorf("global: %+v", c)
	}
	tg, ok := c.Targets["default"]
	if !ok || 1 != len(c.Targets) {
		t.Fatalf("targets: %v", c.Targets)
	}
	if "traveler.example.com" != tg.Server || "probe@example.com" != tg.User || "/run/secrets/pw" != tg.PasswordFile ||
		!tg.Mail || 45 != tg.MailWaitSeconds || !c.Insecure {
		t.Errorf("target: %+v", tg)
	}
	if 0 != len(c.Warnings) {
		t.Errorf("warnings: %v", c.Warnings)
	}
}

func TestEnvironmentWinsOverTheFileAndAddsTargets(t *testing.T) {
	file := writeConfig(t, `{
	  "listen": ":9116", "min_interval_seconds": 60,
	  "targets": [{"name": "traveler-lab","server": "file.example.com", "user": "a@example.com", "password_file": "/pw", "profile": "iphone"}, {"name": "other","server": "other.example.com", "user": "b@example.com", "password_file": "/pw2"}]}`)

	// the named target of the file is changed, the others stay
	c, err := LoadConfigEnv(file, []string{
		"EASEXPORTER_TARGET=traveler-lab", "EASEXPORTER_SERVER=env.example.com", "EASEXPORTER_LISTEN=:9300"})
	if err != nil {
		t.Fatal(err)
	}
	lab := c.Targets["traveler-lab"]
	if "env.example.com" != lab.Server || "a@example.com" != lab.User || "iphone" != lab.Profile || ":9300" != c.Listen {
		t.Errorf("changed target: %+v listen %s", lab, c.Listen)
	}
	if 2 != len(c.Targets) || "other.example.com" != c.Targets["other"].Server {
		t.Errorf("targets: %v", c.Targets)
	}

	// a new name adds a target
	c, err = LoadConfigEnv(file, []string{
		"EASEXPORTER_TARGET=extra", "EASEXPORTER_SERVER=extra.example.com", "EASEXPORTER_USER=c@example.com",
		"EASEXPORTER_PASSWORD_ENV=EXTRA_PW"})
	if err != nil {
		t.Fatal(err)
	}
	if 3 != len(c.Targets) || "extra.example.com" != c.Targets["extra"].Server {
		t.Errorf("targets: %v", c.Targets)
	}

	// without any target variable the file is used as it is
	c, err = LoadConfigEnv(file, []string{"EASEXPORTER_CONFIG=/ignored", "PATH=/bin"})
	if err != nil || 2 != len(c.Targets) || ":9116" != c.Listen || 0 != len(c.Warnings) {
		t.Errorf("file only: %v, %v", c, err)
	}
}

func TestEnvironmentErrorsAndWarnings(t *testing.T) {
	good := []string{"EASEXPORTER_SERVER=s.example.com", "EASEXPORTER_USER=u@example.com", "EASEXPORTER_PASSWORD_FILE=/pw"}
	missing := filepath.Join(t.TempDir(), "missing.json")

	for _, tc := range []struct{ name, extra, wants string }{
		{"bad number", "EASEXPORTER_MAX_CONCURRENT=lots", "EASEXPORTER_MAX_CONCURRENT"},
		{"bad bool", "EASEXPORTER_MAIL=perhaps", "EASEXPORTER_MAIL"},
		{"mail with an address in the login is fine", "EASEXPORTER_MAIL=1", ""},
	} {
		_, err := LoadConfigEnv(missing, append(append([]string(nil), good...), tc.extra))
		if "" == tc.wants {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.wants)
		}
	}

	// a typo is reported, a value is not
	c, err := LoadConfigEnv(missing, append(append([]string(nil), good...), "EASEXPORTER_SERVR=x", "EASEXPORTER_LISTNE=y"))
	if err != nil {
		t.Fatal(err)
	}
	if 2 != len(c.Warnings) || strings.Contains(strings.Join(c.Warnings, " "), "=x") {
		t.Errorf("warnings: %v", c.Warnings)
	}

	// no file and no target variable: still an error
	if _, err := LoadConfigEnv(missing, []string{"EASEXPORTER_LISTEN=:9100"}); err == nil {
		t.Error("a missing file without a target in the environment must fail")
	}
	// an empty variable counts as not set
	if _, err := LoadConfigEnv(missing, []string{"EASEXPORTER_SERVER="}); err == nil {
		t.Error("an empty variable must not define a target")
	}
}

// The example env file is a valid, complete setup: no warning about an unknown variable, and one target.
func TestExampleEnvFileLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "deploy", "exporter.example.env"))
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" && !strings.HasPrefix(line, "#") {
			env = append(env, line)
		}
	}
	c, err := LoadConfigEnv(filepath.Join(t.TempDir(), "missing.json"), env)
	if err != nil {
		t.Fatal(err)
	}
	if 0 != len(c.Warnings) {
		t.Errorf("warnings: %v", c.Warnings)
	}
	if 1 != len(c.Targets) {
		t.Fatalf("targets: %v", c.Targets)
	}
	tg, ok := c.Targets["traveler-lab"]
	if !ok || "traveler.example.com" != tg.Server || !tg.Mail || 60 != tg.MailWaitSeconds {
		t.Errorf("target: %+v", c.Targets)
	}
}

func TestPasswordInTheFileAndInTheEnvironment(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	base := []string{"EASEXPORTER_SERVER=s.example.com", "EASEXPORTER_USER=u@example.com"}

	// the password itself is a key of the target: password, EASEXPORTER_PASSWORD
	c, err := LoadConfigEnv(missing, append(append([]string(nil), base...), "EASEXPORTER_PASSWORD=secret"))
	if err != nil {
		t.Fatal(err)
	}
	tg := c.Targets["default"]
	if "secret" != tg.Password || 0 != len(c.Warnings) {
		t.Errorf("target %+v, warnings %v", tg, c.Warnings)
	}
	if p, err := tg.password(); err != nil || "secret" != p {
		t.Errorf("password() = %q, %v", p, err)
	}

	// in the JSON file
	file := writeConfig(t, `{"targets": [{"name": "a","server": "a.example.com", "user": "a@example.com", "password": "from-file"}]}`)
	c, err = LoadConfigEnv(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.Targets["a"].password(); err != nil || "from-file" != p {
		t.Errorf("password from the file: %q, %v", p, err)
	}

	// the variable wins over the file, as everywhere
	c, err = LoadConfigEnv(file, []string{"EASEXPORTER_TARGET=a", "EASEXPORTER_PASSWORD=from-env"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Targets["a"].password(); "from-env" != p {
		t.Errorf("env over file: %q", p)
	}

	// a password file wins over the password, and the password over the name of a variable
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("from-pw-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOME_PW", "from-named-variable")
	both := Target{PasswordFile: pw, Password: "plain", PasswordEnv: "SOME_PW"}
	if p, err := both.password(); err != nil || "from-pw-file" != p {
		t.Errorf("file first: %q, %v", p, err)
	}
	both.PasswordFile = ""
	if p, err := both.password(); err != nil || "plain" != p {
		t.Errorf("password second: %q, %v", p, err)
	}
	both.Password = ""
	if p, err := both.password(); err != nil || "from-named-variable" != p {
		t.Errorf("variable name last: %q, %v", p, err)
	}

	// no password at all is refused
	if _, err := LoadConfigEnv(missing, base); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("error %v", err)
	}

	// a config file with exactly one target: the variable belongs to it, no name needed
	one := writeConfig(t, `{"targets": [{"name": "a","server": "a.example.com", "user": "a@example.com"}]}`)
	c, err = LoadConfigEnv(one, []string{"EASEXPORTER_PASSWORD=secret"})
	if err != nil {
		t.Fatalf("one target and a password variable: %v", err)
	}
	if p, _ := c.Targets["a"].password(); "secret" != p || 1 != len(c.Targets) {
		t.Errorf("targets %v", c.Targets)
	}

	// with two targets the variable would be ambiguous: it makes a target "default", which has no server
	two := writeConfig(t, `{"targets": [{"name": "a","server": "a.example.com", "user": "a@example.com", "password": "pa"}, {"name": "b","server": "b.example.com", "user": "b@example.com"}]}`)
	if _, err := LoadConfigEnv(two, []string{"EASEXPORTER_PASSWORD=secret"}); err == nil {
		t.Error("without a target name and with two targets the password must not be given to one of them")
	}
	// a name makes it clear: target b gets the password, target a keeps its own
	c, err = LoadConfigEnv(two, []string{"EASEXPORTER_TARGET=b", "EASEXPORTER_PASSWORD=secret"})
	if err != nil {
		t.Fatalf("two targets and a name: %v", err)
	}
	if pa, _ := c.Targets["a"].password(); "pa" != pa {
		t.Errorf("target a: %q", pa)
	}
	if pb, _ := c.Targets["b"].password(); "secret" != pb {
		t.Errorf("target b: %q", pb)
	}
}
