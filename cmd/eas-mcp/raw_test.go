package main

import (
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

// The command line has three commands that print a file and not JSON: calendar ics, contacts vcf and contacts export (and eml). The tools give
// the same raw text.
func TestTheRawFormsOfEventsAndContacts(t *testing.T) {
	cs, srv := session(t, false, nil)
	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Minute)
	eid := srv.SeedEvent("bob", easmockserver.Event{UID: "u-ics", Subject: "Event for the file", Start: start, End: start.Add(time.Hour)})
	jane := srv.SeedContact("bob", easmockserver.Contact{FirstName: "Jane", LastName: "Doe", Email1: "jane@example.com", Company: "Acme", Picture: []byte("\xff\xd8\xffphotobytes")})
	srv.SeedContact("bob", easmockserver.Contact{FirstName: "Bob", LastName: "Baker", Email1: "bob@example.org"})

	ics, bad := call(t, cs, "get_event_ics", map[string]any{"id": eid})
	if bad || !strings.Contains(ics, "BEGIN:VCALENDAR") || !strings.Contains(ics, "BEGIN:VEVENT") || !strings.Contains(ics, "Event for the file") {
		t.Errorf("get_event_ics: %v\n%s", bad, ics)
	}
	if strings.HasPrefix(strings.TrimSpace(ics), "{") {
		t.Errorf("the iCalendar text is JSON: %s", ics)
	}

	card, bad := call(t, cs, "get_contact_vcard", map[string]any{"id": jane})
	if bad || !strings.Contains(card, "BEGIN:VCARD") || !strings.Contains(card, "jane@example.com") || !strings.Contains(strings.ToUpper(card), "PHOTO") {
		t.Errorf("get_contact_vcard: %v\n%s", bad, card)
	}
	short, bad := call(t, cs, "get_contact_vcard", map[string]any{"id": jane, "no_photo": true})
	if bad || !strings.Contains(short, "jane@example.com") || strings.Contains(strings.ToUpper(short), "PHOTO") || len(short) >= len(card) {
		t.Errorf("get_contact_vcard without the photo: %v\n%s", bad, short)
	}

	all, bad := call(t, cs, "export_contacts", map[string]any{"no_photo": true})
	if bad || strings.Count(all, "BEGIN:VCARD") != 2 || !strings.Contains(all, "bob@example.org") {
		t.Errorf("export_contacts: %v\n%s", bad, all)
	}
	found, bad := call(t, cs, "export_contacts", map[string]any{"search": "acme", "no_photo": true})
	if bad || strings.Count(found, "BEGIN:VCARD") != 1 || strings.Contains(found, "Baker") {
		t.Errorf("export_contacts with a search: %v\n%s", bad, found)
	}
	cut, bad := call(t, cs, "export_contacts", map[string]any{"max_bytes": 40})
	if bad || !strings.Contains(cut, "the text is cut here: 40 of ") {
		t.Errorf("export_contacts with a short limit: %v\n%s", bad, cut)
	}
}

func TestThePhotoIsTakenOutOfAVCardWithItsContinuationLines(t *testing.T) {
	in := "BEGIN:VCARD\r\nFN:Jane\r\nPHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQ\r\n SkZJRgABAQ\r\n\tAAAA\r\nEMAIL:jane@example.com\r\nEND:VCARD\r\n"
	want := "BEGIN:VCARD\r\nFN:Jane\r\nEMAIL:jane@example.com\r\nEND:VCARD\r\n"
	if got := string(withoutPhoto([]byte(in))); got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestAMailCanBeSentWithoutACopyInSentItems(t *testing.T) {
	cs, srv := session(t, true, nil)
	sent := func() int { return len(srv.Messages("bob", "6")) }
	if _, bad := call(t, cs, "send_message", map[string]any{"to": []string{"carol@example.com"}, "subject": "a", "body": "b"}); bad {
		t.Fatal("send_message failed")
	}
	before := sent()
	if before != 1 {
		t.Skipf("the mock server keeps %d copies for a normal send: the check of the option below needs one", before)
	}
	if _, bad := call(t, cs, "send_message", map[string]any{"to": []string{"carol@example.com"}, "subject": "c", "body": "d", "no_sent_copy": true}); bad {
		t.Fatal("send_message with no_sent_copy failed")
	}
	if n := sent(); n != before {
		t.Errorf("%d copies in Sent Items, want %d: no_sent_copy was ignored", n, before)
	}
}

// The JSON tools give the result as JSON text and as structured content; the tools of the raw forms give plain text only, as the command line
// prints a file and not JSON.
func TestTheFormsOfTheAnswers(t *testing.T) {
	cs, srv := session(t, false, nil)
	id := srv.Seed("bob", easmockserver.Mail{Subject: "forms", From: "alice@example.com", To: "bob@example.com", Body: "text"})
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_folders"})
	if err != nil || res.IsError {
		t.Fatalf("list_folders: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(strings.TrimSpace(text), "{") || res.StructuredContent == nil {
		t.Errorf("a JSON tool: text %.40q, structured content %v", text, res.StructuredContent)
	}
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_message_eml", Arguments: map[string]any{"id": id}})
	if err != nil || res.IsError {
		t.Fatalf("get_message_eml: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; strings.HasPrefix(strings.TrimSpace(text), "{") || res.StructuredContent != nil || !strings.Contains(text, "Subject: forms") {
		t.Errorf("a raw tool: text %.60q, structured content %v", text, res.StructuredContent)
	}
}
