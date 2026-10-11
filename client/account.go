package client

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Account is one mailbox the client can open. The keys are those of a target of the exporter, so the same account
// description works in both.
type Account struct {
	Name     string `json:"name"`
	Server   string `json:"server"` // host or URL
	Email    string `json:"email"`
	User     string `json:"user"` // login, default Email
	Profile  string `json:"profile"`
	DeviceID string `json:"device_id"`

	// The password: in the file ("password"), in a separate file (first line; a Docker secret), or in the environment
	// variable named by password_env. A file wins over the password, and the password over the variable.
	PasswordFile string `json:"password_file"`
	Password     string `json:"password"`
	PasswordEnv  string `json:"password_env"`

	Provision string `json:"provision"` // auto, always or never
	// TimeZone is the zone of this account, if it is not the one of the configuration or the machine (Europe/Berlin).
	TimeZone string `json:"time_zone"`

	Insecure *bool             `json:"insecure"`
	CAFile   string            `json:"ca_file"`
	Headers  map[string]string `json:"headers"`
}

var accountName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Validate checks the description of the account. It does not read the password.
func (a Account) Validate() error {
	if !accountName.MatchString(a.Name) {
		return fmt.Errorf("account name %q: use letters, digits, . _ - (1 to 64)", a.Name)
	}
	if strings.TrimSpace(a.Server) == "" {
		return errors.New("server is required (autodiscover is not supported by the client)")
	}
	if a.User == "" && a.Email == "" {
		return errors.New("user or email is required")
	}
	if a.PasswordFile == "" && a.Password == "" && a.PasswordEnv == "" {
		return errors.New("password, password_file or password_env is required")
	}
	return nil
}

func (a Account) login() string {
	if a.User != "" {
		return a.User
	}
	return a.Email
}

// password resolves the password when it is needed, so that rotating the file or the variable takes effect at the next
// connection.
func (a Account) password() (string, error) {
	switch {
	case a.PasswordFile != "":
		raw, err := os.ReadFile(a.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("password file: %w", err)
		}
		line, _, _ := strings.Cut(string(raw), "\n")
		line = strings.TrimRight(line, "\r")
		if line == "" {
			return "", errors.New("password file is empty")
		}
		return line, nil
	case a.Password != "":
		return a.Password, nil
	}
	p := os.Getenv(a.PasswordEnv)
	if p == "" {
		return "", fmt.Errorf("environment variable %s is empty", a.PasswordEnv)
	}
	return p, nil
}

func (a Account) headerLines() []string {
	var out []string
	for k, v := range a.Headers {
		out = append(out, k+": "+v)
	}
	return out
}
