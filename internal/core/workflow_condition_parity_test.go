package core

import (
	"encoding/json"
	"testing"
)

// Cross-checks for circus#210: the same graph input and the same condition document must produce the
// same branch on Cloud and on desktop. Each case mirrors a test in desktop's
// crates/application/src/workflow_run/engine/condition.rs (named in the table), so the expectation
// is not Cloud's opinion of the operator but the reference implementation's answer.
//
// Desktop's tests are named here as the source of the expected behavior; this file does not run the
// Rust engine. A change to either side that breaks parity must break one of these cases.
func TestWorkflowConditionDesktopParity(t *testing.T) {
	// The pool shapes mirror pool_with_review(): a text root, a structured_output object, and a
	// start node whose variables include a numeric limit. `agent-a` is present with an empty output,
	// modeling desktop's declared-but-unassigned `agent-a.text` (that test declares the variable by
	// hand). This exercises the evaluator's unset answer; a selector the graph itself does not
	// declare is a different case, refused at publish as condition_selector_undeclared.
	pool := map[string]workflowNodeState{
		"review": {Status: "succeeded", Output: Object{
			"text":              "审查完成",
			"structured_output": Object{"approved": true, "score": 92.0},
		}},
		"start": {Status: "succeeded", Output: Object{
			"token": "审查",
			"limit": 90.0,
		}},
		"agent-a": {Status: "succeeded", Output: Object{}},
	}
	evaluate := func(t *testing.T, cases []any) (string, error) {
		t.Helper()
		return evaluateWorkflowCondition(workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": cases}}, pool)
	}
	rule := func(selector []any, operator string, value any) Object {
		return Object{"variableSelector": selector, "operator": operator, "value": value}
	}
	one := func(selector []any, operator string, value any) []any {
		return []any{Object{"id": "c", "logic": "and", "conditions": []any{rule(selector, operator, value)}}}
	}

	cases := []struct {
		name      string
		desktop   string
		cases     []any
		want      string
		wantError bool
	}{
		{
			name:    "and requires every rule",
			desktop: "logic_and_requires_all_or_requires_any",
			cases: []any{Object{"id": "high", "logic": "and", "conditions": []any{
				rule([]any{"review", "text"}, "not_empty", nil),
				rule([]any{"review", "structured_output", "score"}, "greater_than", 90.0),
			}}},
			want: "high",
		},
		{
			name:    "or requires any rule",
			desktop: "logic_and_requires_all_or_requires_any",
			cases: []any{Object{"id": "flag", "logic": "or", "conditions": []any{
				rule([]any{"review", "text"}, "contains", "不存在"),
				rule([]any{"review", "structured_output", "score"}, "greater_than_or_equal", 92.0),
			}}},
			want: "flag",
		},
		{
			name:    "existence tolerates an unassigned variable",
			desktop: "existence_and_empty_handle_unset_variables",
			cases: []any{Object{"id": "missing", "logic": "and", "conditions": []any{
				rule([]any{"agent-a", "text"}, "not_exists", nil),
			}}},
			want: "missing",
		},
		{
			name:      "a value comparison on an unassigned variable fails",
			desktop:   "value_comparison_fails_on_unset_variable",
			cases:     one([]any{"agent-a", "text"}, "equals", "x"),
			wantError: true,
		},
		{
			name:    "string operators match on text",
			desktop: "string_operators_match_on_text",
			cases:   one([]any{"review", "text"}, "contains", "完成"),
			want:    "c",
		},
		{
			name:    "numeric operators compare across representations",
			desktop: "numeric_operators_compare_across_representations",
			// The expected value is an integer while the pool value is a float, as desktop's json!(90)
			// against a float score.
			cases: one([]any{"review", "structured_output", "score"}, "greater_than", 90),
			want:  "c",
		},
		{
			name:      "contains on an object is a type mismatch",
			desktop:   "string_operator_on_object_fails_as_type_mismatch",
			cases:     one([]any{"review", "structured_output"}, "contains", "approved"),
			wantError: true,
		},
		{
			name:    "an exclusive placeholder keeps the bound type",
			desktop: "comparison_value_placeholder_keeps_bound_type",
			cases:   one([]any{"review", "structured_output", "score"}, "greater_than", "{{#start.limit#}}"),
			want:    "c",
		},
		{
			name:    "a mixed template renders as a string",
			desktop: "mixed_comparison_value_template_renders_as_string",
			cases:   one([]any{"review", "text"}, "contains", "{{#start.token#}}完"),
			want:    "c",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluate(t, tc.cases)
			if tc.wantError {
				if err == nil {
					t.Fatalf("selected %q; want the error desktop raises for %s", got, tc.desktop)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluation error: %v (desktop: %s)", err, tc.desktop)
			}
			if got != tc.want {
				t.Fatalf("selected %q; want %q (desktop: %s)", got, tc.want, tc.desktop)
			}
		})
	}
}

// An explicit `"value": null` is the missing operand, not a value to compare: desktop's
// WireConditionRule.value is Option<Value> and serde reads a JSON null as None, so a comparison
// operator reports a type mismatch instead of comparing against null. This goes through a decoded
// JSON document so the wire spelling is what is pinned, not a Go nil the test author typed.
func TestWorkflowConditionExplicitNullExpected(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(`{
		"cases": [{
			"id": "c",
			"logic": "and",
			"conditions": [{
				"variableSelector": ["s", "nothing"],
				"operator": "equals",
				"value": null
			}]
		}]
	}`), &doc); err != nil {
		t.Fatal(err)
	}
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"nothing": nil}}}
	node := workflowNodeView{ID: "c", Kind: "condition", Data: Object(doc)}
	if branch, err := evaluateWorkflowCondition(node, states); err == nil {
		t.Fatalf("explicit null expected value matched %q; want a type-mismatch error as desktop's None", branch)
	}
}

