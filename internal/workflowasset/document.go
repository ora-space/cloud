package workflowasset

import (
	"encoding/json"
	"strings"
)

// ParsedDocument is what import needs before touching the database: the declared name and the
// verified JSON object. Graph is the whole document, as the desktop importer stores it.
type ParsedDocument struct {
	Name  string
	Graph map[string]any
}

// Parse reads the two fields import owns — the workflow name — and proves the document is a JSON
// object. Graph executability is judged by Cloud's publish gate, which owns that rule; it runs
// before any row is written. The document text is stored verbatim, so parse never rewrites it.
func Parse(content []byte) (ParsedDocument, error) {
	var raw any
	if err := json.Unmarshal(content, &raw); err != nil {
		return ParsedDocument{}, &DocumentError{Code: DocumentInvalidJSON, Reason: "malformed_json"}
	}
	graph, ok := raw.(map[string]any)
	if !ok {
		return ParsedDocument{}, &DocumentError{Code: DocumentInvalidJSON, Reason: "not_an_object"}
	}
	name, _ := graph["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return ParsedDocument{}, &DocumentError{Code: DocumentWithoutName, Reason: "missing_name"}
	}
	if len(name) > 200 {
		return ParsedDocument{}, &DocumentError{Code: DocumentWithoutName, Reason: "name_too_long"}
	}
	return ParsedDocument{Name: name, Graph: graph}, nil
}
