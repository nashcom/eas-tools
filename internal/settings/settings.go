// Package settings fills the options of a command from three places, in this order of precedence: the command line,
// then environment variables, then a JSON config file, then the default of the option itself. Every option has an
// environment variable (the prefix plus the upper-case name, "-" written as "_") and a key in the file (the name with
// "_" for "-"), so a program needs no code per option.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Options says what Resolve may do.
type Options struct {
	// Prefix of the environment variables, for example "EASLOAD_".
	Prefix string
	// ConfigFlag is the name of the option that holds the path of the config file ("" = no file); it is read from the
	// command line or from <Prefix>CONFIG, and it is not a setting itself.
	ConfigFlag string
	// Skip lists options that never come from the environment or the file, such as "version".
	Skip []string
	// Environ returns the environment as "NAME=value" lines; the default is os.Environ.
	Environ func() []string
	// ReadFile reads the config file; the default is os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// Extra lists keys that are not command line options but may be set in the file and as <Prefix><KEY> in the
	// environment, such as "password": it must never be given on a command line, where every user of the machine can
	// read it.
	Extra []string
	// Known lists more environment variable names that belong to the program and are not options, so that they are
	// not reported as unknown (for example the variable that holds a password).
	Known []string
}

// Result reports what Resolve did.
type Result struct {
	// Warnings are environment variables with the prefix that match no option, which is most likely a typo.
	Warnings []string
	// Values holds the value of each Extra key that was set, from the environment or else from the file.
	Values map[string]string
	// Sources tells for each option that did not come from the command line where its value came from: "env" or "file".
	Sources map[string]string
}

// EnvName returns the name of the environment variable of an option.
func EnvName(prefix, option string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(option, "-", "_"))
}

// Resolve completes the options of fs after fs.Parse: every option that was not given on the command line takes the
// value of its environment variable, or else of its key in the config file. The values go through flag.Set, so they
// are checked like command-line values, and the options count as set afterwards (flag.Visit sees them).
func Resolve(fs *flag.FlagSet, o Options) (Result, error) {
	res := Result{Sources: map[string]string{}, Values: map[string]string{}}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	skip := map[string]bool{}
	for _, s := range o.Skip {
		skip[s] = true
	}
	if o.ConfigFlag != "" {
		skip[o.ConfigFlag] = true
	}

	env := map[string]string{}
	for _, line := range o.Environ() {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}

	setOnCommandLine := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setOnCommandLine[f.Name] = true })

	// the config file: the path comes from the command line or the environment
	var file map[string]any
	if o.ConfigFlag != "" {
		path := ""
		if f := fs.Lookup(o.ConfigFlag); f != nil {
			path = f.Value.String()
		}
		if path == "" {
			path = env[EnvName(o.Prefix, o.ConfigFlag)]
		}
		if path != "" {
			raw, err := o.ReadFile(path)
			if err != nil {
				return res, fmt.Errorf("config file: %w", err)
			}
			if file, err = parseFile(raw, fs, skip, o.Extra); err != nil {
				return res, fmt.Errorf("config file %s: %w", path, err)
			}
		}
	}

	var firstErr error
	fs.VisitAll(func(f *flag.Flag) {
		if firstErr != nil || skip[f.Name] || setOnCommandLine[f.Name] {
			return
		}
		name := EnvName(o.Prefix, f.Name)
		if v, ok := env[name]; ok && v != "" {
			if err := fs.Set(f.Name, v); err != nil {
				firstErr = fmt.Errorf("%s=%q: %w", name, v, err)
				return
			}
			res.Sources[f.Name] = "env"
			return
		}
		if v, ok := file[strings.ReplaceAll(f.Name, "-", "_")]; ok {
			values, err := fileValues(v)
			if err != nil {
				firstErr = fmt.Errorf("config file key %q: %w", strings.ReplaceAll(f.Name, "-", "_"), err)
				return
			}
			for _, s := range values {
				if err := fs.Set(f.Name, s); err != nil {
					firstErr = fmt.Errorf("config file key %q: %w", strings.ReplaceAll(f.Name, "-", "_"), err)
					return
				}
			}
			res.Sources[f.Name] = "file"
		}
	})
	if firstErr != nil {
		return res, firstErr
	}

	// the extra keys: the environment wins over the file
	for _, key := range o.Extra {
		if v := env[EnvName(o.Prefix, key)]; v != "" {
			res.Values[key] = v
			res.Sources[key] = "env"
			continue
		}
		if raw, ok := file[key]; ok {
			values, err := fileValues(raw)
			if err != nil || len(values) != 1 {
				return res, fmt.Errorf("config file key %q: must be one string or number", key)
			}
			if values[0] != "" {
				res.Values[key] = values[0]
				res.Sources[key] = "file"
			}
		}
	}

	// variables of the program that are not options
	known := map[string]bool{}
	for _, k := range o.Known {
		known[k] = true
	}
	fs.VisitAll(func(f *flag.Flag) { known[EnvName(o.Prefix, f.Name)] = true })
	for _, k := range o.Extra {
		known[EnvName(o.Prefix, k)] = true
	}
	for k := range env {
		if strings.HasPrefix(k, o.Prefix) && !known[k] {
			res.Warnings = append(res.Warnings, k+" is set, but there is no such option")
		}
	}
	sort.Strings(res.Warnings)
	return res, nil
}

// parseFile reads the config file and checks that every key is an option.
func parseFile(raw []byte, fs *flag.FlagSet, skip map[string]bool, extra []string) (map[string]any, error) {
	extraKey := map[string]bool{}
	for _, k := range extra {
		extraKey[k] = true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	var bad []string
	for k := range m {
		name := strings.ReplaceAll(k, "_", "-")
		if extraKey[k] {
			continue
		}
		if skip[name] || fs.Lookup(name) == nil {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("unknown or not allowed keys: %s", strings.Join(bad, ", "))
	}
	return m, nil
}

// fileValues turns a JSON value into the strings to give to flag.Set: one for a scalar, one per element for an array
// (for options that can be repeated).
func fileValues(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case json.Number:
		return []string{x.String()}, nil
	case bool:
		return []string{fmt.Sprint(x)}, nil
	case []any:
		var out []string
		for _, e := range x {
			s, err := fileValues(e)
			if err != nil || len(s) != 1 {
				return nil, errors.New("an array may only hold strings, numbers and booleans")
			}
			out = append(out, s[0])
		}
		return out, nil
	case nil:
		return nil, errors.New("null is not a value")
	}
	return nil, fmt.Errorf("unsupported value %v", v)
}
