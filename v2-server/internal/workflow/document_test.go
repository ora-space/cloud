package workflow_test

import (
	"embed"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

//go:embed testdata/*.json
var fixtures embed.FS

type obj = map[string]any

func fixture(t testing.TB, name string) obj {
	t.Helper()
	data, err := fixtures.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var graph obj
	if err := json.Unmarshal(data, &graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

func encode(t testing.TB, graph obj) string {
	t.Helper()
	data, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func dataOf(graph obj, id string) obj {
	for _, value := range graph["nodes"].([]any) {
		n := value.(obj)
		if n["id"] == id {
			return n["data"].(obj)
		}
	}
	panic("missing fixture node " + id)
}

func addNode(graph obj, id, kind string, data obj) {
	if data == nil {
		data = obj{}
	}
	data["kind"] = kind
	graph["nodes"] = append(graph["nodes"].([]any), obj{"id": id, "data": data})
}

func connect(graph obj, from, to string) {
	graph["edges"] = append(graph["edges"].([]any), obj{"source": from, "target": to})
}

func flat() obj {
	return obj{"nodes": []any{obj{"id": "start", "data": obj{"kind": "start"}}}, "edges": []any{}}
}

func requireValid(t *testing.T, graph obj) {
	t.Helper()
	if err := workflow.ValidateWorkflow(encode(t, graph)); err != nil {
		t.Fatal(err)
	}
}

func requireInvalid(t *testing.T, graph obj, code string) {
	t.Helper()
	err := workflow.ValidateWorkflow(encode(t, graph))
	var domain *workflow.ValidationError
	if !errors.As(err, &domain) || (code != "" && domain.Code != code) || domain.Reason == "" {
		t.Fatalf("expected domain failure %q, got %v", code, err)
	}
}

// Fixtures are adapted from desktop execution_document/tests.rs and
// loop_graph/tests.rs at df694c8836fc70b1c789cc7e8e523d4d5b1b6c66.
func TestDesktopExecutionProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		unused []string
	}{
		{"spare-chain", []string{"other", "spare"}}, {"iteration", []string{"spare"}}, {"loop", []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := encode(t, fixture(t, test.name))
			original := strings.Clone(source)
			got, err := workflow.AnalyzeWorkflow(source)
			want := workflow.Analysis{UnusedNodeIDs: test.unused, UnrecognizedNodes: []workflow.UnrecognizedNode{}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("analysis = %#v, %v; want %#v", got, err, want)
			}
			if err := workflow.ValidateWorkflow(source); err != nil {
				t.Fatal(err)
			}
			if source != original {
				t.Fatal("authoring document changed")
			}
		})
	}
}

func TestReconnectValidatesPreviouslySpareConfiguration(t *testing.T) {
	g := fixture(t, "iteration")
	connect(g, "iter", "spare")
	analysis, err := workflow.AnalyzeWorkflow(encode(t, g))
	if err != nil || len(analysis.UnusedNodeIDs) != 0 {
		t.Fatalf("analyze must ignore configuration: %#v %v", analysis, err)
	}
	requireInvalid(t, g, "invalid_node")
}

func TestUnknownKindsAreNotSpareNodes(t *testing.T) {
	g := flat()
	addNode(g, "z", "agent", nil)
	addNode(g, "weird", "hologram", nil)
	addNode(g, "a", "prompt", nil)
	want := workflow.Analysis{UnusedNodeIDs: []string{"a", "z"}, UnrecognizedNodes: []workflow.UnrecognizedNode{{NodeID: "weird", Kind: "hologram"}}}
	got, err := workflow.AnalyzeWorkflow(encode(t, g))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v %v", got, err)
	}
	requireValid(t, g)
	connect(g, "start", "weird")
	requireInvalid(t, g, "unknown_node_type")
}

func TestDormantLoopDoesNotRequireConfiguration(t *testing.T) {
	g := obj{"schemaVersion": 2, "nodes": []any{
		obj{"id": "start", "data": obj{"kind": "start"}}, obj{"id": "loop", "data": obj{"kind": "loop"}},
		obj{"id": "entry", "data": obj{"kind": "start", "containerId": "loop"}}, obj{"id": "bad", "data": obj{"kind": "agent", "containerId": "loop", "agentConfig": false}},
	}, "edges": []any{obj{"source": "entry", "target": "bad"}}}
	requireValid(t, g)
	connect(g, "start", "loop")
	requireInvalid(t, g, "invalid_node")
}

