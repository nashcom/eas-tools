package settings

import (
	"flag"
	"strings"
	"testing"
	"time"
)

// list is an option that can be repeated, like -header.
type list []string

func (l *list) String() string     { return strings.Join(*l, "|") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

type fixture struct {
	fs       *flag.FlagSet
	server   *string
	devices  *int
	duration *time.Duration
	insecure *bool
	pwFile   *string
	config   *string
	headers  list
}

func newFixture(args ...string) (*fixture, error) {
	f := &fixture{fs: flag.NewFlagSet("t", flag.ContinueOnError)}
	f.server = f.fs.String("server", "default.example.com", "")
	f.devices = f.fs.Int("devices", 1, "")
	f.duration = f.fs.Duration("duration", 0, "")
	f.insecure = f.fs.Bool("insecure", false, "")
	f.pwFile = f.fs.String("password-file", "", "")
	f.config = f.fs.String("config", "", "")
	f.fs.Var(&f.headers, "header", "")
	f.fs.Bool("version", false, "")
	return f, f.fs.Parse(args)
}

func run(t *testing.T, f *fixture, env []string, files map[string]string) (Result, error) {
	t.Helper()
	return Resolve(f.fs, Options{
		Prefix: "EASLOAD_", ConfigFlag: "config", Skip: []string{"version"},
		Environ:  func() []string { return env },
		ReadFile: func(p string) ([]byte, error) { return []byte(files[p]), nil },
		Known:    []string{"EASLOAD_PASSWORD"},
	})
}

func TestPrecedenceCommandLineEnvFileDefault(t *testing.T) {
	file := `{"server": "file.example.com", "devices": 5, "duration": "10m", "insecure": true, "password_file": "/run/secrets/pw"}`
	files := map[string]string{"/etc/load.json": file}

	// nothing but the file
	f, _ := newFixture("-config", "/etc/load.json")
	if _, err := run(t, f, nil, files); err != nil {
		t.Fatal(err)
	}
	if "file.example.com" != *f.server || 5 != *f.devices || 10*time.Minute != *f.duration || !*f.insecure || "/run/secrets/pw" != *f.pwFile {
		t.Errorf("file values: %s %d %v %v %s", *f.server, *f.devices, *f.duration, *f.insecure, *f.pwFile)
	}

	// the environment beats the file
	f, _ = newFixture("-config", "/etc/load.json")
	if _, err := run(t, f, []string{"EASLOAD_SERVER=env.example.com", "EASLOAD_DEVICES=7"}, files); err != nil {
		t.Fatal(err)
	}
	if "env.example.com" != *f.server || 7 != *f.devices || 10*time.Minute != *f.duration {
		t.Errorf("env over file: %s %d %v", *f.server, *f.devices, *f.duration)
	}

	// the command line beats both
	f, _ = newFixture("-config", "/etc/load.json", "-server", "cli.example.com")
	if _, err := run(t, f, []string{"EASLOAD_SERVER=env.example.com"}, files); err != nil {
		t.Fatal(err)
	}
	if "cli.example.com" != *f.server {
		t.Errorf("command line over env: %s", *f.server)
	}

	// no source: the default stays
	f, _ = newFixture()
	if _, err := run(t, f, nil, nil); err != nil {
		t.Fatal(err)
	}
	if "default.example.com" != *f.server || 1 != *f.devices {
		t.Errorf("defaults: %s %d", *f.server, *f.devices)
	}
}

func TestConfigPathFromEnvironmentAndSources(t *testing.T) {
	f, _ := newFixture()
	res, err := run(t, f, []string{"EASLOAD_CONFIG=/x.json", "EASLOAD_DEVICES=3"}, map[string]string{"/x.json": `{"server": "x.example.com"}`})
	if err != nil {
		t.Fatal(err)
	}
	if "x.example.com" != *f.server || 3 != *f.devices {
		t.Errorf("%s %d", *f.server, *f.devices)
	}
	if "file" != res.Sources["server"] || "env" != res.Sources["devices"] {
		t.Errorf("sources %v", res.Sources)
	}
}

func TestSettingsCountAsSetAndRepeatedOptionsAreNotDuplicated(t *testing.T) {
	f, _ := newFixture("-header", "A: 1")
	if _, err := run(t, f, nil, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if 1 != len(f.headers) {
		t.Errorf("a header given on the command line must not be touched: %v", f.headers)
	}

	f, _ = newFixture("-config", "/h.json")
	if _, err := run(t, f, []string{"EASLOAD_DEVICES=2"}, map[string]string{"/h.json": `{"header": ["X-One: 1", "X-Two: 2"]}`}); err != nil {
		t.Fatal(err)
	}
	if 2 != len(f.headers) || "X-One: 1" != f.headers[0] || "X-Two: 2" != f.headers[1] {
		t.Errorf("headers from the file: %v", f.headers)
	}
	set := map[string]bool{}
	f.fs.Visit(func(x *flag.Flag) { set[x.Name] = true })
	if !set["header"] || !set["devices"] || !set["config"] {
		t.Errorf("options from the file and the environment must count as set: %v", set)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		env   []string
		file  string
		wants string
	}{
		{"unknown key", []string{"-config", "/c.json"}, nil, `{"serverr": "x"}`, "serverr"},
		{"setting the config path in the file", []string{"-config", "/c.json"}, nil, `{"config": "/other.json"}`, "config"},
		{"version in the file", []string{"-config", "/c.json"}, nil, `{"version": true}`, "version"},
		{"bad number in the environment", nil, []string{"EASLOAD_DEVICES=many"}, "", "EASLOAD_DEVICES"},
		{"bad duration in the file", []string{"-config", "/c.json"}, nil, `{"duration": "soon"}`, "duration"},
		{"nested value", []string{"-config", "/c.json"}, nil, `{"server": {"a": 1}}`, "server"},
		{"not JSON", []string{"-config", "/c.json"}, nil, `server=x`, "config file"},
	}
	for _, tc := range cases {
		f, _ := newFixture(tc.args...)
		_, err := run(t, f, tc.env, map[string]string{"/c.json": tc.file})
		if err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("%s: error %v, want it to mention %q", tc.name, err, tc.wants)
		}
	}
}

func TestMissingConfigFileIsAnError(t *testing.T) {
	f, _ := newFixture("-config", "/missing.json")
	_, err := Resolve(f.fs, Options{Prefix: "EASLOAD_", ConfigFlag: "config",
		Environ:  func() []string { return nil },
		ReadFile: func(string) ([]byte, error) { return nil, errNotFound }})
	if err == nil || !strings.Contains(err.Error(), "config file") {
		t.Errorf("error %v", err)
	}
}

var errNotFound = &notFound{}

type notFound struct{}

func (*notFound) Error() string { return "no such file" }

func TestUnknownVariablesAreReported(t *testing.T) {
	f, _ := newFixture()
	res, err := run(t, f, []string{"EASLOAD_SERVR=x", "EASLOAD_PASSWORD=secret", "EASLOAD_SERVER=y", "OTHER=1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if 1 != len(res.Warnings) || !strings.HasPrefix(res.Warnings[0], "EASLOAD_SERVR ") {
		t.Errorf("warnings %v", res.Warnings)
	}
	if strings.Contains(strings.Join(res.Warnings, " "), "secret") {
		t.Error("a warning must never contain a value")
	}
}

func TestEmptyVariableDoesNotOverride(t *testing.T) {
	f, _ := newFixture("-config", "/c.json")
	if _, err := run(t, f, []string{"EASLOAD_SERVER="}, map[string]string{"/c.json": `{"server": "file.example.com"}`}); err != nil {
		t.Fatal(err)
	}
	if "file.example.com" != *f.server {
		t.Errorf("an empty variable counts as not set: %s", *f.server)
	}
}

func TestEnvName(t *testing.T) {
	if "EASLOAD_PASSWORD_FILE" != EnvName("EASLOAD_", "password-file") || "EAS_X" != EnvName("EAS_", "x") {
		t.Error("EnvName")
	}
}

// "password" is no command line option, but a key of the file and a variable like the others.
func TestExtraKeysComeFromTheFileAndTheEnvironmentOnly(t *testing.T) {
	opts := func(env []string, files map[string]string) Options {
		return Options{Prefix: "EASLOAD_", ConfigFlag: "config", Skip: []string{"version"}, Extra: []string{"password"},
			Environ:  func() []string { return env },
			ReadFile: func(p string) ([]byte, error) { return []byte(files[p]), nil }}
	}
	files := map[string]string{"/c.json": `{"server": "s.example.com", "password": "from-file"}`}

	f, _ := newFixture("-config", "/c.json")
	res, err := Resolve(f.fs, opts(nil, files))
	if err != nil || "from-file" != res.Values["password"] || "file" != res.Sources["password"] {
		t.Errorf("file: %v, %v, %v", res.Values, res.Sources, err)
	}

	f, _ = newFixture("-config", "/c.json")
	res, err = Resolve(f.fs, opts([]string{"EASLOAD_PASSWORD=from-env"}, files))
	if err != nil || "from-env" != res.Values["password"] || 0 != len(res.Warnings) {
		t.Errorf("env: %v, %v, warnings %v", res.Values, err, res.Warnings)
	}

	// not given at all: no value, no error
	f, _ = newFixture()
	res, err = Resolve(f.fs, opts(nil, nil))
	if err != nil || "" != res.Values["password"] {
		t.Errorf("none: %v, %v", res.Values, err)
	}

	// it is not a command line option
	f, _ = newFixture()
	if f.fs.Lookup("password") != nil {
		t.Fatal("the fixture must not have a password option")
	}

	// other unknown keys are still an error, and so is a nested value
	f, _ = newFixture("-config", "/c.json")
	if _, err := Resolve(f.fs, opts(nil, map[string]string{"/c.json": `{"passwort": "x"}`})); err == nil {
		t.Error("a misspelled key must be an error")
	}
	f, _ = newFixture("-config", "/c.json")
	if _, err := Resolve(f.fs, opts(nil, map[string]string{"/c.json": `{"password": {"a": 1}}`})); err == nil {
		t.Error("a nested value must be an error")
	}
}
