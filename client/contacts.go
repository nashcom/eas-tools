package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nashcom/eas-tools/probe"
)

// Contact is an entry of the address book. The photo is not part of the JSON: it is in the vCard.
type Contact struct {
	ID          string           `json:"id"`
	Folder      string           `json:"folder"`
	DisplayName string           `json:"display_name"`
	Name        *ContactName     `json:"name,omitempty"`
	Company     string           `json:"company,omitempty"`
	Department  string           `json:"department,omitempty"`
	JobTitle    string           `json:"job_title,omitempty"`
	Office      string           `json:"office,omitempty"`
	Manager     string           `json:"manager,omitempty"`
	Emails      []string         `json:"emails,omitempty"`
	Phones      []Phone          `json:"phones,omitempty"`
	Addresses   []ContactAddress `json:"addresses,omitempty"`
	IMAddresses []string         `json:"im_addresses,omitempty"`
	WebPage     string           `json:"web_page,omitempty"`
	Birthday    *Time            `json:"birthday,omitempty"`
	Anniversary *Time            `json:"anniversary,omitempty"`
	Categories  []string         `json:"categories,omitempty"`
	HasPhoto    bool             `json:"has_photo"`
	PhotoBytes  int              `json:"photo_bytes,omitempty"`
}

type ContactName struct {
	Title    string `json:"title,omitempty"` // a form of address such as Dr.
	First    string `json:"first,omitempty"`
	Middle   string `json:"middle,omitempty"`
	Last     string `json:"last,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	FileAs   string `json:"file_as,omitempty"`
}

type Phone struct {
	Type   string `json:"type"` // home, work, mobile
	Number string `json:"number"`
}

type ContactAddress struct {
	Type       string `json:"type"` // home, work
	Street     string `json:"street,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// ContactsResult is the answer of "contacts list".
type ContactsResult struct {
	Account string `json:"account"`
	Folder  Folder `json:"folder"`
	// Total is the number of contacts that match the search, Count the number in the answer (Limit cuts it).
	Total    int       `json:"total"`
	Count    int       `json:"count"`
	Contacts []Contact `json:"contacts"`
}

// ContactResult is the answer of "contacts get".
type ContactResult struct {
	Account string  `json:"account"`
	Contact Contact `json:"contact"`
}

// ContactParams shape a contacts list.
type ContactParams struct {
	// Search keeps the contacts with this text in the name, an e-mail address, the company or a phone number (no case).
	Search string `json:"search"`
	// Limit is the number of contacts (default 50; -1 all).
	Limit int `json:"limit"`
}

func contactJSON(c probe.Contact) Contact {
	out := Contact{
		ID: c.ID, Folder: c.Folder, DisplayName: c.DisplayName(), Company: c.Company, Department: c.Department, JobTitle: c.JobTitle,
		Office: c.Office, Manager: c.Manager, Emails: c.Emails, IMAddresses: c.IMAddresses, WebPage: c.WebPage,
		Birthday: stamp(c.Birthday), Anniversary: stamp(c.Anniversary), Categories: c.Categories, HasPhoto: len(c.Picture) > 0, PhotoBytes: len(c.Picture),
	}
	n := ContactName{Title: c.Title, First: c.FirstName, Middle: c.MiddleName, Last: c.LastName, Suffix: c.Suffix, Nickname: c.NickName, FileAs: c.FileAs}
	if n != (ContactName{}) {
		out.Name = &n
	}
	for _, p := range []Phone{{"home", c.HomePhone}, {"work", c.BusinessPhone}, {"mobile", c.MobilePhone}} {
		if p.Number != "" {
			out.Phones = append(out.Phones, p)
		}
	}
	for _, a := range []struct {
		kind string
		addr probe.Address
	}{{"home", c.Home}, {"work", c.Business}} {
		if !a.addr.Empty() {
			out.Addresses = append(out.Addresses, ContactAddress{Type: a.kind, Street: a.addr.Street, City: a.addr.City, State: a.addr.State,
				PostalCode: a.addr.PostalCode, Country: a.addr.Country})
		}
	}
	return out
}

// matches tells whether a contact has the text (lower case) in its name, an address, the company or a phone number.
func matches(c probe.Contact, search string) bool {
	if search == "" {
		return true
	}
	fields := append([]string{c.DisplayName(), c.FileAs, c.NickName, c.Company, c.HomePhone, c.BusinessPhone, c.MobilePhone}, c.Emails...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), search) {
			return true
		}
	}
	return false
}

