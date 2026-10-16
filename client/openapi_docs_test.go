package client

import "testing"

// A text in the tables of openapi_docs.go for a type or a field that does not exist any more is a leftover that hides a rename: the new name would
// have no text, and the old one would still pass.
func TestTheTablesOfTextsHaveNothingThatTheDocumentLacks(t *testing.T) {
	schemas := OpenAPI()["components"].(map[string]any)["schemas"].(map[string]any)
	for name := range typeDocs {
		if _, ok := schemas[name]; !ok {
			t.Errorf("typeDocs has %s, which is not a schema of the document", name)
		}
	}
	have := map[string]bool{}
	for name, sc := range schemas {
		props, _ := sc.(map[string]any)["properties"].(map[string]any)
		for field := range props {
			have[name+"."+field] = true
		}
	}
	for key := range fieldDocs {
		if !have[key] {
			t.Errorf("fieldDocs has %s, which is not a field of the document", key)
		}
	}
}