func TestStructureCannotBeHiddenInSpareNodes(t *testing.T) {
	tests := []struct{ name, source string }{
		{"non json", `broken`}, {"null", `null`}, {"array", `[]`}, {"extra value", `{"nodes":[],"edges":[]} {}`},
		{"unpaired surrogate", `{"nodes":[],"edges":[],"title":"\ud800"}`},
		{"invalid UTF-8", "{\"nodes\":[],\"edges\":[],\"title\":\"\xff\"}"},
		{"overflow number", `{"nodes":[],"edges":[],"metadata":[1e999]}`},
		{"missing nodes", `{"edges":[]}`}, {"null nodes", `{"nodes":null,"edges":[]}`}, {"missing edges", `{"nodes":[]}`},
		{"empty id", `{"nodes":[{"id":" ","data":{"kind":"agent"}}],"edges":[]}`},
		{"missing kind", `{"nodes":[{"id":"a","data":{}}],"edges":[]}`},
		{"duplicate id", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"s","data":{"kind":"agent"}}],"edges":[]}`},
		{"two starts", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"s2","data":{"kind":"start"}}],"edges":[]}`},
		{"dangling", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"spare","data":{"kind":"agent"}}],"edges":[{"source":"spare","target":"missing"}]}`},
		{"empty endpoint", `{"nodes":[],"edges":[{"source":"","target":""}]}`},
		{"null container", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"a","data":{"kind":"agent","containerId":null}}],"edges":[]}`},
		{"unknown parent", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"a","parentId":"missing","data":{"kind":"agent"}}],"edges":[]}`},
		{"loop version", `{"nodes":[{"id":"s","data":{"kind":"start"}},{"id":"l","data":{"kind":"loop"}}],"edges":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := workflow.AnalyzeWorkflow(test.source); err == nil {
				t.Fatal("Analyze accepted malformed structure")
			}
			if err := workflow.ValidateWorkflow(test.source); err == nil {
				t.Fatal("Validate accepted malformed structure")
			}
		})
	}
}

func TestPublicationDoesNotPretendToStart(t *testing.T) {
	for _, source := range []string{
		`{"nodes":[],"edges":[]}`,
		`{"nodes":[{"id":"a","data":{"kind":"agent"}}],"edges":[]}`,
		`{"nodes":[{"id":"s","data":{"kind":"start","inputVariables":[{"name":"required","valueType":"string","required":true}]}}],"edges":[]}`,
		`{"nodes":[{"id":"p","data":{"kind":"prompt"}}],"edges":[]}`,
		`{"nodes":[{"id":"o","data":{"kind":"output"}},{"id":"a","data":{"kind":"agent"}}],"edges":[{"source":"o","target":"a"}]}`,
	} {
		if err := workflow.ValidateWorkflow(source); err != nil {
			t.Fatalf("start-time prerequisite blocked publication: %v", err)
		}
	}
	// In an entryless document every node is checked, rather than all becoming spare.
	if err := workflow.ValidateWorkflow(`{"nodes":[{"id":"a","data":{"kind":"agent","agentConfig":false}}],"edges":[]}`); err == nil {
		t.Fatal("entryless configuration escaped validation")
	}
}

func TestReferencesToUnusedProducers(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		data       obj
	}{
		{"prompt", "agent", obj{"agentConfig": obj{"prompt": "{{#spare.text#}}"}}},
		{"comparison value", "condition", obj{"cases": []any{obj{"id": "ok", "conditions": []any{obj{"variableSelector": []string{"start", "input"}, "operator": "equals", "value": "{{#spare.text#}}"}}}}}},
		{"selector", "condition", obj{"cases": []any{obj{"id": "ok", "conditions": []any{obj{"variableSelector": []string{"spare", "output"}, "operator": "exists"}}}}}},
		{"output", "output", obj{"outputs": []any{obj{"name": "result", "variableSelector": []string{"spare", "output"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := flat()
			addNode(g, "consumer", test.kind, test.data)
			addNode(g, "spare", "agent", nil)
			connect(g, "start", "consumer")
			requireInvalid(t, g, "unused_reference")
		})
	}
	// Labels and JSON Schema descriptions are not executable templates.
	g := flat()
	addNode(g, "spare", "agent", nil)
	dataOf(g, "start")["title"] = "{{#spare.output#}}"
	requireValid(t, g)
}

func FuzzDocumentValidation(f *testing.F) {
	for _, name := range []string{"iteration", "loop", "spare-chain"} {
		f.Add(encode(f, fixture(f, name)))
	}
	for _, source := range []string{`null`, `{}`, `{"nodes":[],"edges":[]}`, `{"nodes":[null],"edges":[]}`} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		analysis, err := workflow.AnalyzeWorkflow(source)
		validated := workflow.ValidateWorkflow(source)
		if validated == nil && err != nil {
			t.Fatal("validated a structurally invalid graph")
		}
		if err == nil && (analysis.UnusedNodeIDs == nil || analysis.UnrecognizedNodes == nil) {
			t.Fatal("successful analysis must expose arrays")
		}
		again, againErr := workflow.AnalyzeWorkflow(source)
		if !reflect.DeepEqual(analysis, again) || (err == nil) != (againErr == nil) {
			t.Fatal("analysis is not deterministic")
		}
	})
}
