package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Adapted from desktop structured_output.rs tests.

func TestExtractJSONObject(t *testing.T) {
	want := map[string]any{"approved": true, "score": json.Number("92")}
	accepted := map[string]string{
		"bare":                 `{ "approved": true, "score": 92 }`,
		"markdown fence":       "审查结果如下：\n\n```json\n{\"approved\": true, \"score\": 92}\n```\n",
		"inline in prose":      "审查完成，结果为 {\"approved\": true, \"score\": 92}，请查收。",
		"trailing commentary":  "{\"approved\": true, \"score\": 92}\n以上是结果。",
		"non-json fence first": "```rust\nlet x = 1;\n```\n```\n{\"approved\": true, \"score\": 92}\n```",
		"brace inside string":  `结果 {"approved": true, "score": 92, "note": "a } b"}`,
	}
	for name, text := range accepted {
		got, err := extractJSONObject(text)
		if name == "brace inside string" {
			want := map[string]any{"approved": true, "score": json.Number("92"), "note": "a } b"}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %v %v", name, got, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	for _, text := range []string{"[1, 2, 3]", "no json here", "{broken"} {
		if _, err := extractJSONObject(text); err == nil || !strings.Contains(err.Error(), "not a JSON object") {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestValidateStructuredOutput(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approved": map[string]any{"type": "boolean"},
			"score":    map[string]any{"type": "integer"},
			"files":    map[string]any{"type": "array[file]"},
			"meta":     map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string"}}},
			"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"required":             []any{"approved"},
		"additionalProperties": false,
	}
	ok := `{"approved": true, "score": 5, "files": [{"kind":"workspace_file","path":"docs/a.md"}], "meta": {"owner": "me"}, "tags": ["x"]}`
	if _, err := ParseStructuredOutput(ok, schema); err != nil {
		t.Fatalf("conforming object: %v", err)
	}
	rejected := map[string]string{
		`{"score": 1}`:                             "$ is missing required property approved",
		`{"approved": true, "score": 5.5}`:         "$.score has type number, expected integer",
		`{"approved": true, "meta": {"owner": 1}}`: "$.meta.owner has type integer, expected string",
		`{"approved": true, "extra": 1}`:           "$ has unexpected property extra",
		`{"approved": true, "tags": ["x", 2]}`:     "$.tags[1] has type integer, expected string",
		`{"approved": true, "files": [{"kind":"workspace_file","path":"/etc/passwd"}]}`: "$.files has type array, expected array[file]",
	}
	for text, want := range rejected {
		if _, err := ParseStructuredOutput(text, schema); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", text, err, want)
		}
	}
}

func TestStructuredExampleSatisfiesItsSchema(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"approved": map[string]any{"type": "boolean"}, "score": map[string]any{"type": "integer"},
		"ratio": map[string]any{"type": "number"}, "file": map[string]any{"type": "file"},
		"names": map[string]any{"type": "array[string]"}, "nested": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
	}}
	raw, err := json.Marshal(StructuredExample(schema))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStructuredOutput(string(raw), schema); err != nil {
		t.Fatalf("example %s: %v", raw, err)
	}
}
