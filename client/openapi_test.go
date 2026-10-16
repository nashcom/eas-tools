package client_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nashcom/eas-tools/client"
)

func specOf(t *testing.T) map[string]any {
	t.Helper()
	raw, err := json.Marshal(client.OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func obj(t *testing.T, v any, path string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", path, v)
	}
	return m
}

// collectRefs lists every $ref of the document.
func collectRefs(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "$ref" {
				*out = append(*out, s)
				continue
			}
			collectRefs(e, out)
		}
	case []any:
		for _, e := range x {
			collectRefs(e, out)
		}
	}
}

func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	spec := specOf(t)
	if spec["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v", spec["openapi"])
	}
	info := obj(t, spec["info"], "info")
	if info["title"] == "" || info["version"] == "" {
		t.Errorf("info = %v", info)
	}
	paths := obj(t, spec["paths"], "paths")

	ids := map[string]bool{}
	for _, r := range client.Routes() {
		item, ok := paths[r.Path]
		if !ok {
			t.Errorf("%s %s is not in the description", r.Method, r.Path)
			continue
		}
		op, ok := obj(t, item, r.Path)[strings.ToLower(r.Method)]
		if !ok {
			t.Errorf("%s %s has no %s", r.Path, r.Method, r.Method)
			continue
		}
		o := obj(t, op, r.Path)
		id, _ := o["operationId"].(string)
		if id == "" || ids[id] {
			t.Errorf("%s %s: the operationId %q is empty or used twice", r.Method, r.Path, id)
		}
		ids[id] = true
		if s, _ := o["summary"].(string); s == "" {
			t.Errorf("%s %s has no summary", r.Method, r.Path)
		}
		// every {name} of the path is a parameter
		declared := map[string]bool{}
		if ps, ok := o["parameters"].([]any); ok {
			for _, p := range ps {
				pm := obj(t, p, "parameter")
				if pm["in"] == "path" {
					declared[pm["name"].(string)] = true
					if pm["required"] != true {
						t.Errorf("%s: the path parameter %v is not required", r.Path, pm["name"])
					}
				}
			}
		}
		for _, seg := range strings.Split(r.Path, "/") {
			if strings.HasPrefix(seg, "{") && !declared[strings.Trim(seg, "{}")] {
				t.Errorf("%s %s: the parameter %s is not declared", r.Method, r.Path, seg)
			}
		}
		if r.HasBody != (o["requestBody"] != nil) {
			t.Errorf("%s %s: request body in the table %v, in the description %v", r.Method, r.Path, r.HasBody, o["requestBody"] != nil)
		}
	}
	if len(paths) == 0 || len(ids) != len(client.Routes()) {
		t.Errorf("%d paths, %d operations, %d routes", len(paths), len(ids), len(client.Routes()))
	}

	// no path in the description that the table does not have
	have := map[string]bool{}
	for _, r := range client.Routes() {
		have[r.Path] = true
	}
	for p := range paths {
		if !have[p] {
			t.Errorf("the description has the path %s that no route has", p)
		}
	}
}

func TestOpenAPIReferencesResolveAndTypesAreThere(t *testing.T) {
	spec := specOf(t)
	schemas := obj(t, obj(t, spec["components"], "components")["schemas"], "schemas")
	var refs []string
	collectRefs(spec, &refs)
	if len(refs) == 0 {
		t.Fatal("no references")
	}
	for _, ref := range refs {
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		if _, found := schemas[name]; !ok || !found {
			t.Errorf("the reference %s does not resolve", ref)
		}
	}

	// the types of the answers are made from the Go types: fields, names of the JSON, what is required
	msg := obj(t, schemas["Message"], "Message")
	props := obj(t, msg["properties"], "Message.properties")
	for _, f := range []string{"id", "folder", "subject", "date", "read", "body_truncated", "has_attachments"} {
		if _, ok := props[f]; !ok {
			t.Errorf("Message has no property %q", f)
		}
	}
	if props["read"].(map[string]any)["type"] != "boolean" || refOf(props["date"]) != "#/components/schemas/Time" {
		t.Errorf("Message.read = %v, Message.date = %v", props["read"], props["date"])
	}
	required, _ := msg["required"].([]any)
	has := func(name string) bool {
		for _, r := range required {
			if r == name {
				return true
			}
		}
		return false
	}
	if !has("id") || !has("subject") || has("cc") || has("date") { // omitempty and pointers are not required
		t.Errorf("required = %v", required)
	}
	if _, ok := schemas["ErrorBody"]; !ok {
		t.Error("the error body is not described")
	}
	list := obj(t, obj(t, schemas["ListResult"], "ListResult")["properties"], "ListResult.properties")
	if m := obj(t, list["messages"], "messages"); m["type"] != "array" || obj(t, m["items"], "items")["$ref"] != "#/components/schemas/Message" {
		t.Errorf("ListResult.messages = %v", m)
	}
}

