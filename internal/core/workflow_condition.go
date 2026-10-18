package core

import "strings"

// Workflow Condition node semantics, ported from desktop's engine/condition.rs so a branch decision
// means the same thing on both sides (specs#99 D1 条件④: the scheduler boundary mirrors desktop's
// ports, and desktop is the reference implementation).
//
// The variable pool is the run's committed node outputs: a rule's variableSelector [nodeId, ...path]
// resolves by reading prior node states — selector[0] names the node, selector[1:] walks into its
// output object. A variable a prior node has not produced (unknown node, absent path, or a node that
// never ran) is absent, which the operators handle deliberately: exists / not_exists / empty /
// not_empty classify absence, and every other operator compares nothing — a comparator on an absent
// variable does not match. None of this is a run failure: an unevaluable condition has an else
// branch to fall back to, and a failed run should name a real execution failure, not a document the
// author must fix anyway.

// workflowConditionOperator evaluates one comparison rule. absent reports whether the selector
// resolved to nothing; resolved is the variable when it is present.
type workflowConditionOperator = func(resolved any, absent bool, expected any) bool

// workflowConditionOperators is desktop's full wire operator set (condition.rs
// ComparisonOperator::parse). `is`/`is_not` are the same comparisons as `equals`/`not_equals`.
var workflowConditionOperators = map[string]workflowConditionOperator{
	"equals":                workflowCompareEquals,
	"is":                    workflowCompareEquals,
	"not_equals":            workflowCompareNotEquals,
	"is_not":                workflowCompareNotEquals,
	"contains":              workflowCompareContains,
	"not_contains":          workflowCompareNotContains,
	"starts_with":           workflowCompareTextOrder("starts"),
	"ends_with":             workflowCompareTextOrder("ends"),
	"empty":                 workflowCompareEmpty,
	"not_empty":             workflowCompareNegate(workflowCompareEmpty),
	"exists":                workflowCompareExists,
	"not_exists":            workflowCompareNegate(workflowCompareExists),
	"greater_than":          workflowCompareNumeric("greater"),
	"greater_than_or_equal": workflowCompareNumeric("greater_eq"),
	"less_than":             workflowCompareNumeric("less"),
	"less_than_or_equal":    workflowCompareNumeric("less_eq"),
}

func workflowCompareEquals(resolved any, absent bool, expected any) bool {
	if absent {
		return false
	}
	return workflowJSONEqual(resolved, expected)
}

func workflowCompareNotEquals(resolved any, absent bool, expected any) bool {
	if absent {
		return false
	}
	return !workflowJSONEqual(resolved, expected)
}

func workflowCompareContains(resolved any, absent bool, expected any) bool {
	if absent {
		return false
	}
	return strings.Contains(workflowTextOf(resolved), workflowTextOf(expected))
}

// workflowCompareNotContains is contains' negation with one deliberate exception: absence. Every
// comparator in this file treats an absent variable as a no-match (see the file comment), so
// not_contains has to answer false there too. Negating the inner verdict instead would answer
// true, which puts not_contains and not_equals on opposite branches of the same absent variable —
// not_contains would select its case while not_equals fell through to else. The negation belongs to
// the comparison, not to the classification of absence.
func workflowCompareNotContains(resolved any, absent bool, expected any) bool {
	if absent {
		return false
	}
	return !workflowCompareContains(resolved, absent, expected)
}

// workflowCompareTextOrder serves both text-position operators: "starts" is prefix, "ends" is
// suffix; any other direction is unreachable.
func workflowCompareTextOrder(direction string) workflowConditionOperator {
	return func(resolved any, absent bool, expected any) bool {
		if absent {
			return false
		}
		resolvedText, expectedText := workflowTextOf(resolved), workflowTextOf(expected)
		switch direction {
		case "starts":
			return strings.HasPrefix(resolvedText, expectedText)
		default:
			return strings.HasSuffix(resolvedText, expectedText)
		}
	}
}

func workflowCompareEmpty(resolved any, absent bool, expected any) bool {
	if absent {
		return true // absence is the emptiest a variable can be
	}
	switch value := resolved.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	default:
		// The object case goes through asWorkflowObject rather than a `case map[string]any` arm:
		// Object is a named type, so an Object boxed in `any` — which is exactly what a start node's
		// output is, since it wraps the run input — never matches that arm and would fall through to
		// "not empty". Reading a nested object out of a node output hits the same trap one level in.
		obj, ok := asWorkflowObject(value)
		if !ok {
			return false
		}
		return len(obj) == 0
	}
}

