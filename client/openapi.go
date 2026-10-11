package client

import (
	"reflect"
	"sort"
	"strings"

	"github.com/nashcom/eas-tools/probe"
)

// RouteInfo is one request of the REST API, as the table of routes has it.
type RouteInfo struct {
	ID      string
	Method  string
	Path    string
	HasBody bool
}

// Routes lists the requests of the REST API.
func Routes() []RouteInfo {
	var out []RouteInfo
	for _, r := range routeTable() {
		out = append(out, RouteInfo{ID: r.ID, Method: r.Method, Path: r.Path, HasBody: r.Body != nil})
	}
	return out
}

var pathParamDocs = map[string]string{
	"account": "The name of the account (GET /accounts lists them).",
	"folder":  "A folder id, or inbox, sent, drafts, deleted, or the name of the folder (GET /accounts/{account}/folders lists them).",
	"id":      "The id of the mail, event or contact, as the lists show it. URL-encode it: 4:10207 is 4%3A10207.",
}

// OpenAPI describes the REST API as an OpenAPI 3 document. It is made from the table of the routes and from the Go types of the
// requests and answers, so it follows the code.
func OpenAPI() map[string]any {
	defs := map[string]any{}
	paths := map[string]any{}
	for _, r := range routeTable() {
		op := map[string]any{
			"operationId": r.ID,
			"summary":     r.Summary,
			"tags":        []string{r.Tag},
		}
		desc := r.Desc
		if r.Needs != "" {
			op["x-eas-switch"] = r.Needs
			if desc != "" {
				desc += " "
			}
			desc += "Needs the switch: " + r.Needs + " (-write, -send; \"write\" and \"send\" in the configuration)."
		}
		if desc != "" {
			op["description"] = desc
		}

		var params []any
		for _, seg := range strings.Split(r.Path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				name := seg[1 : len(seg)-1]
				params = append(params, map[string]any{"name": name, "in": "path", "required": true, "description": pathParamDocs[name], "schema": map[string]any{"type": "string"}})
			}
		}
		for _, q := range r.Query {
			s := map[string]any{"type": q.Type}
			if len(q.Enum) > 0 {
				s["enum"] = q.Enum
			}
			params = append(params, map[string]any{"name": q.Name, "in": "query", "description": q.Desc, "schema": s})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if r.Body != nil {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schemaOf(reflect.TypeOf(r.Body), defs)}}}
		}

		ok := map[string]any{"description": "The answer."}
		switch {
		case r.Result != nil:
			ok["content"] = map[string]any{"application/json": map[string]any{"schema": schemaOf(reflect.TypeOf(r.Result), defs)}}
		case r.Raw == "application/json":
			ok["content"] = map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}
		case r.Raw != "":
			ok["description"] = "The file, not JSON."
			ok["content"] = map[string]any{r.Raw: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}
		}
		errResp := func(d string) map[string]any {
			return map[string]any{"description": d, "content": map[string]any{"application/json": map[string]any{"schema": schemaOf(reflect.TypeOf(ErrorBody{}), defs)}}}
		}
		responses := map[string]any{"200": ok, "default": errResp("An error: the code names it, the HTTP status follows it.")}
		if !r.Public {
			responses["401"] = errResp("The bearer token is missing or wrong.")
		}
		if r.Needs != "" {
			responses["403"] = errResp("The switch that the request needs is not on (write_disabled, send_disabled).")
		}
		op["responses"] = responses
		if r.Public {
			op["security"] = []any{}
		}

		item, _ := paths[r.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[r.Path] = item
		}
		item[strings.ToLower(r.Method)] = op
	}

	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "eas-client REST API",
			"version": probe.Version,
			"description": "Mail, calendar and contacts of an ActiveSync account (HCL Traveler). The answers are the JSON that the command line prints for the " +
				"same operation. Reading is always allowed; changing the mailbox needs the switch write and sending mail needs the switch send.",
		},
		"tags": []any{
			map[string]any{"name": "service", "description": "Liveness and this description."},
			map[string]any{"name": "account", "description": "The accounts and what the server is."},
			map[string]any{"name": "mail", "description": "Folders and mail."},
			map[string]any{"name": "calendar", "description": "Calendar events."},
			map[string]any{"name": "contacts", "description": "The address book."},
		},
		"paths": paths,
		"components": map[string]any{
			"schemas":         defs,
			"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "description": "The token of the API: Authorization: Bearer <token>. Never in the URL."}},
		},
		"security": []any{map[string]any{"bearerAuth": []any{}}},
	}
}

// schemaOf makes the JSON schema of a Go type. Structs become named schemas in defs and are referenced.
func schemaOf(t reflect.Type, defs map[string]any) map[string]any {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), defs)}
	case reflect.Struct:
		name := t.Name()
		if _, done := defs[name]; !done {
			defs[name] = map[string]any{} // a placeholder: a type can refer to itself
			props := map[string]any{}
			var required []string
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				tag := f.Tag.Get("json")
				if tag == "-" {
					continue
				}
				jsonName, opts, _ := strings.Cut(tag, ",")
				if jsonName == "" {
					jsonName = f.Name
				}
				props[jsonName] = schemaOf(f.Type, defs)
				if f.Type.Kind() != reflect.Ptr && !strings.Contains(opts, "omitempty") {
					required = append(required, jsonName)
				}
			}
			schema := map[string]any{"type": "object", "properties": props}
			if len(required) > 0 {
				sort.Strings(required)
				schema["required"] = required
			}
			defs[name] = schema
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{} // any value
}
