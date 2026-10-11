package probe_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
	"github.com/nashcom/eas-tools/probe"
)

var vcardNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func sampleContact() probe.Contact {
	return probe.Contact{
		ID: "10:5", Folder: "10", FirstName: "Jane", MiddleName: "Q.", LastName: "Doe, Smith", Title: "Dr.", Suffix: "Jr.", FileAs: "Doe, Jane", NickName: "JD",
		Company: "Example; Corp", Department: "Sales", JobTitle: "Account Manager", Office: "B12",
		Emails: []string{"jane@example.com", "jane.doe@example.org"}, HomePhone: "+49 1", BusinessPhone: "+49 2", MobilePhone: "+49 3",
		Home:     probe.Address{Street: "Main St 1\nApt 2", City: "Berlin", PostalCode: "10115", Country: "Germany"},
		Business: probe.Address{Street: "Work Rd 9", City: "Munich", State: "BY", PostalCode: "80331", Country: "Germany"},
		Birthday: time.Date(1980, 2, 3, 0, 0, 0, 0, time.UTC), Anniversary: time.Date(2010, 6, 1, 0, 0, 0, 0, time.UTC),
		WebPage: "https://example.com/jane", IMAddresses: []string{"jane@chat.example.com"}, Manager: "Mia Boss",
		Categories: []string{"Work", "VIP, big"}, Picture: []byte("\xff\xd8\xff\xe0jpeg-bytes"),
	}
}

func TestVCardHasTheFieldsOfAContact(t *testing.T) {
	lines := unfold(t, sampleContact().VCard(vcardNow))
	if lines[0] != "BEGIN:VCARD" || lines[len(lines)-1] != "END:VCARD" {
		t.Errorf("not a card: %q ... %q", lines[0], lines[len(lines)-1])
	}
	mustHave(t, lines,
		"VERSION:3.0", "UID:10-10.5@eas-client.invalid", "REV:20261010T120000Z",
		`N:Doe\, Smith;Jane;Q.;Dr.;Jr.`, `FN:Dr. Jane Q. Doe\, Smith Jr.`, "NICKNAME:JD",
		`ORG:Example\; Corp;Sales`, "TITLE:Account Manager",
		"EMAIL;TYPE=INTERNET,PREF:jane@example.com", "EMAIL;TYPE=INTERNET:jane.doe@example.org",
		"TEL;TYPE=HOME,VOICE:+49 1", "TEL;TYPE=WORK,VOICE:+49 2", "TEL;TYPE=CELL,VOICE:+49 3",
		`ADR;TYPE=HOME:;;Main St 1\nApt 2;Berlin;;10115;Germany`, "ADR;TYPE=WORK:;;Work Rd 9;Munich;BY;80331;Germany",
		"BDAY:1980-02-03", "X-ANNIVERSARY:2010-06-01", "URL:https://example.com/jane",
		`CATEGORIES:Work,VIP\, big`, "X-MS-IMADDRESS:jane@chat.example.com", "X-MANAGER:Mia Boss",
	)
	var photo string
	for _, l := range lines {
		if strings.HasPrefix(l, "PHOTO;ENCODING=b;TYPE=JPEG:") {
			photo = l
		}
	}
	if photo == "" {
		t.Error("the photo is missing")
	}
}

func TestVCardOfASmallContactHasNoEmptyFields(t *testing.T) {
	lines := unfold(t, probe.Contact{ID: "10:7", Folder: "10", Emails: []string{"only@example.com"}}.VCard(vcardNow))
	mustHave(t, lines, "N:;;;;", "FN:only@example.com", "EMAIL;TYPE=INTERNET,PREF:only@example.com")
	for _, l := range lines {
		for _, bad := range []string{"TEL", "ADR", "BDAY", "ORG", "TITLE", "URL", "PHOTO", "CATEGORIES", "NICKNAME"} {
			if strings.HasPrefix(l, bad) {
				t.Errorf("an empty field was written: %q", l)
			}
		}
	}
}

func TestVCardEscapesABackslash(t *testing.T) {
	c := probe.Contact{ID: "1", Folder: "10", LastName: `A\B`, Company: `C:\D`}
	lines := unfold(t, c.VCard(vcardNow))
	mustHave(t, lines, `N:A\\B;;;;`, `ORG:C:\\D`, `FN:A\\B`)
}

func TestVCardNameFallbacks(t *testing.T) {
	for name, c := range map[string]struct {
		c    probe.Contact
		want string
	}{
		"file as":    {probe.Contact{ID: "1", Folder: "10", FileAs: "Smith, Ann"}, `FN:Smith\, Ann`},
		"company":    {probe.Contact{ID: "1", Folder: "10", Company: "Acme"}, "FN:Acme"},
		"first only": {probe.Contact{ID: "1", Folder: "10", FirstName: "Ann"}, "FN:Ann"},
		"nothing":    {probe.Contact{ID: "1", Folder: "10"}, "FN:(no name)"},
	} {
		if lines := unfold(t, c.c.VCard(vcardNow)); !has(lines, c.want) {
			t.Errorf("%s: want %q in\n%s", name, c.want, strings.Join(lines, "\n"))
		}
	}
}

