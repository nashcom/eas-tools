package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nashcom/eas-tools/internal/easmockserver"
)

func TestCalendarAndContactsCanBeRead(t *testing.T) {
	cs, srv := session(t, false, nil)
	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Minute)
	eid := srv.SeedEvent("bob", easmockserver.Event{UID: "u1", Subject: "Planning with the team", Location: "Room 4", Start: start, End: start.Add(time.Hour)})
	srv.SeedContact("bob", easmockserver.Contact{FirstName: "Jane", LastName: "Doe", Email1: "jane@example.com", Company: "Acme"})
	cid := srv.SeedContact("bob", easmockserver.Contact{FirstName: "Bob", LastName: "Baker", Email1: "bob@example.org"})

	list, bad := call(t, cs, "list_events", nil)
	if bad || !strings.Contains(list, "Planning with the team") || !strings.Contains(list, eid) {
		t.Errorf("list_events: %v %s", bad, list)
	}
	if out, bad := call(t, cs, "get_event", map[string]any{"id": eid}); bad || !strings.Contains(out, "Room 4") {
		t.Errorf("get_event: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "list_contacts", nil); bad || !strings.Contains(out, "jane@example.com") || !strings.Contains(out, "bob@example.org") {
		t.Errorf("list_contacts: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "list_contacts", map[string]any{"search": "acme"}); bad || !strings.Contains(out, "Jane") || strings.Contains(out, "Baker") {
		t.Errorf("list_contacts with a search: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "get_contact", map[string]any{"id": cid}); bad || !strings.Contains(out, "bob@example.org") {
		t.Errorf("get_contact: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "get_event", map[string]any{"id": "99:99999"}); !bad {
		t.Errorf("get_event of an id that is not there: %s", out)
	}
}

func TestEventsCanBeCreatedAndDeleted(t *testing.T) {
	cs, _ := sessionWith(t, serverOptions{Write: true})
	start := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339)
	out, bad := call(t, cs, "create_event", map[string]any{"subject": "Made by the AI", "start": start, "duration": "45m", "location": "Online"})
	if bad || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("create_event: %v %s", bad, out)
	}
	if list, bad := call(t, cs, "list_events", nil); bad || !strings.Contains(list, "Made by the AI") {
		t.Fatalf("the new event is not in the list: %v %s", bad, list)
	}
	id := between(out, `"id":"`, `"`)
	if out, bad := call(t, cs, "delete_event", map[string]any{"id": id}); bad || !strings.Contains(out, `"delete"`) {
		t.Errorf("delete_event: %v %s", bad, out)
	}
	if list, _ := call(t, cs, "list_events", nil); strings.Contains(list, "Made by the AI") {
		t.Errorf("the deleted event is still in the list: %s", list)
	}
}

func TestMeetingsFollowTheSwitchForSendingAndTheListOfDomains(t *testing.T) {
	start := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339)
	meeting := func(email string) map[string]any {
		return map[string]any{"subject": "Meeting", "start": start, "attendees": []map[string]any{{"email": email}}}
	}

	// without -send a meeting cannot be made: the invitation is mail
	cs, _ := sessionWith(t, serverOptions{Write: true})
	if out, bad := call(t, cs, "create_event", meeting("carol@example.com")); !bad || !strings.Contains(out, "sending is not allowed") {
		t.Errorf("a meeting without -send: %v %s", bad, out)
	}

	// with -send the attendees are recipients, and -send-domains limits them
	cs, srv := sessionWith(t, serverOptions{Write: true, Send: true, SendDomains: []string{"example.com"}})
	if out, bad := call(t, cs, "create_event", meeting("mallory@evil.org")); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("an attendee in a domain that is not on the list: %v %s", bad, out)
	}
	if out, bad := call(t, cs, "create_event", meeting("carol@example.com")); bad || !strings.Contains(out, `"ok":true`) {
		t.Errorf("an attendee in a domain on the list: %v %s", bad, out)
	}

	// deleting a meeting of ours with an attendee outside the list is refused, and nothing is deleted
	s := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Minute)
	id := srv.SeedEvent("bob", easmockserver.Event{UID: "u-ext", Subject: "External meeting", Start: s, End: s.Add(time.Hour), Meeting: 1, Organizer: "bob@example.com",
		Attendees: []easmockserver.Attendee{{Email: "mallory@evil.org"}}})
	if out, bad := call(t, cs, "delete_event", map[string]any{"id": id}); !bad || !strings.Contains(out, "not allowed") {
		t.Errorf("deleting a meeting with an attendee outside the list: %v %s", bad, out)
	}
	if list, _ := call(t, cs, "list_events", nil); !strings.Contains(list, "External meeting") {
		t.Errorf("the meeting was deleted although the cancellation may not go: %s", list)
	}
}

// between is the text between two markers, for a value in JSON.
func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return ""
}
