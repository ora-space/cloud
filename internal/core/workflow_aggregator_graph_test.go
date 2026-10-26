package core

import (
	"reflect"
	"testing"
)

// The aggregator's publish gate: an ordered list of root selectors, each naming a declared producer,
// with no nested path and no duplicate. The scheduler reads the same list, so a refused document is
// exactly one the run could not execute.

// aggregatorTestGraph is start feeding two agents that both merge into one aggregator and then an
// output — the branch fan-in the node exists for.
func aggregatorTestGraph(config Object) Object {
	agent := func(id string) Object {
		return Object{"id": id, "data": Object{"kind": "agent", "title": id, "agentConfig": Object{"schemaVersion": 3, "prompt": "x"}}}
	}
	return Object{
		"nodes": []any{
			Object{"id": "s", "data": Object{"kind": "start", "title": "开始", "inputVariables": []any{}}},
			agent("a1"),
			agent("a2"),
			Object{"id": "agg", "data": Object{"kind": "aggregator", "title": "合并", "aggregatorConfig": config}},
			Object{"id": "o", "data": Object{"kind": "output"}},
		},
		"edges": []any{
			Object{"source": "s", "target": "a1"},
			Object{"source": "s", "target": "a2"},
			Object{"source": "a1", "target": "agg"},
			Object{"source": "a2", "target": "agg"},
			Object{"source": "agg", "target": "o"},
		},
	}
}

// aggregatorProblems projects problems as `code@node#field` strings for exact comparison.
func aggregatorProblems(t *testing.T, graph Object) []string {
	t.Helper()
	out := []string{}
	for _, problem := range validateWorkflowGraph(decodeGraph(t, graph)) {
		entry := problem.Code
		if problem.NodeID != "" {
			entry += "@" + problem.NodeID
		}
		if problem.Field != "" {
			entry += "#" + problem.Field
		}
		out = append(out, entry)
	}
	return out
}

// TestWorkflowPublishValidatesAggregatorConfig is the configuration matrix.
func TestWorkflowPublishValidatesAggregatorConfig(t *testing.T) {
	valid := Object{"variables": []any{[]any{"a1", "output"}, []any{"a2", "output"}}}
	if got := aggregatorProblems(t, aggregatorTestGraph(valid)); len(got) != 0 {
		t.Fatalf("a well-formed aggregator must publish, got %v", got)
	}
	cases := []struct {
		name   string
		config any
		want   []string
	}{
		{name: "missing config", config: nil, want: []string{"aggregator_config_missing@agg#aggregatorConfig"}},
		{name: "variables not a list", config: Object{"variables": "a1.output"}, want: []string{"aggregator_variables_not_a_list@agg#aggregatorConfig.variables"}},
		{name: "empty selector list", config: Object{"variables": []any{}}, want: []string{"aggregator_variables_empty@agg#aggregatorConfig.variables"}},
		{name: "one-segment selector", config: Object{"variables": []any{[]any{"a1"}}}, want: []string{"aggregator_selector_unsupported@agg#aggregatorConfig.variables"}},
		{name: "nested selector", config: Object{"variables": []any{[]any{"a1", "output", "text"}}}, want: []string{"aggregator_selector_unsupported@agg#aggregatorConfig.variables"}},
		{name: "duplicate selector", config: Object{"variables": []any{[]any{"a1", "output"}, []any{"a1", "output"}}}, want: []string{"aggregator_selector_duplicate@agg#aggregatorConfig.variables"}},
		{name: "undeclared producer", config: Object{"variables": []any{[]any{"missing", "output"}}}, want: []string{"aggregator_selector_undeclared@agg#aggregatorConfig.variables"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := aggregatorTestGraph(nil)
			if tc.config != nil {
				graph = aggregatorTestGraph(tc.config.(Object))
			}
			if got := aggregatorProblems(t, graph); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}
}
