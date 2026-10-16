package client

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The routes of the REST API are in one table. The handler finds the request in it and the OpenAPI description (see OpenAPI) is made
// from it, so the two cannot differ.

// vars are the parts of the path that a route names: account, folder and id.
type vars map[string]string

// queryParam describes a parameter of the query of a request.
type queryParam struct {
	Name, Desc string
	Type       string // string, integer or boolean
	Enum       []string
}

// route is one request of the API.
type route struct {
	ID      string // operationId
	Method  string
	Path    string // with {account}, {folder} and {id}
	Tag     string
	Summary string
	Desc    string
	Query   []queryParam
	Body    any // a value of the type of the JSON body of the request; nil for none
	Result  any // a value of the type of the JSON answer; nil for a raw one
	Raw     string
	// Needs names the switch a request needs: write, send, or "write and send" for a meeting.
	Needs  string
	Public bool // needs no token
	Handle func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars)
}

// MailPatch is the body of a request that changes a mail: it marks it read or unread, or moves it. One of the two.
type MailPatch struct {
	Read   *bool  `json:"read,omitempty"`
	MoveTo string `json:"move_to,omitempty"`
}

// HealthResult is the answer of the liveness check.
type HealthResult struct {
	Status string `json:"status"`
}

func limitParam(q url.Values) (int, error) {
	v := q.Get("limit")
	switch {
	case v == "":
		return 0, nil
	case v == "all":
		return -1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, newError(http.StatusBadRequest, "bad_request", "limit is a number of 1 or more, or all")
	}
	return n, nil
}

var (
	qLimitMail = queryParam{"limit", "number of mails, newest first, or all (default 20)", "string", nil}
	qLimit50   = queryParam{"limit", "number of entries, or all (default 50)", "string", nil}
	qSince     = queryParam{"since", "date window asked of the server: 1d, 3d, 1w, 2w (default), 1m, 3m or 6m", "string", []string{"1d", "3d", "1w", "2w", "1m", "3m", "6m"}}
)