func workflowCompareExists(resolved any, absent bool, expected any) bool {
	return !absent
}

// workflowCompareNegate flips one operator's verdict. The absent-handling stays the inner
// operator's, so not_exists matches exactly what exists does not.
func workflowCompareNegate(inner workflowConditionOperator) workflowConditionOperator {
	return func(resolved any, absent bool, expected any) bool {
		return !inner(resolved, absent, expected)
	}
}

// workflowTextOf renders a JSON value as the text a text operator compares. A string is itself;
// anything else is its canonical JSON, so comparing against a number or object stays deterministic.
func workflowTextOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return canonicalJSON(value)
}

// workflowJSONEqual compares two JSON values by canonical form (encoding/json sorts map keys), so
// object comparison is order-independent and deterministic. A value JSON cannot represent cannot be
// compared — that is a no-match, not a panic.
func workflowJSONEqual(resolved, expected any) bool {
	a, b := canonicalJSON(resolved), canonicalJSON(expected)
	if a == "" || b == "" {
		return false
	}
	return a == b
}

// workflowCompareNumeric serves the four ordering operators. Only two numbers are orderable; text
// against a number is a no-match rather than an error.
func workflowCompareNumeric(direction string) workflowConditionOperator {
	return func(resolved any, absent bool, expected any) bool {
		if absent {
			return false
		}
		resolvedNum, okA := resolved.(float64)
		expectedNum, okB := expected.(float64)
		if !okA || !okB {
			return false
		}
		switch direction {
		case "greater":
			return resolvedNum > expectedNum
		case "greater_eq":
			return resolvedNum >= expectedNum
		case "less":
			return resolvedNum < expectedNum
		default:
			return resolvedNum <= expectedNum
		}
	}
}

// evaluateWorkflowCondition evaluates a Condition node's cases against the committed node states and
// returns the selected branch id: the first case whose rules match under the case's logic, or `else`
// when none did.
func evaluateWorkflowCondition(node workflowNodeView, states map[string]workflowNodeState) string {
	cases, ok := node.Data["cases"].([]any)
	if !ok {
		return workflowBranchElse
	}
	for _, raw := range cases {
		c, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		rules, _ := c["conditions"].([]any)
		if len(rules) == 0 {
			continue // an authored shell with no rule yet falls through
		}
		matched := false
		if c.S("logic") == "or" {
			for _, raw := range rules {
				if workflowConditionRule(raw, states) {
					matched = true
					break
				}
			}
		} else {
			matched = true
			for _, raw := range rules {
				if !workflowConditionRule(raw, states) {
					matched = false
					break
				}
			}
		}
		if matched {
			return c.S("id")
		}
	}
	return workflowBranchElse
}

// workflowConditionRule resolves a rule's variableSelector and applies its operator. A malformed
// rule (not an object, malformed selector, unknown operator) is a no-match.
func workflowConditionRule(raw any, states map[string]workflowNodeState) bool {
	r, ok := asWorkflowObject(raw)
	if !ok {
		return false
	}
	selector, ok := r["variableSelector"].([]any)
	if !ok || len(selector) == 0 {
		return false
	}
	nodeID, ok := selector[0].(string)
	if !ok || nodeID == "" {
		return false
	}
	state, ok := states[nodeID]
	if !ok || state.Status != "succeeded" {
		return false
	}
	resolved, absent := walkWorkflowOutputPath(state.Output, selector[1:])
	operator, ok := workflowConditionOperators[r.S("operator")]
	if !ok {
		return false
	}
	return operator(resolved, absent, r["value"])
}

// walkWorkflowOutputPath walks a variableSelector's path into one node's output. The first path
// element indexes the output object; deeper elements walk further in. Absent is the honest answer
// for a node whose output is not an object or a path that leaves the object.
func walkWorkflowOutputPath(output Object, path []any) (any, bool) {
	if output == nil {
		return nil, true
	}
	current := any(output)
	for _, element := range path {
		obj, ok := asWorkflowObject(current)
		if !ok {
			return nil, true
		}
		key, ok := element.(string)
		if !ok {
			return nil, true
		}
		value, present := obj[key]
		if !present {
			return nil, true
		}
		current = value
	}
	return current, false
}
