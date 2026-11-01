package workflow_test

import "testing"

func TestLoopValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		edit  func(obj)
		valid bool
	}{
		{"zero rounds", func(g obj) { loopConfig(g)["maxIterations"] = 0 }, false},
		{"hundred rounds", func(g obj) { loopConfig(g)["maxIterations"] = 100 }, true},
		{"over ceiling", func(g obj) { loopConfig(g)["maxIterations"] = 101 }, false},
		{"fraction rounds", func(g obj) { loopConfig(g)["maxIterations"] = 1.5 }, false},
		{"successful exhaustion", func(g obj) { loopConfig(g)["maxIterationsBehavior"] = "succeed" }, true},
		{"unknown exhaustion", func(g obj) { loopConfig(g)["maxIterationsBehavior"] = "continue" }, false},
		{"strict loop fields", func(g obj) { loopConfig(g)["typo"] = true }, false},
		{"missing variables", func(g obj) { delete(loopConfig(g), "variables") }, false},
		{"missing until", func(g obj) { delete(loopConfig(g), "until") }, false},
		{"missing outputs", func(g obj) { delete(loopConfig(g), "outputs") }, false},
		{"feedback selector", func(g obj) { carried(g)["feedback"] = []string{"writer"} }, false},
		{"carried type", func(g obj) { carried(g)["valueType"] = "unsupported" }, false},
		{"carried initial type", func(g obj) { carried(g)["initial"] = obj{"kind": "constant", "value": 0} }, false},
		{"literal null for any", func(g obj) {
			carried(g)["valueType"] = "any"
			carried(g)["initial"] = obj{"kind": "constant", "value": nil}
		}, true},
		{"missing constant value", func(g obj) {
			carried(g)["valueType"] = "any"
			carried(g)["initial"] = obj{"kind": "constant"}
		}, false},
		{"upstream initializer", func(g obj) { carried(g)["initial"] = obj{"kind": "variable", "selector": []string{"start", "input"}} }, true},
		{"body initializer", func(g obj) { carried(g)["initial"] = obj{"kind": "variable", "selector": []string{"writer", "output"}} }, false},
		{"invalid variable name", func(g obj) { carried(g)["name"] = " draft " }, false},
		{"duplicate output name", func(g obj) {
			loopConfig(g)["outputs"] = []any{obj{"name": "same", "variableSelector": []string{"writer", "output"}}, obj{"name": "same", "variableSelector": []string{"writer", "output"}}}
		}, false},
		{"exported output", func(g obj) {
			addNode(g, "out", "output", obj{"outputs": []any{obj{"name": "result", "variableSelector": []string{"loop", "result"}}}})
			connect(g, "loop", "out")
		}, true},
		{"child leaks outside", func(g obj) {
			addNode(g, "out", "output", obj{"outputs": []any{obj{"name": "result", "variableSelector": []string{"writer", "output"}}}})
			connect(g, "loop", "out")
		}, false},
		{"unknown feedback producer", func(g obj) { carried(g)["feedback"] = []string{"unrelated", "output"} }, false},
		{"spare malformed child", func(g obj) {
			addNode(g, "spare", "agent", obj{"containerId": "loop", "agentConfig": false})
			connect(g, "spare", "writer")
		}, true},
		{"active malformed child", func(g obj) {
			addNode(g, "spare", "agent", obj{"containerId": "loop", "agentConfig": false})
			connect(g, "entry", "spare")
		}, false},
		{"body output", func(g obj) { addNode(g, "out", "output", obj{"containerId": "loop"}); connect(g, "writer", "out") }, false},
		{"loop exit", func(g obj) { addNode(g, "exit", "loopExit", obj{"containerId": "loop"}); connect(g, "writer", "exit") }, true},
		{"loop exit successor", func(g obj) {
			addNode(g, "exit", "loopExit", obj{"containerId": "loop"})
			connect(g, "entry", "exit")
			connect(g, "exit", "writer")
		}, false},
		{"cross boundary edge", func(g obj) { connect(g, "writer", "loop") }, false},
		{"null parent on Loop", func(g obj) { g["nodes"].([]any)[1].(obj)["parentId"] = nil }, false},
		{"mismatched parent", func(g obj) { g["nodes"].([]any)[2].(obj)["parentId"] = "other" }, false},
		{"nested composite", func(g obj) { addNode(g, "nested", "iteration", obj{"containerId": "loop"}) }, false},
		{"explicit start required", func(g obj) { dataOf(g, "entry")["kind"] = "agent" }, false},
		{"required version", func(g obj) { g["schemaVersion"] = 1 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := fixture(t, "loop")
			test.edit(g)
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
	g := flat()
	addNode(g, "exit", "loopExit", nil)
	connect(g, "start", "exit")
	requireInvalid(t, g, "invalid_region")
}

func loopConfig(g obj) obj { return dataOf(g, "loop")["loopConfig"].(obj) }
func carried(g obj) obj    { return loopConfig(g)["variables"].([]any)[0].(obj) }

func TestIterationValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		edit  func(obj)
		valid bool
	}{
		{"continue", func(g obj) { iterConfig(g)["errorStrategy"] = "continue" }, true},
		{"invalid strategy", func(g obj) { iterConfig(g)["errorStrategy"] = "skip" }, false},
		{"positive configured ceiling", func(g obj) { iterConfig(g)["maxIterations"] = 1000 }, true},
		{"zero ceiling", func(g obj) { iterConfig(g)["maxIterations"] = 0 }, false},
		{"u32 overflow", func(g obj) { iterConfig(g)["maxIterations"] = 4294967296 }, false},
		{"missing config", func(g obj) { delete(dataOf(g, "iter"), "iterationConfig") }, false},
		{"missing source", func(g obj) { delete(iterConfig(g), "iteratorSelector") }, false},
		{"nonarray source", func(g obj) { iterConfig(g)["iteratorSelector"] = []string{"start", "input"} }, false},
		{"unknown source", func(g obj) { iterConfig(g)["iteratorSelector"] = []string{"unknown", "items"} }, false},
		{"collect outside region", func(g obj) { iterConfig(g)["collectSelector"] = []string{"out", "output"} }, false},
		{"nested collect", func(g obj) { iterConfig(g)["collectSelector"] = []string{"fix", "structured_output", "field"} }, false},
		{"unknown collect root", func(g obj) { iterConfig(g)["collectSelector"] = []string{"fix", "unknown"} }, false},
		{"unreachable collect", func(g obj) { iterConfig(g)["collectSelector"] = []string{"spare", "output"} }, false},
		{"interactive body", func(g obj) { dataOf(g, "fix")["agentConfig"] = obj{"interactive": true} }, false},
		{"output body", func(g obj) { dataOf(g, "fix")["kind"] = "output" }, false},
		{"escaping body", func(g obj) { connect(g, "fix", "out") }, false},
		{"outer enters member", func(g obj) { connect(g, "start", "fix") }, false},
		{"missing region", func(g obj) { delete(g["nodes"].([]any)[2].(obj), "parentId") }, false},
		{"spare ignored before region checks", func(g obj) { dataOf(g, "spare")["kind"] = "output" }, true},
		{"source produced inside own region", func(g obj) {
			dataOf(g, "fix")["kind"] = "start"
			dataOf(g, "fix")["inputVariables"] = []any{obj{"name": "items", "valueType": "array[string]"}}
			dataOf(g, "start")["kind"] = "agent"
			iterConfig(g)["iteratorSelector"] = []string{"fix", "items"}
			iterConfig(g)["collectSelector"] = []string{"fix", "input"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := fixture(t, "iteration")
			test.edit(g)
			if test.valid {
				requireValid(t, g)
			} else {
				requireInvalid(t, g, "")
			}
		})
	}
}

func iterConfig(g obj) obj { return dataOf(g, "iter")["iterationConfig"].(obj) }
