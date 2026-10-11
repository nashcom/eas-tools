package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/client"
	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func TestContactsFromTheCommandLineMatchREST(t *testing.T) {
	e := start(t)
	jane := e.srv.SeedContact("bob", easmockserver.Contact{FirstName: "Jane", LastName: "Doe", Email1: "jane@example.com", Company: "Acme", Picture: []byte("\xff\xd8\xffx")})
	e.srv.SeedContact("bob", easmockserver.Contact{FirstName: "Bob", LastName: "Baker", Email1: "bob@example.org"})

	m, err := client.NewManager([]client.Account{{Name: "default", Server: e.url, Email: "bob@example.com", User: "bob", Password: easmockserver.Password}}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	api := httptest.NewServer(m.Handler(""))
	defer api.Close()

	// JSON: the same bytes
	for _, c := range []struct {
		path string
		args []string
	}{
		{"/accounts/default/contacts", []string{"contacts", "list"}},
		{"/accounts/default/contacts?search=acme&limit=all", []string{"contacts", "list", "-search", "acme", "-limit", "all"}},
		{"/accounts/default/contacts/" + jane, []string{"contacts", "get", jane}},
		{"/accounts/default/folders/10/contacts/" + jane, []string{"contacts", "get", "10", jane}},
	} {
		code, out, errs := e.eas(t, "", c.args...)
		if code != 0 || !strings.Contains(out, "Doe") {
			t.Fatalf("%v: %d %s %s", c.args, code, out, errs)
		}
		resp, err := http.Get(api.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			b.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()
		if out != b.String() {
			t.Errorf("%v printed\n%s\nthe API answers\n%s", c.args, out, b.String())
		}
	}

	// the files: raw vCard text on stdout
	code, out, errs := e.eas(t, "", "contacts", "vcf", jane)
	if code != 0 || !strings.HasPrefix(out, "BEGIN:VCARD\r\n") || !strings.Contains(out, "FN:Jane Doe\r\n") || !strings.Contains(out, "PHOTO;") || !strings.HasSuffix(out, "END:VCARD\r\n") {
		t.Errorf("vcf: %d %q %s", code, out, errs)
	}
	code, out, errs = e.eas(t, "", "contacts", "export")
	if code != 0 || strings.Count(out, "BEGIN:VCARD") != 2 {
		t.Errorf("export: %d %q %s", code, out, errs)
	}
	code, out, errs = e.eas(t, "", "contacts", "export", "-search", "baker")
	if code != 0 || strings.Count(out, "BEGIN:VCARD") != 1 || !strings.Contains(out, "FN:Bob Baker") {
		t.Errorf("export with a search: %d %q %s", code, out, errs)
	}

	// errors
	if code, out, errs := e.eas(t, "", "contacts", "get", "10:99999"); code != 1 || out != "" || !strings.Contains(errs, "contact_not_found") {
		t.Errorf("unknown contact: %d %q %s", code, out, errs)
	}
	for _, args := range [][]string{{"contacts"}, {"contacts", "nothing"}, {"contacts", "get"}, {"contacts", "vcf"}, {"contacts", "list", "a", "b"}, {"contacts", "list", "-limit", "0"}} {
		if code, out, errs := e.eas(t, "", args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d, %q %q; want a usage error", args, code, out, errs)
		}
	}
}
