package workflowasset

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReadsNameAndKeepsTheWholeDocument(t *testing.T) {
	content := []byte(`{"name":"  Release  ","version":1,"nodes":[{"id":"start"}],"description":null}`)
	parsed, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse returned %v", err)
	}
	if parsed.Name != "Release" {
		t.Fatalf("name = %q, want the trimmed declared name", parsed.Name)
	}
	if len(parsed.Graph) != 4 || parsed.Graph["version"] != float64(1) {
		t.Fatalf("graph = %#v, want the whole document", parsed.Graph)
	}
}

func TestParseRejectsBoundedDocumentFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		code    string
		reason  string
	}{
		{name: "malformed json", content: `{"name":`, code: DocumentInvalidJSON, reason: "malformed_json"},
		{name: "top level array", content: `[]`, code: DocumentInvalidJSON, reason: "not_an_object"},
		{name: "top level string", content: `"workflow"`, code: DocumentInvalidJSON, reason: "not_an_object"},
		{name: "missing name", content: `{"nodes":[]}`, code: DocumentWithoutName, reason: "missing_name"},
		{name: "whitespace name", content: `{"name":"   "}`, code: DocumentWithoutName, reason: "missing_name"},
		{name: "non string name", content: `{"name":7}`, code: DocumentWithoutName, reason: "missing_name"},
		{name: "name too long", content: `{"name":"` + strings.Repeat("n", 201) + `"}`, code: DocumentWithoutName, reason: "name_too_long"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.content))
			var documentError *DocumentError
			if !errors.As(err, &documentError) {
				t.Fatalf("Parse error = %v, want a DocumentError", err)
			}
			if documentError.Code != test.code || documentError.Reason != test.reason {
				t.Fatalf("document error = %+v, want %s/%s", documentError, test.code, test.reason)
			}
		})
	}
}

func TestParseAcceptsTheNameLengthBound(t *testing.T) {
	parsed, err := Parse([]byte(`{"name":"` + strings.Repeat("n", 200) + `"}`))
	if err != nil || len(parsed.Name) != 200 {
		t.Fatalf("Parse = %q, %v; want the 200-character name accepted", parsed.Name, err)
	}
}
