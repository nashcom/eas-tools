package exporter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/mailmonitor"
	"github.com/nashcom/eas-tools/probe"
)

// mailMonitorSettings turns the configuration into the settings of the mail monitor runner.
func mailMonitorSettings(c *Config) mailmonitor.Settings {
	s := mailmonitor.Settings{
		Targets:      map[string]mailmonitor.Target{},
		Fingerprints: map[string]string{},
		Poll:         time.Duration(c.MailMonitorPollSeconds) * time.Second,
		AuthBackoff:  c.AuthBackoff(),
	}
	for name, m := range c.MailMonitor {
		key, t := c.monitorAccount(name, m)
		s.Fingerprints[key] = fingerprint(c, t)
		s.Targets[name] = mailmonitor.Target{
			Name: name, To: m.To, Account: key,
			Interval: time.Duration(m.IntervalSeconds) * time.Second,
			Timeout:  time.Duration(m.TimeoutSeconds) * time.Second,
		}
	}
	return s
}

// RunMailMonitor runs the mail monitor until ctx ends. It is idle while no mail_monitor target is configured and
// follows a reload of the configuration.
func (s *Server) RunMailMonitor(ctx context.Context) { s.rm.Run(ctx) }

// openMailMonitor opens the ActiveSync session of an account for the mail monitor. The probe uses a device of its
// own: another one would share its sync keys with the probes of /probe, and the Inbox of its mail test would take the
// answers away.
func (s *Server) openMailMonitor(ctx context.Context, account string) (mailmonitor.Mailbox, error) {
	l := s.cur.Load()
	var t Target
	if name, own := strings.CutPrefix(account, "monitor:"); own {
		m, ok := l.cfg.MailMonitor[name]
		if !ok {
			return nil, fmt.Errorf("mail monitor %q is not configured any more", name)
		}
		t = m.MailMonitorAccount.target()
	} else {
		var ok bool
		if t, ok = l.cfg.Targets[account]; !ok {
			return nil, fmt.Errorf("account %q is not a target any more", account)
		}
	}
	password, err := t.password()
	if err != nil {
		return nil, err
	}
	server, err := probe.NormalizeServerURL(t.Server)
	if err != nil {
		return nil, err
	}
	insecure := l.cfg.Insecure
	if t.Insecure != nil {
		insecure = *t.Insecure
	}
	ca := l.cfg.CAFile
	if t.CAFile != "" {
		ca = t.CAFile
	}
	headers, _ := probe.ParseHeaders(t.headerLines())

	login := t.User
	if login == "" {
		login = t.Email
	}
	sess, err := probe.NewSession(probe.Config{
		ServerURL:   server,
		Email:       t.Email,
		Username:    login,
		Password:    password,
		DeviceID:    probe.DeriveDeviceID(strings.ToLower(login), server+"#mail-monitor"),
		Profile:     t.Profile,
		Provision:   t.Provision,
		Store:       s.store,
		Insecure:    insecure,
		CACertFile:  ca,
		StepTimeout: 30 * time.Second,
		Headers:     headers,
		Logger:      s.log,
	})
	if err != nil {
		return nil, err
	}
	box, err := mailmonitor.OpenSession(ctx, sess)
	if err != nil {
		sess.Close()
		return nil, err
	}
	return box, nil
}

// fingerprint is a hash of what an account is made of, so that a changed password or server is noticed on a reload.
func fingerprint(c *Config, t Target) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%#v|%v|%q", t, c.Insecure, c.CAFile)))
	return hex.EncodeToString(sum[:8])
}
