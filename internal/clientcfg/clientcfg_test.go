package clientcfg

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func newSet(t *testing.T, args ...string) (*flag.FlagSet, *Flags) {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := Define(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs, f
}

func resolve(t *testing.T, fs *flag.FlagSet, f *Flags, env []string, file string) (res struct{ Password string }, err error) {
	t.Helper()
	r, err := f.Resolve(fs, func() []string { return env }, func(string) ([]byte, error) { return []byte(file), nil })
	res.Password = r.Values["password"]
	return res, err
}

func TestAnAccountOfTheFileWithoutAPasswordUsesTheVariable(t *testing.T) {
	file := `{"accounts": [
		{"name": "lab", "server": "s.example.com", "user": "u"},
		{"name": "two", "server": "s.example.com", "user": "u", "password_file": "/run/secrets/pw"},
		{"name": "three", "server": "s.example.com", "user": "u", "password_env": "MY_PW"}]}`
	fs, f := newSet(t, "-config", "c.json")
	r, err := f.Resolve(fs, func() []string { return nil }, func(string) ([]byte, error) { return []byte(file), nil })
	if err != nil {
		t.Fatal(err)
	}
	accs, err := f.Accounts(r, strings.NewReader(""), false)
	if err != nil || len(accs) != 3 {
		t.Fatalf("%v %v", accs, err)
	}
	if accs[0].PasswordEnv != "EASCLIENT_PASSWORD" || accs[1].PasswordFile != "/run/secrets/pw" || accs[1].PasswordEnv != "" || accs[2].PasswordEnv != "MY_PW" {
		t.Errorf("%+v", accs)
	}

	fs, f = newSet(t, "-config", "c.json", "-password-env", "OTHER_PW")
	r, _ = f.Resolve(fs, func() []string { return nil }, func(string) ([]byte, error) { return []byte(file), nil })
	if accs, _ := f.Accounts(r, nil, false); accs[0].PasswordEnv != "OTHER_PW" || accs[2].PasswordEnv != "MY_PW" {
		t.Errorf("-password-env names the variable for the accounts that name none: %+v", accs)
	}
}

func TestTheSingleAccountOfTheCommandLine(t *testing.T) {
	fs, f := newSet(t, "-server", "s.example.com", "-email", "a@example.com", "-device-id", "abc", "-insecure")
	r, err := f.Resolve(fs, func() []string { return []string{"EASCLIENT_PASSWORD=pw"} }, nil)
	if err != nil {
		t.Fatal(err)
	}
	accs, err := f.Accounts(r, nil, false)
	if err != nil || len(accs) != 1 {
		t.Fatalf("%v %v", accs, err)
	}
	a := accs[0]
	if a.Name != "default" || a.Server != "s.example.com" || a.Password != "pw" || a.DeviceID != "ABC" || a.Insecure == nil || !*a.Insecure {
		t.Errorf("%+v", a)
	}
}

func TestAccountsThatAreWrong(t *testing.T) {
	// both the command line and the file
	fs, f := newSet(t, "-config", "c.json", "-server", "s.example.com", "-email", "a@example.com")
	r, _ := f.Resolve(fs, func() []string { return nil }, func(string) ([]byte, error) {
		return []byte(`{"accounts":[{"name":"lab","server":"s","user":"u"}]}`), nil
	})
	if _, err := f.Accounts(r, nil, false); err == nil || !strings.Contains(err.Error(), "use one of the two") {
		t.Errorf("both: %v", err)
	}
	// none
	fs, f = newSet(t)
	r, _ = f.Resolve(fs, func() []string { return nil }, nil)
	if _, err := f.Accounts(r, nil, false); err == nil || !strings.Contains(err.Error(), "no account") {
		t.Errorf("none: %v", err)
	}
	if accs, err := f.Accounts(r, nil, true); err != nil || len(accs) != 0 {
		t.Errorf("none, and the program can run without one: %v %v", accs, err)
	}
	// the password on stdin
	fs, f = newSet(t, "-server", "s.example.com", "-email", "a@example.com", "-password-stdin")
	r, _ = f.Resolve(fs, func() []string { return nil }, nil)
	if accs, err := f.Accounts(r, strings.NewReader("secret\r\nmore"), false); err != nil || accs[0].Password != "secret" {
		t.Errorf("stdin: %v %v", accs, err)
	}
	if _, err := f.Accounts(r, strings.NewReader(""), false); err == nil {
		t.Error("an empty stdin was taken for a password")
	}
}

func TestTheStateFileAndTheLogOfTheOptions(t *testing.T) {
	if _, f := newSet(t, "-no-state"); f.StateFilePath() != "" {
		t.Error("-no-state keeps no file")
	}
	if _, f := newSet(t, "-state-file", "/data/s.json"); f.StateFilePath() != "/data/s.json" {
		t.Error("-state-file")
	}
	if _, f := newSet(t); filepath.Base(f.StateFilePath()) != "client-state.json" {
		t.Errorf("the default: %s", f.StateFilePath())
	}

	fs, f := newSet(t, "-verbose")
	r, _ := f.Resolve(fs, func() []string { return nil }, nil)
	l, err := f.Logger(r, slog.LevelWarn, io.Discard, "t", "1")
	if err != nil || !l.Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("-verbose: %v", err)
	}
	fs, f = newSet(t)
	r, _ = f.Resolve(fs, func() []string { return nil }, nil)
	if l, _ := f.Logger(r, slog.LevelWarn, io.Discard, "t", "1"); l.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("the default level of a command is warn")
	}
	fs, f = newSet(t, "-log-format", "yaml")
	r, _ = f.Resolve(fs, func() []string { return nil }, nil)
	if _, err := f.Logger(r, slog.LevelInfo, io.Discard, "t", "1"); err == nil {
		t.Error("a log format that does not exist was accepted")
	}
}

func TestListsAndFirstLines(t *testing.T) {
	var l StringList
	for _, v := range []string{"a, b", "c", " ,d,, "} {
		if err := l.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if l.String() != "a,b,c,d" {
		t.Errorf("%q", l.String())
	}
	if s, err := FirstLine(strings.NewReader("token\nsecond")); err != nil || s != "token" {
		t.Errorf("%q %v", s, err)
	}
}

func TestTheSecretsComeFromTheEnvironmentOrTheFileOnly(t *testing.T) {
	fs, f := newSet(t)
	if r, _ := resolve(t, fs, f, []string{"EASCLIENT_PASSWORD=pw"}, ""); r.Password != "pw" {
		t.Errorf("from the environment: %q", r.Password)
	}
	for _, name := range []string{"password", "auth-token"} {
		if fs.Lookup(name) != nil {
			t.Errorf("-%s is an option: a secret on the command line is visible to every user", name)
		}
	}
}
