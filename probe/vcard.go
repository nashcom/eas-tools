package probe

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// VCard writes the contact as a vCard 3.0 (RFC 2426), the version that Outlook, Apple, Google and most address books import.
// EAS sends the fields of a contact and no vCard text, so this is built from the fields. What a vCard has no place for (the
// government id and customer id of the server, the office location) is left out. now is the REV.
func (c Contact) VCard(now time.Time) []byte {
	var w icalWriter
	w.line("BEGIN:VCARD")
	w.line("VERSION:3.0")
	w.line("PRODID:-//Nash!Com//eas-client " + Version + "//EN")
	w.line("UID:" + c.Folder + "-" + strings.ReplaceAll(c.ID, ":", ".") + "@eas-client.invalid")
	w.line("REV:" + icalTime(now))
	w.line("N:" + strings.Join([]string{vEsc(c.LastName), vEsc(c.FirstName), vEsc(c.MiddleName), vEsc(c.Title), vEsc(c.Suffix)}, ";"))
	w.text("FN", c.DisplayName())
	if c.NickName != "" {
		w.text("NICKNAME", c.NickName)
	}
	if c.Company != "" || c.Department != "" {
		org := vEsc(c.Company)
		if c.Department != "" {
			org += ";" + vEsc(c.Department)
		}
		w.line("ORG:" + org)
	}
	if c.JobTitle != "" {
		w.text("TITLE", c.JobTitle)
	}
	for i, e := range c.Emails {
		kind := "INTERNET"
		if i == 0 {
			kind = "INTERNET,PREF"
		}
		w.line("EMAIL;TYPE=" + kind + ":" + e)
	}
	for _, p := range []struct{ kind, number string }{{"HOME", c.HomePhone}, {"WORK", c.BusinessPhone}, {"CELL", c.MobilePhone}} {
		if p.number != "" {
			w.text("TEL;TYPE="+p.kind+",VOICE", p.number)
		}
	}
	for _, a := range []struct {
		kind string
		addr Address
	}{{"HOME", c.Home}, {"WORK", c.Business}} {
		if !a.addr.Empty() {
			w.line("ADR;TYPE=" + a.kind + ":;;" + strings.Join([]string{vEsc(a.addr.Street), vEsc(a.addr.City), vEsc(a.addr.State), vEsc(a.addr.PostalCode), vEsc(a.addr.Country)}, ";"))
		}
	}
	if !c.Birthday.IsZero() {
		w.line("BDAY:" + c.Birthday.UTC().Format("2006-01-02"))
	}
	if !c.Anniversary.IsZero() {
		w.line("X-ANNIVERSARY:" + c.Anniversary.UTC().Format("2006-01-02"))
	}
	if c.WebPage != "" {
		w.line("URL:" + c.WebPage)
	}
	if c.Notes != "" {
		w.text("NOTE", c.Notes)
	}
	if len(c.Categories) > 0 {
		var cats []string
		for _, v := range c.Categories {
			cats = append(cats, vEsc(v))
		}
		w.line("CATEGORIES:" + strings.Join(cats, ","))
	}
	for _, im := range c.IMAddresses {
		w.text("X-MS-IMADDRESS", im)
	}
	if c.Manager != "" {
		w.text("X-MANAGER", c.Manager)
	}
	if len(c.Picture) > 0 {
		kind := "JPEG"
		if strings.HasSuffix(http.DetectContentType(c.Picture), "png") {
			kind = "PNG"
		}
		w.line("PHOTO;ENCODING=b;TYPE=" + kind + ":" + base64.StdEncoding.EncodeToString(c.Picture))
	}
	w.line("END:VCARD")
	return []byte(w.b.String())
}

// VCards writes the contacts as one file of vCards, one after the other.
func VCards(contacts []Contact, now time.Time) []byte {
	var out []byte
	for _, c := range contacts {
		out = append(out, c.VCard(now)...)
	}
	return out
}

// vEsc escapes a part of a structured value: backslash, semicolon, comma and line breaks.
func vEsc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
}