func TestOpenAPIRawAnswersAndSecurity(t *testing.T) {
	spec := specOf(t)
	paths := obj(t, spec["paths"], "paths")
	content := func(path, method string) map[string]any {
		op := obj(t, obj(t, paths[path], path)[method], path+" "+method)
		ok := obj(t, obj(t, op["responses"], "responses")["200"], "200")
		return obj(t, ok["content"], "content")
	}
	for path, ct := range map[string]string{
		"/accounts/{account}/messages/{id}/eml":                  "message/rfc822",
		"/accounts/{account}/calendar/events/{id}/ics":           "text/calendar",
		"/accounts/{account}/contacts/{id}/vcf":                  "text/vcard",
		"/accounts/{account}/contacts.vcf":                       "text/vcard",
		"/accounts/{account}/folders/{folder}/messages/{id}/eml": "message/rfc822",
	} {
		if _, ok := content(path, "get")[ct]; !ok {
			t.Errorf("%s has no answer of the type %s: %v", path, ct, content(path, "get"))
		}
	}
	if _, ok := content("/accounts", "get")["application/json"]; !ok {
		t.Error("/accounts has no JSON answer")
	}

	schemes := obj(t, obj(t, spec["components"], "components")["securitySchemes"], "securitySchemes")
	if b := obj(t, schemes["bearerAuth"], "bearerAuth"); b["type"] != "http" || b["scheme"] != "bearer" {
		t.Errorf("bearerAuth = %v", b)
	}
	if sec, ok := spec["security"].([]any); !ok || len(sec) != 1 {
		t.Errorf("security = %v", spec["security"])
	}
	health := obj(t, obj(t, paths["/health"], "/health")["get"], "/health get")
	if sec, ok := health["security"].([]any); !ok || len(sec) != 0 {
		t.Errorf("/health needs no token, but security = %v", health["security"])
	}
	// the switch that a request needs is said in the description
	del := obj(t, obj(t, paths["/accounts/{account}/messages/{id}"], "p")["delete"], "delete")
	if del["x-eas-switch"] != "write" {
		t.Errorf("delete needs the switch write: %v", del["x-eas-switch"])
	}
}

// Every route of the table is served, whatever the request does.
func TestEveryRouteIsServed(t *testing.T) {
	_, _, ts := api(t, true, "tok", "bob")
	replace := strings.NewReplacer("{account}", "bob", "{folder}", "inbox", "{id}", "5%3A1")
	for _, r := range client.Routes() {
		body := ""
		if r.HasBody {
			body = "{}"
		}
		req, _ := http.NewRequest(r.Method, ts.URL+replace.Replace(r.Path), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e client.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed || (e.Error != nil && e.Error.Code == "not_found") {
			t.Errorf("%s %s is in the table but the handler does not serve it: %d %+v", r.Method, r.Path, resp.StatusCode, e.Error)
		}
	}
}

// refOf is the schema that a property refers to: directly, or inside allOf when the property has a description of its own (OpenAPI 3.0 allows no
// siblings next to a reference).
func refOf(v any) string {
	m, _ := v.(map[string]any)
	if r, ok := m["$ref"].(string); ok {
		return r
	}
	if all, ok := m["allOf"].([]any); ok && len(all) == 1 {
		return refOf(all[0])
	}
	return ""
}

// The description of the API is what a person, Swagger UI, a generator of clients and an AI read: every operation says what it does, every parameter and
// every field says what it is, and the name of an operation is a verb and an object, since a generator makes the name of a tool of it.
func TestEveryOperationParameterAndFieldIsDescribed(t *testing.T) {
	spec := specOf(t)
	verbs := []string{"get", "list", "create", "delete", "send", "reply", "forward", "patch", "export"}
	for p, item := range obj(t, spec["paths"], "paths") {
		for method, op := range obj(t, item, p) {
			o := obj(t, op, p+" "+method)
			where := method + " " + p
			id, _ := o["operationId"].(string)
			okVerb := false
			for _, v := range verbs {
				if strings.HasPrefix(id, v) && len(id) > len(v) && id[len(v)] >= 'A' && id[len(v)] <= 'Z' {
					okVerb = true
				}
			}
			if !okVerb {
				t.Errorf("%s: the operationId %q is not a verb and an object (get, list, create, ...): a generator makes the name of a tool of it", where, id)
			}
			if s, _ := o["summary"].(string); s == "" {
				t.Errorf("%s has no summary", where)
			}
			if d, _ := o["description"].(string); len(d) < 20 {
				t.Errorf("%s has no description (%q)", where, d)
			}
			if o["tags"] == nil {
				t.Errorf("%s has no tag", where)
			}
			params, _ := o["parameters"].([]any)
			for _, pr := range params {
				pm := obj(t, pr, where+" parameter")
				if d, _ := pm["description"].(string); d == "" {
					t.Errorf("%s: the parameter %v has no description", where, pm["name"])
				}
			}
			if responses, ok := o["responses"].(map[string]any); ok {
				for code, r := range responses {
					if d, _ := r.(map[string]any)["description"].(string); d == "" {
						t.Errorf("%s: the response %s has no description", where, code)
					}
				}
			}
		}
	}
	schemas := obj(t, obj(t, spec["components"], "components")["schemas"], "schemas")
	for name, sc := range schemas {
		sm := obj(t, sc, name)
		if d, _ := sm["description"].(string); d == "" {
			t.Errorf("the schema %s has no description", name)
		}
		props, _ := sm["properties"].(map[string]any)
		for field, pv := range props {
			if d, _ := pv.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s.%s has no description", name, field)
			}
		}
	}
}
