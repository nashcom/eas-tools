package probe

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hstern/go-activesync/eas"
	"github.com/hstern/go-activesync/wbxml"
)

// Address is a postal address of a contact.
type Address struct {
	Street, City, State, PostalCode, Country string
}

// Empty reports whether the address has no part.
func (a Address) Empty() bool { return a == Address{} }

// Contact is an entry of the address book.
type Contact struct {
	ID         string
	Folder     string
	FirstName  string
	MiddleName string
	LastName   string
	Title      string // a form of address such as Dr.
	Suffix     string
	FileAs     string
	NickName   string

	Company    string
	Department string
	JobTitle   string
	Office     string
	Manager    string

	Emails        []string
	HomePhone     string
	BusinessPhone string
	MobilePhone   string
	IMAddresses   []string
	WebPage       string

	Home     Address
	Business Address

	Birthday    time.Time
	Anniversary time.Time
	Categories  []string
	Notes       string // the text of the contact; cut to 4096 bytes by the request
	Picture     []byte // the photo, as the server has it (a JPEG)
}

// DisplayName is the name of the contact as it is shown: form of address, first, middle and last name, suffix; else the
// "file as" text, the company, an address or "(no name)".
func (c Contact) DisplayName() string {
	if n := joinWords(c.Title, c.FirstName, c.MiddleName, c.LastName, c.Suffix); n != "" {
		return n
	}
	for _, s := range []string{c.FileAs, c.Company} {
		if s != "" {
			return s
		}
	}
	if len(c.Emails) > 0 {
		return c.Emails[0]
	}
	return "(no name)"
}

