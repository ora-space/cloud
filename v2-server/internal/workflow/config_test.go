package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// These acceptance/rejection pairs exercise the public publication boundary,
// following desktop start_input, agent_config, retry and structured_output tests.
func TestNodeConfigurationValidation(t *testing.T) {
	tests := []struct {
		name, kind string
		data       obj
		valid      bool
	}{
		{"legacy agent", "agent", obj{}, true},
		{"no local catalog required", "agent", obj{"agentConfig": obj{"executor": obj{"agentCli": "not-installed", "modelId": "not-installed"}, "prompt": ""}}, true},
		{"agent not object", "agent", obj{"agentConfig": false}, false},
		{"wrong prompt", "agent", obj{"agentConfig": obj{"prompt": 2}}, false},
		{"wrong executor", "agent", obj{"agentConfig": obj{"executor": "bad"}}, false},
		{"wrong interactive", "agent", obj{"agentConfig": obj{"interactive": "true"}}, false},
		{"bad skills shape", "agent", obj{"agentConfig": obj{"skills": nil}}, false},
		{"legacy skill", "agent", obj{"agentConfig": obj{"skills": []any{obj{}}}}, true},
		{"valid mcp", "agent", obj{"agentConfig": obj{"mcps": []any{obj{"mcpId": "official/tools", "enabled": true}}}}, true},
		{"noncanonical mcp", "agent", obj{"agentConfig": obj{"mcps": []any{obj{"mcpId": "tools", "enabled": true}}}}, false},
		{"mcp namespace limit", "agent", obj{"agentConfig": obj{"mcps": []any{obj{"mcpId": strings.Repeat("a", 34) + "/tools", "enabled": false}}}}, false},
		{"duplicate disabled mcp", "agent", obj{"agentConfig": obj{"mcps": []any{obj{"mcpId": "local/tools", "enabled": false}, obj{"mcpId": "local/tools", "enabled": false}}}}, false},
		{"mcp enabled required", "agent", obj{"agentConfig": obj{"mcps": []any{obj{"mcpId": "local/tools"}}}}, false},
		{"wait inactivity", "agent", obj{"agentConfig": obj{"promptInactivity": "wait"}}, true},
		{"unknown inactivity", "agent", obj{"agentConfig": obj{"promptInactivity": "forever"}}, false},
		{"retry limits", "agent", obj{"agentConfig": obj{"retry": obj{"enabled": true, "maxRetries": 5, "initialDelaySeconds": 300}}}, true},
		{"disabled retry still validated", "agent", obj{"agentConfig": obj{"retry": obj{"enabled": false, "maxRetries": 6, "initialDelaySeconds": 10}}}, false},
		{"retry missing field", "agent", obj{"agentConfig": obj{"retry": obj{"enabled": true, "maxRetries": 2}}}, false},
		{"retry fraction", "agent", obj{"agentConfig": obj{"retry": obj{"enabled": true, "maxRetries": 1.5, "initialDelaySeconds": 10}}}, false},
		{"retry negative", "agent", obj{"agentConfig": obj{"retry": obj{"enabled": true, "maxRetries": 2, "initialDelaySeconds": -1}}}, false},
		{"empty condition shell", "condition", obj{}, true},
		{"condition missing id", "condition", obj{"cases": []any{obj{"conditions": []any{}}}}, false},
		{"condition empty id is parseable", "condition", obj{"cases": []any{obj{"id": ""}}}, true},
		{"condition duplicate id is start check", "condition", obj{"cases": []any{obj{"id": "same"}, obj{"id": "same"}}}, true},
		{"condition wrong logic", "condition", obj{"cases": []any{obj{"id": "a", "logic": "xor"}}}, false},
		{"condition unknown operator", "condition", obj{"cases": []any{obj{"id": "a", "conditions": []any{obj{"variableSelector": []string{"start", "input"}, "operator": "regex"}}}}}, false},
		{"condition incomplete selector", "condition", obj{"cases": []any{obj{"id": "a", "conditions": []any{obj{"variableSelector": []string{"start"}, "operator": "exists"}}}}}, false},
		{"condition unknown producer is runtime lookup", "condition", obj{"cases": []any{obj{"id": "a", "conditions": []any{obj{"variableSelector": []string{"external", "value"}, "operator": "exists"}}}}}, true},
		{"output legacy rows", "output", obj{"outputs": []any{obj{}, obj{"name": "x", "variableSelector": []string{"start"}}}}, true},
		{"output wrong name type", "output", obj{"outputs": []any{obj{"name": false}}}, false},
		{"nullable output selector rejected", "output", obj{"outputs": []any{obj{"name": "x", "variableSelector": nil}}}, false},
		{"aggregator needs config", "aggregator", obj{}, false},
		{"aggregator needs selectors", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{}}}, false},
		{"aggregator root selector", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "input"}}}}, true},
		{"aggregator nested selector", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "input", "nested"}}}}, false},
		{"aggregator duplicate", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "input"}, []string{"start", "input"}}}}, false},
		{"aggregator unknown variable", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "unknown"}}}}, false},
		{"bad unrelated wire field", "output", obj{"cases": "unfinished"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			g := flat()
			addNode(g, "target", test.kind, test.data)
			connect(g, "start", "target")
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
}

func TestStartDeclarations(t *testing.T) {
	for _, test := range []struct {
		name     string
		variable obj
		valid    bool
	}{
		{"required without default", obj{"name": "x", "valueType": "string", "required": true}, true},
		{"empty name", obj{"name": " ", "valueType": "string"}, false},
		{"dot name", obj{"name": "x.y", "valueType": "string"}, false},
		{"unknown type", obj{"name": "x", "valueType": "decimal"}, false},
		{"typed array", obj{"name": "x", "valueType": "array[string]", "fieldType": "json", "value": []string{"a", "b"}}, true},
		{"mixed typed array", obj{"name": "x", "valueType": "array[string]", "value": []any{"a", 0}}, false},
		{"legacy integer", obj{"name": "x", "valueType": "integer", "value": 1}, true},
		{"integer negative zero", obj{"name": "x", "valueType": "integer", "value": json.Number("-0")}, false},
		{"integer minimum", obj{"name": "x", "valueType": "integer", "value": json.Number("-9223372036854775808")}, true},
		{"integer maximum", obj{"name": "x", "valueType": "integer", "value": json.Number("18446744073709551615")}, true},
		{"integer overflow", obj{"name": "x", "valueType": "integer", "value": json.Number("18446744073709551616")}, false},
		{"explicit number integer mismatch", obj{"name": "x", "valueType": "integer", "fieldType": "number"}, false},
		{"unicode length", obj{"name": "x", "valueType": "string", "maxLength": 2, "value": "中文"}, true},
		{"too long", obj{"name": "x", "valueType": "string", "maxLength": 2, "value": "中文三"}, false},
		{"zero length", obj{"name": "x", "valueType": "string", "maxLength": 0}, false},
		{"nontext length", obj{"name": "x", "valueType": "number", "maxLength": 4}, false},
		{"select", obj{"name": "x", "valueType": "string", "fieldType": "select", "options": []string{" a ", "b"}, "value": "a"}, true},
		{"duplicate trimmed option", obj{"name": "x", "valueType": "string", "fieldType": "select", "options": []string{" a ", "a"}}, false},
		{"select wrong default", obj{"name": "x", "valueType": "string", "fieldType": "select", "options": []string{"a"}, "value": "b"}, false},
		{"options on number", obj{"name": "x", "valueType": "number", "options": []string{"a"}}, false},
		{"checkbox zero", obj{"name": "x", "valueType": "boolean", "fieldType": "checkbox", "value": 0}, false},
		{"checkbox false", obj{"name": "x", "valueType": "boolean", "fieldType": "checkbox", "value": false}, true},
		{"null means unset", obj{"name": "x", "valueType": "number", "value": nil}, true},
		{"legacy file", obj{"name": "x", "valueType": "file", "value": "docs\\./file.txt"}, true},
		{"file root", obj{"name": "x", "valueType": "file", "value": "./"}, false},
		{"file traversal", obj{"name": "x", "valueType": "file", "value": "docs/../file"}, false},
		{"file drive", obj{"name": "x", "valueType": "file", "value": "safe/C:secret"}, false},
		{"file reserved", obj{"name": "x", "valueType": "array[file]", "value": []string{"safe.txt", "docs/NuL.md"}}, false},
		{"canonical file", obj{"name": "x", "valueType": "file", "value": obj{"kind": "workspace_file", "path": "safe.txt"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := flat()
			dataOf(g, "start")["inputVariables"] = []any{test.variable}
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
	g := flat()
	dataOf(g, "start")["inputVariables"] = []any{obj{"name": "x", "valueType": "string"}, obj{"name": " x ", "valueType": "string"}}
	requireInvalid(t, g, "")
}

func TestStructuredSchemaAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema any
		valid  bool
	}{
		{"object", obj{"type": "object", "properties": obj{"result": obj{"type": "array", "items": obj{"type": "string"}}}, "required": []string{"result"}, "additionalProperties": false}, true},
		{"unknown extensions retained", obj{"type": "object", "description": "{{#unused.output#}}", "future": true}, true},
		{"root array", obj{"type": "array"}, false},
		{"missing schema", nil, false},
		{"nonobject schema", false, false},
		{"missing type", obj{}, false},
		{"unsupported property type", obj{"type": "object", "properties": obj{"x": obj{"type": "undefined"}}}, false},
		{"unknown required", obj{"type": "object", "required": []string{"missing"}}, false},
		{"required not array", obj{"type": "object", "required": "x"}, false},
		{"additional not boolean", obj{"type": "object", "additionalProperties": obj{}}, false},
		{"items on object", obj{"type": "object", "items": obj{"type": "string"}}, false},
		{"properties on scalar", obj{"type": "object", "properties": obj{"x": obj{"type": "string", "properties": obj{}}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := flat()
			addNode(g, "agent", "agent", obj{"agentConfig": obj{"outputContract": obj{"type": "structured", "schema": test.schema}}})
			connect(g, "start", "agent")
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
}

func TestGlobalAndDerivedTypes(t *testing.T) {
	for _, test := range []struct {
		name    string
		globals any
		valid   bool
	}{
		{"valid", []any{obj{"name": "global.flag", "valueType": "boolean", "value": false}, obj{"name": "global.zero", "valueType": "number", "value": 0}}, true},
		{"no value", []any{obj{"name": "global.x", "valueType": "any"}}, false},
		{"null is not assigned default", []any{obj{"name": "global.x", "valueType": "any", "value": nil}}, false},
		{"unqualified", []any{obj{"name": "x", "valueType": "string", "value": ""}}, false},
		{"wrong value", []any{obj{"name": "global.x", "valueType": "number", "value": "1"}}, false},
		{"duplicate", []any{obj{"name": "global.x", "valueType": "string", "value": "a"}, obj{"name": "global.x", "valueType": "string", "value": "b"}}, false},
		{"system defaults", []any{obj{"name": "sys.workflow_id", "valueType": "string"}, obj{"name": "sys.timestamp", "valueType": "number"}}, true},
		{"null list", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := flat()
			g["globalVariables"] = test.globals
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
	g := flat()
	dataOf(g, "start")["inputVariables"] = []any{obj{"name": "count", "valueType": "number"}}
	addNode(g, "agg", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "input"}, []string{"start", "count"}}}})
	connect(g, "start", "agg")
	requireInvalid(t, g, "invalid_aggregator")
	// A declared downstream producer still cannot supply an aggregator.
	dataOf(g, "agg")["aggregatorConfig"] = obj{"variables": []any{[]string{"later", "output"}}}
	addNode(g, "later", "agent", nil)
	connect(g, "agg", "later")
	requireInvalid(t, g, "invalid_aggregator")
	// Cycles reachable from Start are rejected, while the spare fixture's cycle is ignored.
	connect(g, "later", "start")
	requireInvalid(t, g, "cycle_detected")
	if _, err := workflow.AnalyzeWorkflow(encode(t, g)); err != nil {
		t.Fatalf("analysis must not perform DAG execution validation: %v", err)
	}
}

func TestDerivedCatalogKeepsFirstDeclaration(t *testing.T) {
	g := flat()
	dataOf(g, "start")["inputVariables"] = []any{obj{"name": "input", "valueType": "number"}}
	addNode(g, "agent", "agent", nil)
	addNode(g, "agg", "aggregator", obj{"aggregatorConfig": obj{"variables": []any{[]string{"start", "input"}, []string{"agent", "output"}}}})
	connect(g, "start", "agent")
	connect(g, "agent", "agg")
	// The reserved Start input was declared as string before the user input.
	requireValid(t, g)
	// Globals precede both base and derived node declarations, as on desktop.
	g["globalVariables"] = []any{obj{"name": "agent.output", "valueType": "number", "value": 0}}
	requireInvalid(t, g, "invalid_aggregator")
}
