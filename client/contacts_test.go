package client_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func seedBook(srv *easmockserver.Server) (jane, bob string) {
	jane = srv.SeedContact("bob", easmockserver.Contact{FirstName: "Jane", LastName: "Doe", Email1: "jane@example.com", Company: "Acme",
		MobilePhone: "+49 3", Home: easmockserver.Address{City: "Berlin", Country: "Germany"}, Categories: []string{"VIP"},
		Birthday: time.Date(1980, 2, 3, 0, 0, 0, 0, time.UTC), Picture: []byte("\xff\xd8\xffphoto")})
	bob = srv.SeedContact("bob", easmockserver.Contact{FirstName: "Bob", LastName: "Baker", Email1: "bob@example.org", NickName: "Bobby"})
	return jane, bob
}

func TestContactsListSearchAndGet(t *testing.T) {
	srv, m := setup(t, false, "bob") // reading only
	jane, _ := seedBook(srv)

	l, err := m.Contacts(t.Context(), "bob", "", client.ContactParams{})
	if err != nil || l.Total != 2 || l.Count != 2 || l.Folder.Type != "contacts" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if l.Contacts[0].DisplayName != "Bob Baker" || l.Contacts[1].DisplayName != "Jane Doe" { // by last name
		t.Errorf("order: %q, %q", l.Contacts[0].DisplayName, l.Contacts[1].DisplayName)
	}
	j := l.Contacts[1]
	if j.Company != "Acme" || len(j.Emails) != 1 || len(j.Phones) != 1 || j.Phones[0].Type != "mobile" || len(j.Addresses) != 1 || j.Addresses[0].Type != "home" ||
		j.Birthday == nil || !j.HasPhoto || j.PhotoBytes != 8 || j.Name == nil || j.Name.Last != "Doe" {
		t.Errorf("jane = %+v", j)
	}

	for search, want := range map[string]int{"jane": 1, "ACME": 1, "example.org": 1, "bobby": 1, "+49": 1, "nobody": 0, "": 2} {
		l, err := m.Contacts(t.Context(), "bob", "", client.ContactParams{Search: search})
		if err != nil || l.Total != want {
			t.Errorf("search %q: %d contacts, %v; want %d", search, l.Total, err, want)
		}
	}
	if l, _ := m.Contacts(t.Context(), "bob", "", client.ContactParams{Limit: 1}); l == nil || l.Count != 1 || l.Total != 2 {
		t.Errorf("limit 1: %+v", l)
	}

	g, err := m.Contact(t.Context(), "bob", "", jane) // the folder from the id
	if err != nil || g.Contact.DisplayName != "Jane Doe" || g.Contact.Folder != "10" {
		t.Fatalf("get = %+v, %v", g, err)
	}
	if _, err := m.Contact(t.Context(), "bob", "contacts", "10:99999"); code(t, err) != "contact_not_found" {
		t.Errorf("unknown contact: %v", err)
	}
	if _, err := m.Contacts(t.Context(), "bob", "nothing", client.ContactParams{}); code(t, err) != "folder_not_found" {
		t.Errorf("unknown folder: %v", err)
	}
}