// readContacts reads the address book (the folder, or the one of the mailbox) and keeps the contacts that match the search.
func readContacts(ctx context.Context, s *probe.Session, folder, search string) (probe.Folder, []probe.Contact, error) {
	if folder == "" {
		folder = "contacts"
	}
	f, err := folderOf(s, folder)
	if err != nil {
		return f, nil, err
	}
	all, err := s.ListContacts(ctx, f.ID)
	if err != nil {
		return f, nil, err
	}
	search = strings.ToLower(strings.TrimSpace(search))
	var out []probe.Contact
	for _, c := range all {
		if matches(c, search) {
			out = append(out, c)
		}
	}
	return f, out, nil
}

// Contacts lists the contacts of an address book by name. Without a folder the address book of the mailbox is used.
func (m *Manager) Contacts(ctx context.Context, account, folder string, p ContactParams) (*ContactsResult, error) {
	limit := p.Limit
	if limit == 0 {
		limit = 50
	}
	var out *ContactsResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, list, err := readContacts(ctx, s, folder, p.Search)
		if err != nil {
			return err
		}
		out = &ContactsResult{Account: a.Name, Folder: folderJSON(f), Total: len(list), Contacts: []Contact{}}
		for _, c := range list {
			if limit > 0 && len(out.Contacts) >= limit {
				break
			}
			out.Contacts = append(out.Contacts, contactJSON(c))
		}
		out.Count = len(out.Contacts)
		return nil
	})
	return out, err
}

// findContact reads the address book of the folder the id says (or the one given) and returns the contact.
func findContact(ctx context.Context, s *probe.Session, folder, id string) (probe.Contact, error) {
	f, err := folderForMail(s, folder, id)
	if err != nil {
		return probe.Contact{}, err
	}
	list, err := s.ListContacts(ctx, f.ID)
	if err != nil {
		return probe.Contact{}, err
	}
	for _, c := range list {
		if c.ID == id {
			return c, nil
		}
	}
	return probe.Contact{}, newError(http.StatusNotFound, "contact_not_found", fmt.Sprintf("the address book has no contact %s", id))
}

// Contact returns one contact with everything the server sends, except the photo.
func (m *Manager) Contact(ctx context.Context, account, folder, id string) (*ContactResult, error) {
	var out *ContactResult
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		c, err := findContact(ctx, s, folder, id)
		if err != nil {
			return err
		}
		out = &ContactResult{Account: a.Name, Contact: contactJSON(c)}
		return nil
	})
	return out, err
}

// VCard returns one contact as a vCard file (a .vcf), with the photo.
func (m *Manager) VCard(ctx context.Context, account, folder, id string) (*Raw, error) {
	var out *Raw
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		c, err := findContact(ctx, s, folder, id)
		if err != nil {
			return err
		}
		out = &Raw{Data: c.VCard(time.Now()), ContentType: "text/vcard; charset=utf-8", Filename: fileName(c.Folder, c.ID) + ".vcf"}
		return nil
	})
	return out, err
}

// Export returns the contacts of an address book that match the search as one vCard file, the photos included. Without a
// folder the address book of the mailbox is used.
func (m *Manager) Export(ctx context.Context, account, folder, search string) (*Raw, error) {
	var out *Raw
	err := m.with(ctx, account, func(a Account, s *probe.Session) error {
		f, list, err := readContacts(ctx, s, folder, search)
		if err != nil {
			return err
		}
		out = &Raw{Data: probe.VCards(list, time.Now()), ContentType: "text/vcard; charset=utf-8", Filename: "contacts-" + fileName(f.ID, "all") + ".vcf"}
		return nil
	})
	return out, err
}
