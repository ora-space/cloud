package taskdecomp

import (
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// OutputRequirement is the user-facing contract a decomposition workflow must declare.
const OutputRequirement = "用于拆分的工作流，输出节点必须声明名为 tasks 的输出，内容是 {title, description} 的数组。"

// CanDecompose reports whether a published workflow snapshot declares the output Subtasks consumes:
// an Output node with a result named tasks. Its declared type may be array, array[object], string
// (for a JSON-encoded array), or untyped/any; every other declared type, including arrays of other
// element types such as array[string], is rejected because Subtasks needs objects with a title.
// Incomplete legacy output rows, unreachable Output nodes and malformed graphs do not satisfy the
// rule.
func CanDecompose(graph string) bool {
	g, err := workflow.ParseGraph(graph)
	if err != nil {
		return false
	}
	pool := workflow.NewPool(g)
	for _, n := range g.Nodes() {
		if n.Kind != "output" {
			continue
		}
		for _, binding := range n.OutputBindings() {
			if binding.Name == "tasks" && acceptsTasksType(declaredType(g, pool, binding.Selector)) {
				return true
			}
		}
	}
	return false
}

func acceptsTasksType(typ string) bool {
	switch typ {
	case "", "any", "string", "array", "array[object]":
		return true
	}
	return false
}

// declaredType resolves the selected root declaration and, for structured output, the schema path.
// Unknown paths stay any, matching the editor's untyped declaration behavior.
func declaredType(g *workflow.Graph, pool workflow.VariablePool, selector []string) string {
	if len(selector) < 2 {
		return "any"
	}
	def, ok := pool.Catalog[strings.Join(selector[:2], ".")]
	if !ok {
		return "any"
	}
	if len(selector) == 2 {
		return def.ValueType
	}
	n, ok := g.Node(selector[0])
	if !ok {
		return "any"
	}
	// Only an Agent's structured output has a schema to walk; other nested paths stay any.
	if n.Kind != "agent" {
		return "any"
	}
	schema := n.Agent().Structured
	for _, segment := range selector[2:] {
		properties, _ := schema["properties"].(map[string]any)
		next, _ := properties[segment].(map[string]any)
		if next == nil {
			return "any"
		}
		schema = next
	}
	typ, _ := schema["type"].(string)
	if typ != "array" {
		if typ == "" {
			return "any"
		}
		return typ
	}
	items, _ := schema["items"].(map[string]any)
	itemType, _ := items["type"].(string)
	if itemType == "" {
		return "array"
	}
	return "array[" + itemType + "]"
}