// routeTable is the list of the requests of the API, in the order in which they are tried.
func routeTable() []route {
	var rs []route
	add := func(r route) { rs = append(rs, r) }
	add(route{ID: "getHealth", Method: "GET", Path: "/health", Tag: "service", Summary: "Liveness check", Desc: "Answers ok while the process runs. It needs no token, and it does not connect to the server of the account.", Result: &HealthResult{}, Public: true,
		Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			writeJSON(w, http.StatusOK, HealthResult{Status: "ok"})
		}})
	add(route{ID: "getOpenAPI", Method: "GET", Path: "/openapi.json", Tag: "service", Summary: "This description of the API", Desc: "This OpenAPI 3 document: every request with its parameters, the JSON of the requests and of the answers, and the errors. It needs the token, unless the page /docs is switched on (-docs), which makes it open.", Raw: "application/json",
		Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			writeJSON(w, http.StatusOK, OpenAPI())
		}})

	add(route{ID: "listAccounts", Method: "GET", Path: "/accounts", Tag: "account", Summary: "The accounts", Desc: "The accounts of the configuration, never their passwords, and whether writing and sending are allowed.",
		Result: &AccountsResult{}, Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			reply(w, http.StatusOK, h.m.Accounts(), nil)
		}})
	add(route{ID: "getAccountInfo", Method: "GET", Path: "/accounts/{account}", Tag: "account", Summary: "Server and device", Desc: "The server product, the protocol version, the commands it offers, the device and the time zone.",
		Result: &Info{}, Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			res, err := h.m.Info(r.Context(), v["account"])
			reply(w, http.StatusOK, res, err)
		}})
	add(route{ID: "listFolders", Method: "GET", Path: "/accounts/{account}/folders", Tag: "mail", Summary: "The folders", Desc: "Every folder of the mailbox (mail, calendar, contacts and others) with its id, name, parent and type. The ids are what the other requests take as folder.", Result: &FoldersResult{},
		Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			res, err := h.m.Folders(r.Context(), v["account"])
			reply(w, http.StatusOK, res, err)
		}})

	// mail
	add(route{ID: "listMessages", Method: "GET", Path: "/accounts/{account}/folders/{folder}/messages", Tag: "mail", Summary: "The mails of a folder, newest first",
		Desc:  "EAS has no listing: the folder is read from the start, which gives the mails of a date window (since). A mail of an older date is not in the list.",
		Query: []queryParam{qLimitMail, {"preview", "bytes of the body of every mail to show (default 0: none)", "integer", nil}, qSince}, Result: &ListResult{},
		Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			p, err := listParams(r.URL.Query())
			if err != nil {
				writeError(w, err)
				return
			}
			res, err := h.m.List(r.Context(), v["account"], v["folder"], p)
			reply(w, http.StatusOK, res, err)
		}})
	add(route{ID: "sendMessage", Method: "POST", Path: "/accounts/{account}/messages", Tag: "mail", Summary: "Send a mail", Body: &SendRequest{}, Result: &SendResult{}, Needs: "send",
		Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
			var req SendRequest
			if err := readBody(w, r, &req); err != nil {
				writeError(w, err)
				return
			}
			res, err := h.m.Send(r.Context(), v["account"], req)
			reply(w, http.StatusOK, res, err)
		}})
	for _, base := range []string{"/accounts/{account}/folders/{folder}/messages/{id}", "/accounts/{account}/messages/{id}"} {
		withFolder := strings.Contains(base, "{folder}")
		note := " The folder can be left out of the path when the id says it (4:10207 is in the folder 4)."
		if withFolder {
			note = ""
		}
		suffix := "InFolder"
		if !withFolder {
			suffix = ""
		}
		add(route{ID: "getMessage" + suffix, Method: "GET", Path: base, Tag: "mail", Summary: "One mail with its whole body", Desc: "The whole mail: sender, recipients, subject, date, flags and the full text of the body, as plain text. Attachments are not in the JSON; the .eml request returns the whole message with them. The id is the one that the list shows." + note, Result: &GetResult{},
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				res, err := h.m.Get(r.Context(), v["account"], v["folder"], v["id"])
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "patchMessage" + suffix, Method: "PATCH", Path: base, Tag: "mail", Summary: "Mark a mail read or unread, or move it",
			Desc: `The body is {"read":true} or {"read":false} or {"move_to":"archive"}: one of them.` + note, Body: &MailPatch{}, Result: &ChangeResult{}, Needs: "write",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				var req MailPatch
				if err := readBody(w, r, &req); err != nil {
					writeError(w, err)
					return
				}
				switch {
				case req.Read != nil && req.MoveTo == "":
					res, err := h.m.SetRead(r.Context(), v["account"], v["folder"], v["id"], *req.Read)
					reply(w, http.StatusOK, res, err)
				case req.Read == nil && req.MoveTo != "":
					res, err := h.m.Move(r.Context(), v["account"], v["folder"], v["id"], req.MoveTo)
					reply(w, http.StatusOK, res, err)
				default:
					writeError(w, newError(http.StatusBadRequest, "bad_request", `the body has to be {"read":true|false} or {"move_to":"FOLDER"}, one of them`))
				}
			}})
		add(route{ID: "deleteMessage" + suffix, Method: "DELETE", Path: base, Tag: "mail", Summary: "Delete a mail",
			Desc: "The mail goes to Deleted Items; a mail that is there already is gone for good." + note, Result: &ChangeResult{}, Needs: "write",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				res, err := h.m.Delete(r.Context(), v["account"], v["folder"], v["id"])
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "getMessageEml" + suffix, Method: "GET", Path: base + "/eml", Tag: "mail", Summary: "The whole message as a mail file (.eml)",
			Desc: "The message as the server builds it (MIME): all headers, the text and the parts. Not JSON." + note, Raw: "message/rfc822",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				raw, err := h.m.EML(r.Context(), v["account"], v["folder"], v["id"])
				writeRaw(w, raw, err)
			}})
		add(route{ID: "replyMessage" + suffix, Method: "POST", Path: base + "/reply", Tag: "mail", Summary: "Reply to a mail",
			Desc: "The server adds the original to the text (SmartReply)." + note, Body: &ReplyRequest{}, Result: &SendResult{}, Needs: "send",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				var req ReplyRequest
				if err := readBody(w, r, &req); err != nil {
					writeError(w, err)
					return
				}
				res, err := h.m.Reply(r.Context(), v["account"], v["folder"], v["id"], req)
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "forwardMessage" + suffix, Method: "POST", Path: base + "/forward", Tag: "mail", Summary: "Forward a mail",
			Desc: "The server adds the original to the text (SmartForward)." + note, Body: &ForwardRequest{}, Result: &SendResult{}, Needs: "send",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				var req ForwardRequest
				if err := readBody(w, r, &req); err != nil {
					writeError(w, err)
					return
				}
				res, err := h.m.Forward(r.Context(), v["account"], v["folder"], v["id"], req)
				reply(w, http.StatusOK, res, err)
			}})
	}

	// calendar: the calendar of the mailbox, or another calendar folder
	for _, base := range []string{"/accounts/{account}/folders/{folder}/events", "/accounts/{account}/calendar/events"} {
		suffix := ""
		note := ""
		if strings.Contains(base, "{folder}") {
			suffix, note = "InFolder", " For another calendar folder."
		} else {
			note = " The calendar of the mailbox; the folder can be left out of the path of one event when the id says it."
		}
		add(route{ID: "listEvents" + suffix, Method: "GET", Path: base, Tag: "calendar", Summary: "The events of a calendar, by start time",
			Desc:  "From now on by default. A repeating event is not expanded: it comes once, with its rule." + note,
			Query: []queryParam{{"from", "start of the list: now (default), a date 2026-10-10 or a time 2026-10-10T09:00:00Z; times without a zone are in the time zone of the account", "string", nil}, {"to", "end of the list (default: none)", "string", nil}, qSince, qLimit50}, Result: &EventsResult{},
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				q := r.URL.Query()
				lim, err := limitParam(q)
				if err != nil {
					writeError(w, err)
					return
				}
				res, err := h.m.Events(r.Context(), v["account"], v["folder"], EventParams{From: q.Get("from"), To: q.Get("to"), Since: q.Get("since"), Limit: lim})
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "createEvent" + suffix, Method: "POST", Path: base, Tag: "calendar", Summary: "Create an event",
			Desc: "With attendees it is a meeting: the client mails the invitation to them (HCL Traveler invites nobody itself). That needs the switch for sending as well." + note,
			Body: &EventRequest{}, Result: &ChangeResult{}, Needs: "write, and send for a meeting",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				var req EventRequest
				if err := readBody(w, r, &req); err != nil {
					writeError(w, err)
					return
				}
				res, err := h.m.CreateEvent(r.Context(), v["account"], v["folder"], req)
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "getEvent" + suffix, Method: "GET", Path: base + "/{id}", Tag: "calendar", Summary: "One event with all details", Desc: "The subject, place, start and end, the busy state and sensitivity, the reminder, the attendees and their answers, the repeat rule and its exceptions, and the description. The id is the one that the list shows.", Result: &EventResult{},
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				res, err := h.m.Event(r.Context(), v["account"], v["folder"], v["id"])
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "deleteEvent" + suffix, Method: "DELETE", Path: base + "/{id}", Tag: "calendar", Summary: "Delete an event",
			Desc:  "A meeting that the account organizes also gets its cancellation mailed to the attendees, unless cancel_mail is false; that needs the switch for sending.",
			Query: []queryParam{{"cancel_mail", "false: do not mail the cancellation of a meeting", "boolean", nil}}, Result: &ChangeResult{}, Needs: "write, and send for a meeting",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				res, err := h.m.DeleteEventWith(r.Context(), v["account"], v["folder"], v["id"], r.URL.Query().Get("cancel_mail") != "false")
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "getEventIcs" + suffix, Method: "GET", Path: base + "/{id}/ics", Tag: "calendar", Summary: "One event as an iCalendar file (.ics)",
			Desc: "Built from the fields of the event, in the time zone of the account. Not JSON.", Raw: "text/calendar",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				raw, err := h.m.ICS(r.Context(), v["account"], v["folder"], v["id"])
				writeRaw(w, raw, err)
			}})
	}

	// contacts: the address book of the mailbox, or another one
	for _, base := range []string{"/accounts/{account}/folders/{folder}/contacts", "/accounts/{account}/contacts"} {
		suffix := ""
		note := ""
		if strings.Contains(base, "{folder}") {
			suffix, note = "InFolder", " For another address book."
		} else {
			note = " The address book of the mailbox; the folder can be left out of the path of one contact when the id says it."
		}
		add(route{ID: "listContacts" + suffix, Method: "GET", Path: base, Tag: "contacts", Summary: "The contacts, by name", Desc: "The photo is not part of the JSON: it is in the vCard." + note,
			Query:  []queryParam{{"search", "only the contacts with this text in the name, an e-mail address, the company or a phone number", "string", nil}, qLimit50},
			Result: &ContactsResult{}, Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				q := r.URL.Query()
				lim, err := limitParam(q)
				if err != nil {
					writeError(w, err)
					return
				}
				res, err := h.m.Contacts(r.Context(), v["account"], v["folder"], ContactParams{Search: q.Get("search"), Limit: lim})
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "getContact" + suffix, Method: "GET", Path: base + "/{id}", Tag: "contacts", Summary: "One contact with all details", Desc: "The names, e-mail addresses, phone numbers, postal addresses, company data and categories. The photo is not in the JSON (has_photo and photo_bytes say that there is one); the vCard request returns it. The id is the one that the list shows.", Result: &ContactResult{},
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				res, err := h.m.Contact(r.Context(), v["account"], v["folder"], v["id"])
				reply(w, http.StatusOK, res, err)
			}})
		add(route{ID: "getContactVcard" + suffix, Method: "GET", Path: base + "/{id}/vcf", Tag: "contacts", Summary: "One contact as a vCard file (.vcf)", Desc: "vCard 3.0, with the photo. Not JSON.", Raw: "text/vcard",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				raw, err := h.m.VCard(r.Context(), v["account"], v["folder"], v["id"])
				writeRaw(w, raw, err)
			}})
		add(route{ID: "exportContacts" + suffix, Method: "GET", Path: base + ".vcf", Tag: "contacts", Summary: "All contacts as one vCard file", Desc: "vCard 3.0, with the photos. Not JSON." + note,
			Query: []queryParam{{"search", "only the contacts with this text in the name, an e-mail address, the company or a phone number", "string", nil}}, Raw: "text/vcard",
			Handle: func(h *restHandler, w http.ResponseWriter, r *http.Request, v vars) {
				raw, err := h.m.Export(r.Context(), v["account"], v["folder"], r.URL.Query().Get("search"))
				writeRaw(w, raw, err)
			}})
	}
	return rs
}

// match finds the route of a request in the parts of its path: the route, the parts that it names, and, when the path is there but not
// with this method, the methods that it has.
func match(rs []route, method string, parts []string) (found *route, v vars, allowed []string) {
	for i := range rs {
		rt := &rs[i]
		pv, ok := matchPath(rt.Path, parts)
		if !ok {
			continue
		}
		if rt.Method == method {
			return rt, pv, nil
		}
		allowed = append(allowed, rt.Method)
	}
	return nil, nil, allowed
}

// matchPath matches the parts of a path with the pattern of a route.
func matchPath(pattern string, parts []string) (vars, bool) {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	if len(segs) != len(parts) {
		return nil, false
	}
	v := vars{}
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			v[s[1:len(s)-1]] = parts[i]
			continue
		}
		if s != parts[i] {
			return nil, false
		}
	}
	return v, true
}