// A template that resolves to null is a present operand, unlike a literal `value: null`: desktop's
// Option<Value> reads the literal null as None, but a placeholder resolves to Some(Value::Null),
// which the operator then compares. resolveWorkflowExpected keeps hasExpected=true for the template
// even though the bound value is JSON null, while the literal is reported absent.
func TestWorkflowExpectedNullLiteralVersusTemplate(t *testing.T) {
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"nothing": nil}}}

	literal, hasExpected, err := resolveWorkflowExpected(Object{"value": nil}, states)
	if err != nil || hasExpected || literal != nil {
		t.Fatalf("literal null = (%v, %v, %v); want an absent operand", literal, hasExpected, err)
	}

	templated, hasExpected, err := resolveWorkflowExpected(Object{"value": "{{#s.nothing#}}"}, states)
	if err != nil || !hasExpected {
		t.Fatalf("template null = (%v, %v, %v); want a present null value", templated, hasExpected, err)
	}
}

// The selector shape is part of parity: desktop indexes [node, root, ...nested] against the node's
// variable roots, so [start, limit] reads a run-form variable and [start, input] reads the kickoff
// prompt. The old Cloud shape nested the whole form under input, which made [start, limit] resolve
// nothing and [start, input, limit] accidentally work.
func TestWorkflowConditionSelectorShapeMatchesDesktop(t *testing.T) {
	node := workflowNodeView{ID: "start", Kind: "start", Data: Object{"kind": "start", "input": "kickoff"}}
	output := workflowStartOutput(node, Object{"limit": 90.0})
	states := map[string]workflowNodeState{"start": {Status: "succeeded", Output: output}}

	rule := func(selector []any) Object {
		return Object{"id": "c", "logic": "and", "conditions": []any{
			Object{"variableSelector": selector, "operator": "equals", "value": 90.0},
		}}
	}
	if branch, err := evaluateWorkflowCondition(
		workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": []any{rule([]any{"start", "limit"})}}}, states,
	); err != nil || branch != "c" {
		t.Fatalf("desktop-shaped selector [start, limit] = %q, %v; want c", branch, err)
	}
	if _, err := evaluateWorkflowCondition(
		workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": []any{rule([]any{"start", "input", "limit"})}}}, states,
	); err == nil {
		t.Fatal("old Cloud-shaped selector [start, input, limit] resolved; want a path error")
	}

	promptRule := Object{"id": "c", "logic": "and", "conditions": []any{
		Object{"variableSelector": []any{"start", "input"}, "operator": "equals", "value": "kickoff"},
	}}
	if branch, err := evaluateWorkflowCondition(
		workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": []any{promptRule}}}, states,
	); err != nil || branch != "c" {
		t.Fatalf("[start, input] = %q, %v; want c", branch, err)
	}
}

// A condition error fails the node and the run instead of committing a branch. The run's failure
// reason is the evaluation message, so the viewer names the selector that needs fixing.
func TestWorkflowConditionFailureFailsTheNode(t *testing.T) {
	node := workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": []any{
		Object{"id": "yes", "logic": "and", "conditions": []any{
			Object{"variableSelector": []any{"s", "missing"}, "operator": "equals", "value": true},
		}},
	}}}
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{}}}
	state := executeWorkflowNode(node, nil, states, nil)
	if state.Status != "failed" || state.Error == "" {
		t.Fatalf("state = %+v; want a failed node carrying the evaluation error", state)
	}
	if state.Output.S("selectedBranch") != "" {
		t.Fatalf("failed condition committed branch %q", state.Output.S("selectedBranch"))
	}
	if state.Output.S("reason") != state.Error {
		t.Fatalf("reason %q != error %q", state.Output.S("reason"), state.Error)
	}
}
