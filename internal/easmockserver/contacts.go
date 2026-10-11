package easmockserver

import (
	"encoding/base64"
	"strconv"
	"time"

	"github.com/hstern/go-activesync/wbxml"
)

// FolderContacts is the contacts folder of the mailbox mode.
const FolderContacts = "10"

// Address is a postal address of a Contact.
type Address struct{ Street, City, State, PostalCode, Country string }

// Contact is an address book entry for SeedContact (Options.Mailbox only).
type Contact struct {
	Folder                                         string // default: the contacts folder
	FirstName, MiddleName, LastName, Title, Suffix string
	FileAs, NickName                               string
	Company, Department, JobTitle, Office          string
	Email1, Email2, Email3                         string
	HomePhone, BusinessPhone, MobilePhone          string
	Home, Business                                 Address
	Birthday, Anniversary                          time.Time
	WebPage, IMAddress, Manager                    string
	Notes                                          string
	Categories                                     []string
	Picture                                        []byte
}

// SeedContact puts a contact into the address book of a user and returns its server id.
func (f *Server) SeedContact(user string, c Contact) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c.Folder == "" {
		c.Folder = FolderContacts
	}
	return f.boxPut(user, &boxMsg{folder: c.Folder, subject: c.FileAs, at: time.Now(), ct: &c})
}

// contactData is the ApplicationData of a contact.
func contactData(m *boxMsg) *wbxml.Element {
	c := m.ct
	out := wbxml.E(wbxml.PageAirSync, "ApplicationData")
	add := func(page byte, name, v string) {
		if v != "" {
			out.Children = append(out.Children, txt(page, name, v))
		}
	}
	p := wbxml.PageContacts
	add(p, "FirstName", c.FirstName)
	add(p, "LastName", c.LastName)
	add(p, "MiddleName", c.MiddleName)
	add(p, "Title", c.Title)
	add(p, "Suffix", c.Suffix)
	add(p, "FileAs", c.FileAs)
	add(p, "CompanyName", c.Company)
	add(p, "Department", c.Department)
	add(p, "JobTitle", c.JobTitle)
	add(p, "OfficeLocation", c.Office)
	add(p, "Email1Address", c.Email1)
	add(p, "Email2Address", c.Email2)
	add(p, "Email3Address", c.Email3)
	add(p, "HomePhoneNumber", c.HomePhone)
	add(p, "BusinessPhoneNumber", c.BusinessPhone)
	add(p, "MobilePhoneNumber", c.MobilePhone)
	for _, a := range []struct {
		prefix string
		addr   Address
	}{{"Home", c.Home}, {"Business", c.Business}} {
		add(p, a.prefix+"AddressStreet", a.addr.Street)
		add(p, a.prefix+"AddressCity", a.addr.City)
		add(p, a.prefix+"AddressState", a.addr.State)
		add(p, a.prefix+"AddressPostalCode", a.addr.PostalCode)
		add(p, a.prefix+"AddressCountry", a.addr.Country)
	}
	if !c.Birthday.IsZero() {
		add(p, "Birthday", easTime(c.Birthday))
	}
	if !c.Anniversary.IsZero() {
		add(p, "Anniversary", easTime(c.Anniversary))
	}
	add(p, "WebPage", c.WebPage)
	if len(c.Picture) > 0 {
		add(p, "Picture", base64.StdEncoding.EncodeToString(c.Picture))
	}
	if len(c.Categories) > 0 {
		cats := wbxml.E(p, "Categories")
		for _, v := range c.Categories {
			cats.Children = append(cats.Children, txt(p, "Category", v))
		}
		out.Children = append(out.Children, cats)
	}
	add(wbxml.PageContacts2, "NickName", c.NickName)
	add(wbxml.PageContacts2, "IMAddress", c.IMAddress)
	add(wbxml.PageContacts2, "ManagerName", c.Manager)
	if c.Notes != "" {
		out.Children = append(out.Children, wbxml.E(wbxml.PageAirSyncBase, "Body",
			txt(wbxml.PageAirSyncBase, "Type", "1"),
			txt(wbxml.PageAirSyncBase, "EstimatedDataSize", strconv.Itoa(len(c.Notes))),
			txt(wbxml.PageAirSyncBase, "Data", c.Notes)))
	}
	return out
}
