package client

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
)

// The list of domains that mail may be sent to (Options.SendDomains). It is a guard for what the program sends for someone else: a person
// at the command line, a program that uses the REST API, or an AI that has been given the mailbox. An entry is a domain (example.com), which
// is that domain alone, or *.example.com, which is every domain below it. Without a list there is no restriction.

// checkRecipients refuses a mail with a recipient in a domain that is not on the list. Every address of every list is checked, the mail
// goes to none of them when one is refused.
func (m *Manager) checkRecipients(lists ...[]string) error {
	if len(m.opts.SendDomains) == 0 {
		return nil
	}
	for _, list := range lists {
		for _, addr := range list {
			domain, err := addressDomain(addr)
			if err != nil {
				return newError(http.StatusBadRequest, "invalid_recipient", fmt.Sprintf("%q is not an e-mail address", addr))
			}
			if !domainAllowed(domain, m.opts.SendDomains) {
				return newError(http.StatusForbidden, "recipient_not_allowed", fmt.Sprintf("mail to %s is not allowed: sending is limited to these domains: %s",
					domain, strings.Join(m.opts.SendDomains, ", ")))
			}
		}
	}
	return nil
}

// checkReplyRecipients is the check for a reply, whose recipients the server decides: the sender of the mail, and with all the others of the
// mail. The mail is read for that. The address of the account itself is not a recipient of a reply.
func (m *Manager) checkReplyRecipients(ctx context.Context, account, folder, id string, all bool) error {
	if len(m.opts.SendDomains) == 0 {
		return nil
	}
	got, err := m.Get(ctx, account, folder, id)
	if err != nil {
		return err
	}
	e, err := m.resolve(account)
	if err != nil {
		return err
	}
	own := strings.ToLower(e.acct.Email)
	var to []string
	for _, field := range []string{got.Message.From} {
		to = append(to, splitAddresses(field)...)
	}
	if all {
		to = append(to, splitAddresses(got.Message.To)...)
		to = append(to, splitAddresses(got.Message.Cc)...)
	}
	var check []string
	for _, a := range to {
		if p, err := mail.ParseAddress(a); err == nil && strings.EqualFold(p.Address, own) {
			continue
		}
		check = append(check, a)
	}
	return m.checkRecipients(check)
}

// splitAddresses splits the address list of a header, "A <a@x>, b@y", into its addresses. A list that cannot be read is one address that
// the check then refuses.
func splitAddresses(list string) []string {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil
	}
	parsed, err := mail.ParseAddressList(list)
	if err != nil {
		return []string{list}
	}
	out := make([]string, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, p.Address)
	}
	return out
}

// addressDomain is the domain of an address, "a@x.org" and "A <a@x.org>" alike, in lower case.
func addressDomain(addr string) (string, error) {
	p, err := mail.ParseAddress(strings.TrimSpace(addr))
	if err != nil {
		return "", err
	}
	at := strings.LastIndex(p.Address, "@")
	if at < 1 || at == len(p.Address)-1 {
		return "", fmt.Errorf("no domain in %q", addr)
	}
	return strings.ToLower(p.Address[at+1:]), nil
}

// domainAllowed says whether the domain is on the list: the domain itself, or a domain below *.name.
func domainAllowed(domain string, allowed []string) bool {
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(domain, a[1:]) {
				return true
			}
		case domain == a:
			return true
		}
	}
	return false
}
