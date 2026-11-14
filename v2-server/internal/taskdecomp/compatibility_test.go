package taskdecomp_test

import (
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

func compatibilityGraph(t *testing.T, outputName string, selector []string, startType string, structured map[string]any, connect bool) string {
	t.Helper()
	startData := map[string]any{"kind": "start"}
	if startType != "" {
		startData["inputVariables"] = []any{map[string]any{"name": "items", "valueType": startType}}
	}
	nodes := []any{
		map[string]any{"id": "start", "data": startData},
		map[string]any{"id": "writer", "data": map[string]any{"kind": "agent"}},
		map[string]any{"id": "out", "data": map[string]any{"kind": "output", "outputs": []any{
			map[string]any{"name": outputName, "variableSelector": selector},
		}}},
	}
	if structured != nil {
		nodes[1] = map[string]any{"id": "writer", "data": map[string]any{"kind": "agent", "agentConfig": map[string]any{
			"outputContract": map[string]any{"type": "structured", "schema": structured},
		}}}
	}
	edges := []any{}
	if connect {
		edges = append(edges,
			map[string]any{"source": "start", "target": "writer"},
			map[string]any{"source": "writer", "target": "out"},
		)
	}
	raw, err := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCanDecompose(t *testing.T) {
	arraySchema := map[string]any{"type": "object", "properties": map[string]any{
		"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
	}}
	numberArraySchema := map[string]any{"type": "object", "properties": map[string]any{
		"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
	}}
	objectSchema := map[string]any{"type": "object", "properties": map[string]any{
		"tasks": map[string]any{"type": "object"},
	}}
	for _, tc := range []struct {
		name       string
		outputName string
		selector   []string
		startType  string
		structured map[string]any
		connect    bool
		want       bool
	}{
		{name: "structured object array", outputName: "tasks", selector: []string{"writer", "structured_output", "tasks"}, structured: arraySchema, connect: true, want: true},
		{name: "typed start array", outputName: "tasks", selector: []string{"start", "items"}, startType: "array[object]", connect: true, want: true},
		{name: "untyped array", outputName: "tasks", selector: []string{"start", "items"}, startType: "array", connect: true, want: true},
		{name: "json string", outputName: "tasks", selector: []string{"start", "input"}, connect: true, want: true},
		{name: "any", outputName: "tasks", selector: []string{"start", "items"}, startType: "any", connect: true, want: true},
		{name: "unknown declaration is untyped", outputName: "tasks", selector: []string{"missing", "value"}, connect: true, want: true},
		{name: "missing tasks", outputName: "summary", selector: []string{"start", "input"}, connect: true, want: false},
		{name: "wrong object type", outputName: "tasks", selector: []string{"writer", "structured_output", "tasks"}, structured: objectSchema, connect: true, want: false},
		{name: "array of strings", outputName: "tasks", selector: []string{"start", "items"}, startType: "array[string]", connect: true, want: false},
		{name: "array of numbers", outputName: "tasks", selector: []string{"writer", "structured_output", "tasks"}, structured: numberArraySchema, connect: true, want: false},
		{name: "whole structured output", outputName: "tasks", selector: []string{"writer", "structured_output"}, structured: arraySchema, connect: true, want: false},
		{name: "wrong scalar type", outputName: "tasks", selector: []string{"start", "items"}, startType: "number", connect: true, want: false},
		{name: "unreachable output", outputName: "tasks", selector: []string{"start", "input"}, connect: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := compatibilityGraph(t, tc.outputName, tc.selector, tc.startType, tc.structured, tc.connect)
			if got := taskdecomp.CanDecompose(graph); got != tc.want {
				t.Fatalf("CanDecompose() = %v, want %v", got, tc.want)
			}
		})
	}
	if taskdecomp.CanDecompose("not JSON") {
		t.Fatal("malformed graph passed")
	}
	if taskdecomp.CanDecompose(`{"nodes":[],"edges":[]}`) {
		t.Fatal("graph without an Output node passed")
	}
}
