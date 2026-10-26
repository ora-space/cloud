package core

import (
	"fmt"
	"strings"
)

// workflowOutputBinding names one final result without importing unrelated predecessor values.
type workflowOutputBinding struct {
	Name     string
	Selector workflowSelector
}

// readWorkflowOutputBindings is shared by publication and execution so malformed authored rows
// cannot be silently skipped into a successful, partial result. An absent list retains empty output.
func readWorkflowOutputBindings(nodeID string, data Object) ([]workflowOutputBinding, []workflowGraphProblem) {
	value, present := data["outputs"]
	if !present || value == nil {
		return nil, nil
	}
	rows, ok := value.([]any)
	if !ok {
		return nil, []workflowGraphProblem{{Code: "output_bindings_not_a_list", NodeID: nodeID, Field: "outputs"}}
	}
	var bindings []workflowOutputBinding
	var problems []workflowGraphProblem
	seen := map[string]bool{}
	for _, row := range rows {
		binding, object := asWorkflowObject(row)
		name := binding.S("name")
		if !object || strings.TrimSpace(name) == "" {
			problems = append(problems, workflowGraphProblem{Code: "output_binding_invalid", NodeID: nodeID, Field: "outputs.name"})
			continue
		}
		if seen[name] {
			problems = append(problems, workflowGraphProblem{Code: "output_name_duplicate", NodeID: nodeID, Field: "outputs.name"})
		}
		seen[name] = true
		selector, valid := parseWorkflowSelector(binding["variableSelector"])
		if !valid {
			problems = append(problems, workflowGraphProblem{Code: "output_selector_unsupported", NodeID: nodeID, Field: "outputs.variableSelector"})
			continue
		}
		bindings = append(bindings, workflowOutputBinding{Name: name, Selector: selector})
	}
	return bindings, problems
}

// validateWorkflowOutputConfig checks roots against the consumer's scope, including frozen globals.
func validateWorkflowOutputConfig(nodeID string, data Object, roots map[string]map[string]bool) []workflowGraphProblem {
	bindings, problems := readWorkflowOutputBindings(nodeID, data)
	for _, binding := range bindings {
		if !roots[binding.Selector.nodeID][binding.Selector.root] {
			problems = append(problems, workflowGraphProblem{Code: "output_selector_undeclared", NodeID: nodeID, Field: "outputs.variableSelector"})
		}
	}
	return problems
}

// workflowOutputFailure separates the public reason from the actionable, bounded diagnostic.
type workflowOutputFailure struct {
	Reason  string
	Message string
}

// Error reports the binding that failed without including any variable values.
func (failure *workflowOutputFailure) Error() string { return failure.Message }

// evaluateWorkflowOutput resolves all bindings against one committed snapshot. Missing or invalid
// paths fail the whole node; explicit null, false, zero and empty values remain assigned results.
func evaluateWorkflowOutput(node workflowNodeView, states map[string]workflowNodeState) (Object, *workflowOutputFailure) {
	bindings, problems := readWorkflowOutputBindings(node.ID, node.Data)
	if len(problems) > 0 {
		return nil, &workflowOutputFailure{Reason: "output_invalid_config", Message: "output bindings are invalid"}
	}
	result := Object{}
	for _, binding := range bindings {
		value, present, err := resolveWorkflowSelector(binding.Selector, states)
		if err != nil {
			return nil, &workflowOutputFailure{Reason: "output_invalid_path", Message: fmt.Sprintf("result %s references invalid path %s", binding.Name, binding.Selector.qualified())}
		}
		if !present {
			return nil, &workflowOutputFailure{Reason: "output_value_unset", Message: fmt.Sprintf("result %s references unassigned variable %s", binding.Name, binding.Selector.qualified())}
		}
		result[binding.Name] = value
	}
	return result, nil
}
