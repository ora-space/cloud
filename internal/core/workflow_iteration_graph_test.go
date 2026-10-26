package core

import (
	"reflect"
	"testing"
)

// The iteration publish contract's configuration matrix. Execution is a separate slice, so these
// tests exercise the validator directly; the gate wires it in when the round engine lands.

// iterationFixture is one small region: outer start `s` (declares `items`), region members `m1` and
// `m2` (m1 → m2, both declare `output`) and the iteration node `it`.
type iterationFixture struct {
	nodes    []any
	edges    []any
	declared map[string]map[string]bool
}

func newIterationFixture() iterationFixture {
	return iterationFixture{
		nodes: []any{
			Object{"id": "s", "data": Object{"kind": "start", "title": "开始"}},
			Object{"id": "it", "parentId": "", "data": Object{"kind": "iteration", "title": "迭代", "iterationConfig": Object{
				"iteratorSelector": []any{"s", "items"},
				"collectSelector":  []any{"m2", "output"},
				"errorStrategy":    "continue",
				"maxIterations":    float64(50),
			}}},
			Object{"id": "m1", "parentId": "it", "data": Object{"kind": "agent", "title": "m1"}},
			Object{"id": "m2", "parentId": "it", "data": Object{"kind": "agent", "title": "m2"}},
		},
		edges: []any{Object{"source": "s", "target": "it"}, Object{"source": "m1", "target": "m2"}},
		declared: map[string]map[string]bool{
			"s":  {"items": true},
			"m1": {"output": true},
			"m2": {"output": true},
		},
	}
}

func (f iterationFixture) nodeData() Object {
	for _, raw := range f.nodes {
		node, _ := asWorkflowObject(raw)
		if node.S("id") == "it" {
			return node.O("data")
		}
	}
	return Object{}
}

func (f iterationFixture) problems(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, problem := range validateIterationConfig("it", f.nodeData(), f.nodes, f.edges, f.declared) {
		entry := problem.Code
		if problem.Field != "" {
			entry += "#" + problem.Field
		}
		out = append(out, entry)
	}
	return out
}

// TestWorkflowPublishValidatesIterationConfig covers every refusal reason in the iteration ADR's D2
// table plus the entry/reachability rule from D3.
func TestWorkflowPublishValidatesIterationConfig(t *testing.T) {
	if got := newIterationFixture().problems(t); len(got) != 0 {
		t.Fatalf("a well-formed iteration must publish, got %v", got)
	}
	cases := []struct {
		name   string
		mutate func(*iterationFixture)
		want   []string
	}{
		{
			name:   "missing config",
			mutate: func(f *iterationFixture) { f.nodes[1].(Object)["data"].(Object)["iterationConfig"] = nil },
			want:   []string{"iteration_config_missing#iterationConfig"},
		},
		{
			name: "iterator is not a root selector",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["iteratorSelector"] = []any{"s"}
			},
			want: []string{"iteration_iterator_unsupported#iterationConfig.iteratorSelector"},
		},
		{
			name: "iterator has a nested path",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["iteratorSelector"] = []any{"s", "items", "name"}
			},
			want: []string{"iteration_iterator_unsupported#iterationConfig.iteratorSelector"},
		},
		{
			name: "iterator root is undeclared",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["iteratorSelector"] = []any{"s", "missing"}
			},
			want: []string{"iteration_iterator_undeclared#iterationConfig.iteratorSelector"},
		},
		{
			name: "iterator reads a region root",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["iteratorSelector"] = []any{"m1", "output"}
			},
			want: []string{"iteration_iterator_undeclared#iterationConfig.iteratorSelector"},
		},
		{
			name: "collect is not a root selector",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["collectSelector"] = []any{"m2", "output", "text"}
			},
			want: []string{"iteration_collect_unsupported#iterationConfig.collectSelector"},
		},
		{
			name: "collect target is outside the region",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["collectSelector"] = []any{"s", "items"}
			},
			want: []string{"iteration_collect_outside_region#iterationConfig.collectSelector"},
		},
		{
			name: "error strategy is unknown",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["errorStrategy"] = "retry"
			},
			want: []string{"iteration_error_strategy_unknown#iterationConfig.errorStrategy"},
		},
		{
			name: "max iterations is zero",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["maxIterations"] = float64(0)
			},
			want: []string{"iteration_max_iterations_invalid#iterationConfig.maxIterations"},
		},
		{
			name: "max iterations is fractional",
			mutate: func(f *iterationFixture) {
				f.nodeData()["iterationConfig"].(Object)["maxIterations"] = 2.5
			},
			want: []string{"iteration_max_iterations_invalid#iterationConfig.maxIterations"},
		},
		{
			name: "region is empty",
			mutate: func(f *iterationFixture) {
				f.nodes = append(f.nodes[:2], f.nodes[2:]...)
				f.nodes = f.nodes[:2]
			},
			want: []string{"iteration_collect_outside_region#iterationConfig.collectSelector", "iteration_body_empty"},
		},
		{
			name: "region member is unreachable",
			mutate: func(f *iterationFixture) {
				f.nodes = append(f.nodes, Object{"id": "m3", "parentId": "it", "data": Object{"kind": "agent", "title": "m3"}})
				f.declared["m3"] = map[string]bool{"output": true}
				// A two-member cycle with no entry: m1 is the region's only entry but reaches
				// nothing else, so m2 and m3 would never run in any round.
				f.edges = []any{
					Object{"source": "s", "target": "it"},
					Object{"source": "m2", "target": "m3"},
					Object{"source": "m3", "target": "m2"},
				}
			},
			want: []string{"iteration_body_unreachable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newIterationFixture()
			tc.mutate(&fixture)
			if got := fixture.problems(t); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkflowPublishAcceptsAnIterationRegion pins the end-to-end gate: a start feeding an iteration
// whose single aggregator member collects from the start passes every rule at once.
func TestWorkflowPublishAcceptsAnIterationRegion(t *testing.T) {
	graph := Object{
		"nodes": []any{
			Object{"id": "s", "data": Object{"kind": "start", "title": "开始", "inputVariables": []any{Object{"name": "items", "valueType": "array"}}}},
			Object{"id": "it", "data": Object{"kind": "iteration", "title": "迭代", "iterationConfig": Object{
				"iteratorSelector": []any{"s", "items"},
				"collectSelector":  []any{"agg", "output"},
				"errorStrategy":    "continue",
				"maxIterations":    float64(50),
			}}},
			Object{"id": "agg", "parentId": "it", "data": Object{"kind": "aggregator", "title": "聚合", "aggregatorConfig": Object{"variables": []any{[]any{"s", "items"}}}}},
			Object{"id": "o", "data": Object{"kind": "output"}},
		},
		"edges": []any{
			Object{"source": "s", "target": "it"},
			Object{"source": "it", "target": "o"},
		},
	}
	if problems := validateWorkflowGraph(decodeGraph(t, graph)); len(problems) != 0 {
		t.Fatalf("a well-formed iteration region must publish, got %v", problems)
	}
}