func joinWords(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// bareAddr is the address of "Name <a@b>", or the text itself when it is not a header form.
func bareAddr(s string) string {
	s = strings.TrimSpace(s)
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}

// contactFromApp reads the ApplicationData of a contact of a Sync. The names are those of the Contacts and Contacts2 code
// pages of MS-ASCNTC.
func contactFromApp(folder, id string, app *wbxml.Element) Contact {
	out := Contact{ID: id, Folder: folder}
	if app == nil {
		return out
	}
	var emails, ims []string
	set := map[string]*string{
		"FirstName": &out.FirstName, "LastName": &out.LastName, "MiddleName": &out.MiddleName, "Title": &out.Title, "Suffix": &out.Suffix,
		"FileAs": &out.FileAs, "CompanyName": &out.Company, "Department": &out.Department, "JobTitle": &out.JobTitle,
		"OfficeLocation": &out.Office, "HomePhoneNumber": &out.HomePhone, "BusinessPhoneNumber": &out.BusinessPhone,
		"MobilePhoneNumber": &out.MobilePhone, "WebPage": &out.WebPage,
		"HomeAddressStreet": &out.Home.Street, "HomeAddressCity": &out.Home.City, "HomeAddressState": &out.Home.State,
		"HomeAddressPostalCode": &out.Home.PostalCode, "HomeAddressCountry": &out.Home.Country,
		"BusinessAddressStreet": &out.Business.Street, "BusinessAddressCity": &out.Business.City, "BusinessAddressState": &out.Business.State,
		"BusinessAddressPostalCode": &out.Business.PostalCode, "BusinessAddressCountry": &out.Business.Country,
	}
	for _, c := range app.Children {
		el, ok := c.(*wbxml.Element)
		if !ok {
			continue
		}
		v := el.TextContent()
		switch el.Codepage {
		case wbxml.PageContacts:
			switch el.Name {
			case "Email1Address", "Email2Address", "Email3Address":
				if e := bareAddr(v); e != "" {
					emails = append(emails, e)
				}
			case "Birthday":
				out.Birthday = parseEASTimeText(v)
			case "Anniversary":
				out.Anniversary = parseEASTimeText(v)
			case "Picture":
				out.Picture = decodePicture(v)
			case "Categories":
				for _, cc := range el.Children {
					if ce, ok := cc.(*wbxml.Element); ok && ce.Name == "Category" {
						out.Categories = append(out.Categories, ce.TextContent())
					}
				}
			default:
				if p := set[el.Name]; p != nil {
					*p = v
				}
			}
		case wbxml.PageContacts2:
			switch el.Name {
			case "NickName":
				out.NickName = v
			case "ManagerName":
				out.Manager = v
			case "IMAddress", "IMAddress2", "IMAddress3":
				if v != "" {
					ims = append(ims, v)
				}
			}
		case wbxml.PageAirSyncBase:
			if el.Name == "Body" {
				if d := el.Find("Data"); d != nil {
					out.Notes = d.TextContent()
				}
			}
		}
	}
	out.Emails, out.IMAddresses = emails, ims
	return out
}

// syncContactsOnce sends one Sync of an address book. The request is made here and not by the EAS library, whose contacts Sync
// has no Options: an HCL Traveler answered it with status 3 (invalid sync key) although the key was right. The Options ask
// for the notes of the contacts as plain text, like the Sync of the mail does for the body.
func (s *Session) syncContactsOnce(ctx context.Context, folder, key string) (items []Contact, newKey string, more bool, err error) {
	collection := wbxml.E(wbxml.PageAirSync, "Collection",
		wbxml.E(wbxml.PageAirSync, "SyncKey", wbxml.Text(key)),
		wbxml.E(wbxml.PageAirSync, "CollectionId", wbxml.Text(folder)),
		wbxml.E(wbxml.PageAirSync, "DeletesAsMoves", wbxml.Text("1")),
		wbxml.E(wbxml.PageAirSync, "GetChanges", wbxml.Text("1")),
		wbxml.E(wbxml.PageAirSync, "WindowSize", wbxml.Text("100")),
		wbxml.E(wbxml.PageAirSync, "Options",
			wbxml.E(wbxml.PageAirSyncBase, "BodyPreference",
				wbxml.E(wbxml.PageAirSyncBase, "Type", wbxml.Text("1")),
				wbxml.E(wbxml.PageAirSyncBase, "TruncationSize", wbxml.Text("4096")),
			),
		),
	)
	body, err := wbxml.Marshal(&wbxml.Document{Root: wbxml.E(wbxml.PageAirSync, "Sync",
		wbxml.E(wbxml.PageAirSync, "Collections", collection))}, wbxml.DefaultRegistry())
	if err != nil {
		return nil, "", false, fmt.Errorf("eas: Sync: marshal: %w", err)
	}
	raw, err := s.postWBXML(ctx, "Sync", body)
	if err != nil {
		return nil, "", false, err
	}
	if len(raw) == 0 {
		return nil, key, false, nil // an empty reply: nothing changed
	}
	doc, err := wbxml.Unmarshal(raw, wbxml.DefaultRegistry())
	if err != nil || doc.Root == nil {
		return nil, "", false, fmt.Errorf("eas: Sync: decode response: %v", err)
	}
	if st := childText(doc.Root, "Status"); st != "" && st != "1" {
		code, _ := strconv.Atoi(st)
		return nil, "", false, &eas.StatusError{Command: "Sync", Code: code}
	}
	coll := doc.Root.Find("Collection")
	if coll == nil {
		return nil, "", false, fmt.Errorf("eas: Sync: response without a Collection")
	}
	if st := childText(coll, "Status"); st != "" && st != "1" {
		code, _ := strconv.Atoi(st)
		return nil, "", false, &eas.StatusError{Command: "Sync", Code: code}
	}
	newKey = childText(coll, "SyncKey")
	if newKey == "" {
		return nil, "", false, fmt.Errorf("eas: Sync: response without a SyncKey")
	}
	more = coll.Find("MoreAvailable") != nil
	if cmds := coll.Find("Commands"); cmds != nil {
		for _, c := range cmds.Children {
			el, ok := c.(*wbxml.Element)
			if !ok || (el.Name != "Add" && el.Name != "Change") {
				continue
			}
			items = append(items, contactFromApp(folder, childText(el, "ServerId"), el.Find("ApplicationData")))
		}
	}
	if err := s.state.SetSyncKey(ctx, folder, newKey); err != nil {
		return nil, "", false, err
	}
	return items, newKey, more, nil
}

// ListContacts reads an address book folder from the start and returns its contacts, by name. A Sync from key 0 first brings
// the sync key and then the contacts, in pages.
func (s *Session) ListContacts(ctx context.Context, folder string) ([]Contact, error) {
	if err := s.ensureClient(); err != nil {
		return nil, err
	}
	s.rec.setStep(StepSync)
	key := "0"
	if err := s.state.SetSyncKey(ctx, folder, key); err != nil {
		return nil, err
	}
	var out []Contact
	for page := 0; page < mailMaxPages; page++ {
		items, newKey, more, err := s.syncContactsOnce(ctx, folder, key)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		key = newKey
		if page >= 1 && !more { // the first request is the one that brings the key
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ka, kb := strings.ToLower(a.LastName+"\x00"+a.FirstName+"\x00"+a.DisplayName()), strings.ToLower(b.LastName+"\x00"+b.FirstName+"\x00"+b.DisplayName())
		return ka < kb
	})
	return out, nil
}

// parseEASTimeText reads the date of a contact in the forms that servers use; zero when it is not one of them.
func parseEASTimeText(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z", time.RFC3339Nano, "20060102T150405Z", "20060102T150405", "20060102", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// decodePicture reads the photo of a contact, which the server sends as base64 text.
func decodePicture(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return b
}
