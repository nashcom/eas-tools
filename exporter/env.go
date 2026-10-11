package exporter

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Environment variables of the exporter. They are an alternative to the config file, for a container with one target,
// and they win over the file. The name of a variable is the key of the config file in upper case with the prefix:
//
//	EASEXPORTER_LISTEN, EASEXPORTER_STATE_FILE, EASEXPORTER_MIN_INTERVAL_SECONDS, ...   the global settings
//	EASEXPORTER_SERVER, EASEXPORTER_USER, EASEXPORTER_PASSWORD_FILE, EASEXPORTER_MAIL, ...   the fields of the one target
//	EASEXPORTER_TARGET   the name of that target in /probe?target=NAME; a target of the config file with the same name is
//	                   changed, otherwise a new target is added. Without it the only target of the config file is changed,
//	                   and when there is not exactly one, the target is called "default"
//
// "insecure" and "ca_file" are global settings that every target inherits, so there is one variable for each. The
// password of the target is EASEXPORTER_PASSWORD_FILE (recommended), EASEXPORTER_PASSWORD (the password itself) or
// EASEXPORTER_PASSWORD_ENV (the name of another variable that holds it).
const (
	envPrefix     = "EASEXPORTER_"
	envTargetName = "EASEXPORTER_TARGET"
	envConfig     = "EASEXPORTER_CONFIG" // the path of the config file, read by the command
	defaultTarget = "default"
)

// sharedKeys are keys of Target that are also keys of Config: the variable sets the global one, which targets inherit.
var sharedKeys = map[string]bool{"insecure": true, "ca_file": true}

// applyEnv sets the global settings and the one target of the environment. It returns the variables with the exporter
// prefix that it did not use, which is most likely a typo.
func applyEnv(c *Config, environ []string) (warnings []string, err error) {
	env := map[string]string{}
	for _, line := range environ {
		if k, v, ok := strings.Cut(line, "="); ok && v != "" {
			env[k] = v
		}
	}

	used := map[string]bool{envConfig: true, envTargetName: true}
	for _, n := range remoteEnvNames {
		used[n] = true
	}

	if err := setFields(reflect.ValueOf(c).Elem(), env, used, nil, "targets", "mail_monitor"); err != nil {
		return nil, err
	}

	if hasTargetVariable(env) {
		name := env[envTargetName]
		if name == "" {
			// without a name the variables belong to the only target of the file, if there is exactly one
			name = defaultTarget
			if len(c.Targets) == 1 {
				for only := range c.Targets {
					name = only
				}
			}
		}
		if c.Targets == nil {
			c.Targets = map[string]Target{}
		}
		t := c.Targets[name]
		if err := setFields(reflect.ValueOf(&t).Elem(), env, used, sharedKeys, "headers"); err != nil {
			return nil, err
		}
		c.Targets[name] = t
	}

	for k := range env {
		if strings.HasPrefix(k, envPrefix) && !used[k] {
			warnings = append(warnings, k+" is set, but there is no such setting")
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

// hasTargetVariable reports whether the environment sets a field of the one target.
func hasTargetVariable(env map[string]string) bool {
	t := reflect.TypeOf(Target{})
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if key == "" || key == "-" || key == "headers" || sharedKeys[key] {
			continue
		}
		if _, ok := env[envPrefix+strings.ToUpper(key)]; ok {
			return true
		}
	}
	return false
}

// hasEnvTarget reports whether the environment defines a target, so that a config file is not needed.
func hasEnvTarget(environ []string) bool {
	env := map[string]string{}
	for _, line := range environ {
		if k, v, ok := strings.Cut(line, "="); ok && v != "" {
			env[k] = v
		}
	}
	return hasTargetVariable(env)
}

// setFields sets the plain fields of a struct (string, int, bool and *bool) from the variables EASEXPORTER_<JSONKEY>.
// It leaves out the keys in leaveOut (they belong to the other struct) and the ones in skip, which hold maps and need
// the config file.
func setFields(v reflect.Value, env map[string]string, used map[string]bool, leaveOut map[string]bool, skip ...string) error {
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if key == "" || key == "-" || skipped[key] || leaveOut[key] {
			continue
		}
		name := envPrefix + strings.ToUpper(key)
		val, ok := env[name]
		if !ok {
			continue
		}
		used[name] = true

		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(val)
		case reflect.Int:
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("%s=%q: not a number", name, val)
			}
			f.SetInt(int64(n))
		case reflect.Bool:
			b, err := parseBool(val)
			if err != nil {
				return fmt.Errorf("%s=%q: %v", name, val, err)
			}
			f.SetBool(b)
		case reflect.Slice:
			if f.Type().Elem().Kind() != reflect.String {
				continue
			}
			// a list of texts: the variable has them separated by commas
			var list []string
			for _, part := range strings.Split(val, ",") {
				if part = strings.TrimSpace(part); part != "" {
					list = append(list, part)
				}
			}
			f.Set(reflect.ValueOf(list))
		case reflect.Ptr:
			if f.Type().Elem().Kind() != reflect.Bool {
				continue
			}
			b, err := parseBool(val)
			if err != nil {
				return fmt.Errorf("%s=%q: %v", name, val, err)
			}
			f.Set(reflect.ValueOf(&b))
		}
	}
	return nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("must be true or false")
}