func TestVCardAndExport(t *testing.T) {
	srv, m := setup(t, false, "bob")
	jane, _ := seedBook(srv)

	one, err := m.VCard(t.Context(), "bob", "", jane)
	if err != nil {
		t.Fatal(err)
	}
	text := string(one.Data)
	for _, want := range []string{"BEGIN:VCARD\r\n", "FN:Jane Doe\r\n", "EMAIL;TYPE=INTERNET,PREF:jane@example.com\r\n", "BDAY:1980-02-03\r\n", "PHOTO;ENCODING=b;TYPE=JPEG:", "END:VCARD\r\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the vCard lacks %q:\n%s", want, text)
		}
	}
	if !strings.HasPrefix(one.ContentType, "text/vcard") || !strings.HasSuffix(one.Filename, ".vcf") {
		t.Errorf("content type %q, file name %q", one.ContentType, one.Filename)
	}

	all, err := m.Export(t.Context(), "bob", "", "")
	if err != nil || strings.Count(string(all.Data), "BEGIN:VCARD") != 2 || !strings.HasSuffix(all.Filename, ".vcf") {
		t.Fatalf("export = %q, %v", all, err)
	}
	some, err := m.Export(t.Context(), "bob", "contacts", "baker")
	if err != nil || strings.Count(string(some.Data), "BEGIN:VCARD") != 1 || !strings.Contains(string(some.Data), "FN:Bob Baker") {
		t.Errorf("export of a search = %q, %v", some, err)
	}
	empty, err := m.Export(t.Context(), "bob", "", "nobody-at-all")
	if err != nil || len(empty.Data) != 0 {
		t.Errorf("export without a match = %q, %v", empty, err)
	}
	if _, err := m.VCard(t.Context(), "bob", "contacts", "10:99999"); code(t, err) != "contact_not_found" {
		t.Errorf("unknown contact: %v", err)
	}
}

func TestRESTContacts(t *testing.T) {
	srv, m, ts := api(t, false, "", "bob")
	jane, _ := seedBook(srv)

	for path, f := range map[string]func() (any, error){
		"/accounts/bob/contacts": func() (any, error) { return m.Contacts(t.Context(), "bob", "", client.ContactParams{}) },
		"/accounts/bob/contacts?search=jane&limit=1": func() (any, error) {
			return m.Contacts(t.Context(), "bob", "", client.ContactParams{Search: "jane", Limit: 1})
		},
		"/accounts/bob/folders/contacts/contacts?limit=all": func() (any, error) {
			return m.Contacts(t.Context(), "bob", "contacts", client.ContactParams{Limit: -1})
		},
		"/accounts/bob/contacts/" + jane:                  func() (any, error) { return m.Contact(t.Context(), "bob", "", jane) },
		"/accounts/bob/folders/contacts/contacts/" + jane: func() (any, error) { return m.Contact(t.Context(), "bob", "contacts", jane) },
	} {
		want, err := f()
		if err != nil {
			t.Fatal(path, err)
		}
		status, got := do(t, "GET", ts.URL+path, "", "")
		if status != 200 || got != encoded(t, want) {
			t.Errorf("GET %s = %d\n%s\nwant\n%s", path, status, got, encoded(t, want))
		}
	}

	// the files: raw bytes, vCard
	strip := func(b []byte) string { // the REV is the time of the request
		var out []string
		for _, l := range strings.Split(string(b), "\r\n") {
			if !strings.HasPrefix(l, "REV:") {
				out = append(out, l)
			}
		}
		return strings.Join(out, "\r\n")
	}
	one, _ := m.VCard(t.Context(), "bob", "", jane)
	all, _ := m.Export(t.Context(), "bob", "", "")
	for path, want := range map[string]*client.Raw{
		"/accounts/bob/contacts/" + jane + "/vcf":                  one,
		"/accounts/bob/folders/contacts/contacts/" + jane + "/vcf": one,
		"/accounts/bob/contacts.vcf":                               all,
		"/accounts/bob/folders/contacts/contacts.vcf":              all,
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strip(body) != strip(want.Data) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/vcard") {
			t.Errorf("GET %s = %d, type %q:\n%s", path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	if resp, err := http.Get(ts.URL + "/accounts/bob/contacts.vcf?search=baker"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if bytes.Count(body, []byte("BEGIN:VCARD")) != 1 {
			t.Errorf("export of a search: %s", body)
		}
	}
	if status, body := do(t, "GET", ts.URL+"/accounts/bob/contacts/10:99999/vcf", "", ""); status != 404 || !strings.Contains(body, "contact_not_found") {
		t.Errorf("unknown contact = %d %s", status, body)
	}
	if status, _ := do(t, "POST", ts.URL+"/accounts/bob/contacts", "", `{}`); status != 405 {
		t.Errorf("POST = %d, want 405: the address book is read only", status)
	}
}
