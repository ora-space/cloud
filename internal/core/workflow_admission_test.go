package core

import (
	"reflect"
	"testing"
)

// inputGraph declares `variables` on a Start node, the shape the run input is judged against. It is
// decoded like every stored document, so the schema reads the same map shape it reads in production.
func inputGraph(t *testing.T, variables []any) Object {
	t.Helper()
	return decodeGraph(t, workflowGraph([]any{Object{
		"id": "start-1", "type": "workflow",
		"data": Object{"kind": "start", "inputVariables": variables},
	}}, []any{}, nil))
}

// TestRequireWorkflowRunInput covers what a run's input must answer: every required Start variable
// present and every submitted value of the type its control declares.
func TestRequireWorkflowRunInput(t *testing.T) {
	graph := inputGraph(t, []any{
		Object{"name": "topic", "valueType": "string", "required": true},
		Object{"name": "depth", "valueType": "number"},
		Object{"name": "dryRun", "valueType": "boolean"},
		Object{
			"name": "scope", "fieldType": "select", "valueType": "string",
			"options": []any{"changed", "full"},
		},
		Object{"name": "items", "valueType": "array"},
	})

	cases := []struct {
		name  string
		input Object
		want  *Fault
	}{
		{
			name:  "every declared variable answered",
			input: Object{"topic": "auth", "depth": 2, "dryRun": true, "scope": "full"},
		},
		{
			// A run input is an execution input, not a form submission: it carries the platform's own
			// material alongside the workflow's parameters, so an extra key is not an error here.
			name:  "a key the graph does not declare",
			input: Object{"topic": "auth", "workflowSnapshotId": "11111111-1111-4111-8111-111111111111"},
		},
		{
			name:  "a required variable left out",
			input: Object{"depth": 2},
			want:  &Fault{Code: "required_field_missing", Status: 400, Params: Object{"field": "topic"}},
		},
		{
			name:  "a required variable explicitly null",
			input: Object{"topic": nil},
			want:  &Fault{Code: "required_field_missing", Status: 400, Params: Object{"field": "topic"}},
		},
		{
			name:  "a string where a number is declared",
			input: Object{"topic": "auth", "depth": "two"},
			want:  &Fault{Code: "invalid_field_value", Status: 400, Params: Object{"field": "depth"}},
		},
		{
			name:  "a string where a boolean is declared",
			input: Object{"topic": "auth", "dryRun": "yes"},
			want:  &Fault{Code: "invalid_field_value", Status: 400, Params: Object{"field": "dryRun"}},
		},
		{
			name:  "a value outside a select's options",
			input: Object{"topic": "auth", "scope": "everything"},
			want:  &Fault{Code: "invalid_field_value", Status: 400, Params: Object{"field": "scope"}},
		},
		{
			// An iteration source is a structured value: a direct run carries the array itself, not
			// only its JSON text.
			name:  "an array for a structured variable",
			input: Object{"topic": "auth", "items": []any{"a", "b"}},
		},
		{
			name:  "a JSON document for a structured variable",
			input: Object{"topic": "auth", "items": Object{"a": 1}},
		},
		{
			// `input` is the reserved kickoff selector rather than a declared variable. Desktop
			// carries it as a string, and accepting another type here would make [start, input] an
			// object on Cloud and a string on desktop.
			name:  "the reserved kickoff selector as a string",
			input: Object{"topic": "auth", "input": "review the diff"},
		},
		{
			// A null override is a run that named none: it is treated as absent and falls back to
			// the authored Start instruction, exactly like a declared variable left null.
			name:  "the reserved kickoff selector explicitly null",
			input: Object{"topic": "auth", "input": nil},
		},
		{
			name:  "the reserved kickoff selector as an object",
			input: Object{"topic": "auth", "input": Object{"text": "review the diff"}},
			want:  &Fault{Code: "invalid_field_value", Status: 400, Params: Object{"field": "input"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				noFault(t, func() { requireWorkflowRunInput(graph, tc.input) })
				return
			}
			got := faultOf(t, func() { requireWorkflowRunInput(graph, tc.input) })
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fault = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRequireWorkflowRunInputKickoffShadowsInputVariable pins the desktop shadow semantics for a Start
// variable literally named `input`: the reserved kickoff selector overwrites the declared variable, so
// the declared type is never judged while the reserved key stays string-only. Without the shadow a
// graph declaring `input` as a number would reject every run — a string kickoff fails the declared
// type, and a number fails the reserved-string check — a Cloud-only dead end desktop runs fine.
func TestRequireWorkflowRunInputKickoffShadowsInputVariable(t *testing.T) {
	graph := inputGraph(t, []any{
		Object{"name": "topic", "valueType": "string", "required": true},
		Object{"name": "input", "valueType": "number"},
	})

	// A string `input` is the kickoff and is admitted even though the declared `input` variable is a
	// number: the reserved selector shadows it, so the number's type is not judged.
	noFault(t, func() { requireWorkflowRunInput(graph, Object{"topic": "auth", "input": "review the diff"}) })

	// The reserved key is still string-only: a number fails the reserved check even though a declared
	// `input` variable of that type would have been legal on its own.
	fault := faultOf(t, func() { requireWorkflowRunInput(graph, Object{"topic": "auth", "input": 7}) })
	want := &Fault{Code: "invalid_field_value", Status: 400, Params: Object{"field": "input"}}
	if !reflect.DeepEqual(fault, want) {
		t.Fatalf("fault = %+v, want %+v", fault, want)
	}
}

// TestRequireWorkflowRunInputReadsTheGraphItIsGiven covers the reason the check takes a graph rather
// than reading the live document: the schema that governs a run is the one the version it executes
// was published with. A variable the live document added after that publish must not be demanded of
// an older version's run, and a variable the live document removed must still be accepted.
func TestRequireWorkflowRunInputReadsTheGraphItIsGiven(t *testing.T) {
	older := inputGraph(t, []any{Object{"name": "topic", "valueType": "string", "required": true}})
	newer := inputGraph(t, []any{
		Object{"name": "topic", "valueType": "string", "required": true},
		Object{"name": "added-later", "valueType": "string", "required": true},
	})

	noFault(t, func() { requireWorkflowRunInput(older, Object{"topic": "auth"}) })
	fault := faultOf(t, func() { requireWorkflowRunInput(newer, Object{"topic": "auth"}) })
	if fault.Params["field"] != "added-later" {
		t.Fatalf("the live schema was not the one applied: %+v", fault)
	}
}

// TestRequireWorkflowRunInputIgnoresUnprojectableVariables covers the graph that publishes fine but
// declares a variable Issues has no control for. It cannot have been filled in, so demanding it would
// refuse every run of a legal graph — the form drops it for the same reason.
func TestRequireWorkflowRunInputIgnoresUnprojectableVariables(t *testing.T) {
	graph := inputGraph(t, []any{
		Object{"name": "not a field key", "valueType": "string", "required": true},
		Object{"name": "topic", "valueType": "string", "required": true},
	})
	noFault(t, func() { requireWorkflowRunInput(graph, Object{"topic": "auth"}) })
}

// TestStringList covers the wire shape of a fault's node list: an empty list rather than null, so a
// client iterating it unconditionally cannot be surprised by a slot nothing references.
func TestStringList(t *testing.T) {
	if got := stringList(nil); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("stringList(nil) = %#v, want an empty list", got)
	}
	if got := stringList([]string{"a", "b"}); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("stringList = %#v", got)
	}
}