func TestVCardsAreOneFileOfManyCards(t *testing.T) {
	a, b := sampleContact(), probe.Contact{ID: "10:6", Folder: "10", FirstName: "Bob", Emails: []string{"bob@example.com"}}
	raw := probe.VCards([]probe.Contact{a, b}, vcardNow)
	lines := unfold(t, raw)
	n := 0
	for _, l := range lines {
		if l == "BEGIN:VCARD" {
			n++
		}
	}
	if n != 2 || lines[0] != "BEGIN:VCARD" || lines[len(lines)-1] != "END:VCARD" {
		t.Errorf("%d cards in %q ... %q", n, lines[0], lines[len(lines)-1])
	}
	if len(probe.VCards(nil, vcardNow)) != 0 {
		t.Error("no contacts, but the file is not empty")
	}
}

func TestListContactsReadsTheAddressBook(t *testing.T) {
	srv, s := mailbox(t)
	srv.SeedContact("bob", easmockserver.Contact{FirstName: "Zed", LastName: "Zebra", Email1: "zed@example.com", MobilePhone: "+1 2", Categories: []string{"A", "B"},
		Home: easmockserver.Address{City: "Bonn", Country: "DE"}, Birthday: time.Date(1990, 5, 6, 0, 0, 0, 0, time.UTC), Picture: []byte("pic"), NickName: "Z", IMAddress: "z@im", Manager: "Boss"})
	srv.SeedContact("bob", easmockserver.Contact{FirstName: "Amy", LastName: "Adams", Email1: "amy@example.com", Email2: "amy2@example.com", Company: "Acme"})

	book, err := s.FindFolder("contacts")
	if err != nil || book.ID != "10" {
		t.Fatalf("contacts folder = %+v, %v", book, err)
	}
	list, err := s.ListContacts(t.Context(), book.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("%d contacts, %v", len(list), err)
	}
	if list[0].LastName != "Adams" || list[1].LastName != "Zebra" {
		t.Errorf("order: %q, %q; want by name", list[0].LastName, list[1].LastName)
	}
	a, z := list[0], list[1]
	if len(a.Emails) != 2 || a.Emails[1] != "amy2@example.com" || a.Company != "Acme" || a.Folder != "10" {
		t.Errorf("amy = %+v", a)
	}
	if z.MobilePhone != "+1 2" || strings.Join(z.Categories, ",") != "A,B" || z.Home.City != "Bonn" || !z.Birthday.Equal(time.Date(1990, 5, 6, 0, 0, 0, 0, time.UTC)) ||
		string(z.Picture) != "pic" || z.NickName != "Z" || len(z.IMAddresses) != 1 || z.Manager != "Boss" {
		t.Errorf("zed = %+v", z)
	}
}

func TestListContactsReadsAllPages(t *testing.T) {
	srv, s := mailbox(t)
	for i := 0; i < 130; i++ { // more than the 100 of a page
		srv.SeedContact("bob", easmockserver.Contact{LastName: "N", FirstName: strings.Repeat("x", 1+i%7), Email1: "x@example.com"})
	}
	for round := 0; round < 2; round++ { // a second list starts from the beginning again
		list, err := s.ListContacts(t.Context(), "10")
		if err != nil || len(list) != 130 {
			t.Fatalf("round %d: %d contacts, %v; want 130", round, len(list), err)
		}
	}
}

// A Traveler answered the contacts Sync of the EAS library, which sends no Options, with status 3. The list sends its own.
func TestListContactsWorksWithAServerThatNeedsOptions(t *testing.T) {
	srv := easmockserver.NewWithOptions(easmockserver.Options{Mailbox: true, RequireOptions: true})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	s, err := probe.NewSession(probe.Config{ServerURL: ts.URL, Username: "bob", Email: "bob@example.com", Password: easmockserver.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	for _, step := range []func() error{
		func() error { return s.Options(ctx, nil) }, func() error { return s.Provision(ctx, nil) }, func() error { return s.FolderSync(ctx, nil) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	srv.SeedContact("bob", easmockserver.Contact{FirstName: "Ann", LastName: "Lee", Email1: "ann@example.com", Notes: "likes tea\nand coffee"})

	list, err := s.ListContacts(ctx, "10")
	if err != nil || len(list) != 1 {
		t.Fatalf("%d contacts, %v", len(list), err)
	}
	if list[0].Notes != "likes tea\nand coffee" || list[0].LastName != "Lee" {
		t.Errorf("contact = %+v", list[0])
	}
	lines := unfold(t, list[0].VCard(vcardNow))
	mustHave(t, lines, `NOTE:likes tea\nand coffee`)
}
