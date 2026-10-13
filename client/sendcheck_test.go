package client

import (
	"context"
	"errors"
	"testing"
)

func TestTheDomainsThatMailMayGoTo(t *testing.T) {
	allowed := []string{"example.com", "*.partner.org"}
	for domain, want := range map[string]bool{
		"example.com":        true,
		"EXAMPLE.com":        false, // callers hand over the lower case domain of addressDomain
		"mail.example.com":   false, // example.com is that domain alone
		"partner.org":        false, // *.partner.org is the domains below it
		"mail.partner.org":   true,
		"a.b.partner.org":    true,
		"notpartner.org":     false,
		"evil.org":           false,
		"example.com.evil.x": false,
	} {
		if got := domainAllowed(domain, allowed); got != want {
			t.Errorf("%s: %v, want %v", domain, got, want)
		}
	}
}

func TestTheDomainOfAnAddress(t *testing.T) {
	for addr, want := range map[string]string{"a@Example.com": "example.com", "Name <a@x.org>": "x.org", " a@x.org ": "x.org"} {
		if got, err := addressDomain(addr); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", addr, got, err, want)
		}
	}
	for _, bad := range []string{"", "nobody", "a@", "@x.org", "a@@x.org, b@y.org"} {
		if _, err := addressDomain(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestSendingIsRefusedForADomainThatIsNotOnTheList(t *testing.T) {
	m := &Manager{opts: Options{AllowSend: true, SendDomains: []string{"example.com"}}}
	ctx := context.Background()
	for name, r := range map[string]SendRequest{
		"to":      {To: []string{"a@evil.org"}, Subject: "s"},
		"cc":      {To: []string{"a@example.com"}, Cc: []string{"b@evil.org"}, Subject: "s"},
		"garbage": {To: []string{"nobody"}, Subject: "s"},
	} {
		_, err := m.Send(ctx, "", r)
		var e *Error
		if !errors.As(err, &e) || (e.Code != "recipient_not_allowed" && e.Code != "invalid_recipient") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := m.Forward(ctx, "", "inbox", "1:1", ForwardRequest{To: []string{"a@evil.org"}}); err == nil {
		t.Error("forward to another domain was not refused")
	} else if e := AsError(err); e.Code != "recipient_not_allowed" {
		t.Errorf("forward: %v", err)
	}
	// without a list nothing is restricted: the check passes and the mail then fails for the account that is not there
	free := &Manager{opts: Options{AllowSend: true}}
	if err := free.checkRecipients([]string{"a@anywhere.org"}); err != nil {
		t.Errorf("without a list: %v", err)
	}
}
