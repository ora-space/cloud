package core

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Workflow Condition node semantics, ported from desktop's engine/condition.rs so a branch decision
// means the same thing on both sides (specs#99 D1 条件④: the scheduler boundary mirrors desktop's
// ports, and desktop is the reference implementation).
//
// The variable pool is the run's committed node outputs: a rule's variableSelector is
// [nodeId, root, ...nested]. nodeId names a prior node, root is one variable that node's kind
// declares (start: `input` plus each declared input variable; agent: `output`, plus
// `structured_output` when the node declares an output contract; output: `output`; condition: none),
// and nested walks object fields past the root.
//
// Today only the start node actually writes roots. Agent and output roots are declared by the
// contract but stay unassigned until cloud#70/#74 completes the variable passing, so a selector on
// them reads the same declared-but-unassigned state desktop has before a node produces its output.
// Cloud's runtime has no declaration set — the states map cannot tell an unassigned variable from one
// the graph never declared — so the publish gate refuses selectors naming an unknown node or a root
// its kind does not expose (`condition_selector_undeclared`), which desktop's pool reports as
// `Undeclared` and fails. What reaches the evaluator is therefore either assigned or genuinely
// unassigned, and a variable a prior node has not produced is absent.
//
// Absence is not a universal no-match: desktop's existence and emptiness operators classify it, and
// every other operator fails the node and the run rather than silently falling through to else. The
// same applies to a value whose type the operator cannot compare. That failure is the point of the
// parity work (circus#210): a condition the author could not have meant must be visible, not routed.
//
// The expected value may also be a Dify-style template. `{{#node.var#}}` alone keeps the bound JSON
// type (so numeric and boolean comparisons stay typed); a template mixed with literal text renders
// as a string. Both forms resolve every placeholder eagerly, so an unset variable fails the node
// before a branch is chosen.

// workflowConditionError is one runtime condition failure. Code is the stable identifier callers and
// tests key on; the message names the selector and operator the way desktop's ConditionError does so
// the run viewer points at the value that needs fixing.
type workflowConditionError struct {
	Code   string
	Detail string
}

func (e *workflowConditionError) Error() string { return e.Detail }

func workflowConditionFailure(code, format string, args ...any) *workflowConditionError {
	return &workflowConditionError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// workflowSelector is a resolved variableSelector. Root is the variable name inside the node's
// output, not part of an opaque path, which is what makes Cloud's selector the same shape desktop's
// variable pool uses.
type workflowSelector struct {
	nodeID string
	root   string
	nested []string
}

func (s workflowSelector) qualified() string {
	parts := make([]string, 0, 2+len(s.nested))
	parts = append(parts, s.nodeID, s.root)
	parts = append(parts, s.nested...)
	return strings.Join(parts, ".")
}

// parseWorkflowSelector reads a wire variableSelector. The editor sends JSON arrays, so []any is the
// production shape; []string is accepted too because hand-built documents and tests spell selectors
// naturally that way. A selector needs both a node and a root: one segment would name a whole node
// output, which desktop refuses.
func parseWorkflowSelector(raw any) (workflowSelector, bool) {
	var parts []string
	switch value := raw.(type) {
	case []any:
		parts = make([]string, len(value))
		for i, element := range value {
			text, ok := element.(string)
			if !ok {
				return workflowSelector{}, false
			}
			parts[i] = text
		}
	case []string:
		parts = value
	default:
		return workflowSelector{}, false
	}
	return buildWorkflowSelector(parts)
}

// buildWorkflowSelector validates selector parts from either a wire array or a template placeholder.
func buildWorkflowSelector(parts []string) (workflowSelector, bool) {
	if len(parts) < 2 {
		return workflowSelector{}, false
	}
	for _, part := range parts {
		if part == "" {
			return workflowSelector{}, false
		}
	}
	return workflowSelector{nodeID: parts[0], root: parts[1], nested: parts[2:]}, true
}

// resolveWorkflowSelector reads one selector out of committed node states. The boolean reports
// whether the variable is assigned; absence is an ordinary state the operators classify. A nested
// path that cannot be walked is an error, matching desktop's variable pool: once the root exists, a
// missing or non-object field means the selector names something the author did not produce.
func resolveWorkflowSelector(selector workflowSelector, states map[string]workflowNodeState) (value any, present bool, err error) {
	state, ok := states[selector.nodeID]
	if !ok || state.Status != "succeeded" {
		return nil, false, nil
	}
	output, ok := asWorkflowObject(state.Output)
	if !ok {
		return nil, false, nil
	}
	value, present = output[selector.root]
	if !present {
		return nil, false, nil
	}
	for _, segment := range selector.nested {
		object, ok := asWorkflowObject(value)
		if !ok {
			return nil, false, workflowConditionFailure("condition_invalid_path",
				"condition variable %s is not an object, so its path cannot be resolved", selector.qualified())
		}
		value, present = object[segment]
		if !present {
			return nil, false, workflowConditionFailure("condition_invalid_path",
				"condition variable %s has no object path segment %s", selector.qualified(), segment)
		}
	}
	return value, true, nil
}

// workflowConditionOperand is one rule's fully resolved inputs: the variable, the expected value and
// the names both are reported with. hasExpected is false for a missing `value` and for an explicit
// JSON null alike, matching desktop's Option<Value>: serde reads null as None, and a comparison
// operator then reports the missing operand instead of comparing against null.
type workflowConditionOperand struct {
	actual      any
	present     bool
	expected    any
	hasExpected bool
	selector    string
	operator    string
}

// workflowConditionOperator evaluates one comparison rule; an error fails the node and the run.
type workflowConditionOperator func(*workflowConditionOperand) (bool, error)

// workflowConditionOperators is desktop's full wire operator set (condition.rs
// ComparisonOperator::parse). `is`/`is_not` are the same comparisons as `equals`/`not_equals`.
//
// Only the keys are wire spellings. The direction labels the comparator helpers are built with
// ("starts", "greater_eq", …) name one comparison on the Go side and are not operators a document
// may write: the gate reads this map, so a document spelling one is refused as an unknown operator
// rather than quietly evaluated as the comparison it feeds.
var workflowConditionOperators = map[string]workflowConditionOperator{
	"equals":                workflowCompareEquals,
	"is":                    workflowCompareEquals,
	"not_equals":            workflowCompareNotEquals,
	"is_not":                workflowCompareNotEquals,
	"contains":              workflowCompareContains,
	"not_contains":          workflowCompareNotContains,
	"starts_with":           workflowCompareTextOrder(true),
	"ends_with":             workflowCompareTextOrder(false),
	"empty":                 workflowCompareEmpty,
	"not_empty":             workflowCompareNotEmpty,
	"exists":                workflowCompareExists,
	"not_exists":            workflowCompareNotExists,
	"greater_than":          workflowCompareNumeric("greater"),
	"greater_than_or_equal": workflowCompareNumeric("greater_eq"),
	"less_than":             workflowCompareNumeric("less"),
	"less_than_or_equal":    workflowCompareNumeric("less_eq"),
}

// operands requires an assigned variable and an expected value. Existence and emptiness are the
// only operators that read an absent variable; every other one fails instead of treating absence as
// false, so an unset variable can never silently choose the else branch.
func (o *workflowConditionOperand) operands() (actual, expected any, err error) {
	if !o.present {
		return nil, nil, workflowConditionFailure("condition_variable_unset",
			"condition variable %s has not been assigned yet", o.selector)
	}
	if !o.hasExpected {
		return nil, nil, workflowCompareTypeMismatch(o, "a comparison value")
	}
	return o.actual, o.expected, nil
}

func workflowCompareTypeMismatch(o *workflowConditionOperand, expectedLabel string) error {
	return workflowConditionFailure("condition_type_mismatch",
		"operator %s cannot compare variable %s of type %s against %s",
		o.operator, o.selector, workflowTypeLabel(o.actual), expectedLabel)
}

func workflowCompareEquals(o *workflowConditionOperand) (bool, error) {
	actual, expected, err := o.operands()
	if err != nil {
		return false, err
	}
	return workflowJSONEqual(actual, expected), nil
}

func workflowCompareNotEquals(o *workflowConditionOperand) (bool, error) {
	matched, err := workflowCompareEquals(o)
	if err != nil {
		return false, err
	}
	return !matched, nil
}

func workflowCompareContains(o *workflowConditionOperand) (bool, error) {
	actual, expected, err := o.operands()
	if err != nil {
		return false, err
	}
	switch value := actual.(type) {
	case string:
		needle, ok := expected.(string)
		if !ok {
			return false, workflowCompareTypeMismatch(o, workflowTypeLabel(expected))
		}
		return strings.Contains(value, needle), nil
	case []any:
		for _, item := range value {
			if workflowJSONEqual(item, expected) {
				return true, nil
			}
		}
		return false, nil
	default:
		// desktop's contains is defined for strings (substring) and arrays (element equality) only;
		// an object or scalar has no containment relation to guess at.
		return false, workflowCompareTypeMismatch(o, workflowTypeLabel(expected))
	}
}

func workflowCompareNotContains(o *workflowConditionOperand) (bool, error) {
	matched, err := workflowCompareContains(o)
	if err != nil {
		return false, err
	}
	return !matched, nil
}

// workflowCompareTextOrder serves both text-position operators: starts is prefix, ends is suffix.
// desktop requires both sides to be strings; a number or object is a type error, not text.
func workflowCompareTextOrder(starts bool) workflowConditionOperator {
	return func(o *workflowConditionOperand) (bool, error) {
		actual, expected, err := o.operands()
		if err != nil {
			return false, err
		}
		actualText, okActual := actual.(string)
		expectedText, okExpected := expected.(string)
		if !okActual || !okExpected {
			return false, workflowCompareTypeMismatch(o, workflowTypeLabel(expected))
		}
		if starts {
			return strings.HasPrefix(actualText, expectedText), nil
		}
		return strings.HasSuffix(actualText, expectedText), nil
	}
}

func workflowCompareEmpty(o *workflowConditionOperand) (bool, error) {
	return workflowValueEmpty(o.actual, o.present), nil
}

func workflowCompareNotEmpty(o *workflowConditionOperand) (bool, error) {
	return !workflowValueEmpty(o.actual, o.present), nil
}

func workflowCompareExists(o *workflowConditionOperand) (bool, error) {
	return o.present, nil
}

func workflowCompareNotExists(o *workflowConditionOperand) (bool, error) {
	return !o.present, nil
}

// workflowValueEmpty is desktop's is_empty: unassigned, null, and an empty string/array/object are
// empty; booleans and numbers are not.
func workflowValueEmpty(value any, present bool) bool {
	if !present {
		return true
	}
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	default:
		// The object case goes through asWorkflowObject rather than a `case map[string]any` arm:
		// Object is a named type, so an Object boxed in `any` — which is exactly what a start node's
		// output is, since it wraps the run input — never matches that arm and would fall through to
		// "not empty". Reading a nested object out of a node output hits the same trap one level in.
		object, ok := asWorkflowObject(typed)
		if !ok {
			return false
		}
		return len(object) == 0
	}
}

// workflowCompareNumeric serves the four ordering operators. Both sides must be JSON numbers;
// text against a number is a type error, exactly as desktop's compare_numbers answers None.
func workflowCompareNumeric(direction string) workflowConditionOperator {
	return func(o *workflowConditionOperand) (bool, error) {
		actual, expected, err := o.operands()
		if err != nil {
			return false, err
		}
		left, okLeft := workflowNumberOf(actual)
		right, okRight := workflowNumberOf(expected)
		if !okLeft || !okRight {
			return false, workflowCompareTypeMismatch(o, workflowTypeLabel(expected))
		}
		switch direction {
		case "greater":
			return left > right, nil
		case "greater_eq":
			return left >= right, nil
		case "less":
			return left < right, nil
		default:
			return left <= right, nil
		}
	}
}

// workflowNumberOf reads any Go numeric representation JSON decoding can leave in a document.
func workflowNumberOf(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case int32:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint64:
		return float64(number), true
	case uint32:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// workflowTypeLabel names a JSON value's type for a type-mismatch message. Cloud decodes every number
// as float64, so it cannot tell 92 from 92.0 and labels any integral value "integer"; desktop's
// type_label calls only is_i64/is_u64 "integer" and 92.0 "number". The difference is confined to the
// message text — comparison and equality are unaffected.
func workflowTypeLabel(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	}
	if number, ok := workflowNumberOf(value); ok {
		if number == math.Trunc(number) {
			return "integer"
		}
		return "number"
	}
	if _, ok := asWorkflowObject(value); ok {
		return "object"
	}
	return "value"
}

// workflowJSONEqual compares two JSON values by canonical form (encoding/json sorts map keys), so
// object comparison is order-independent and deterministic. Numeric spellings 2 and 2.0 render the
// same way, which is desktop's equal() across integer and float representations.
//
// Two edge cases differ from desktop's Value equality: inside a container desktop keeps integer and
// float distinct (`{a:2}` and `{a:2.0}` are not equal), while the canonical form unifies them; and
// desktop's as_f64 comparison treats -0.0 and 0 as equal, while their canonical forms "-0" and "0"
// differ. Top-level numeric comparison matches on both sides.
func workflowJSONEqual(actual, expected any) bool {
	a, b := canonicalJSON(actual), canonicalJSON(expected)
	if a == "" || b == "" {
		return false
	}
	return a == b
}

// evaluateWorkflowCondition evaluates a Condition node's cases against the committed node states and
// returns the selected branch id: the first case whose rules match under its case logic, or `else`
// when none did. A malformed document or an unevaluable rule is an error: the publish gate refuses
// such documents, and a snapshot that predates the gate must fail loudly rather than route wrongly.
func evaluateWorkflowCondition(node workflowNodeView, states map[string]workflowNodeState) (string, error) {
	raw, present := node.Data["cases"]
	if !present || raw == nil {
		return workflowBranchElse, nil
	}
	cases, ok := raw.([]any)
	if !ok {
		return "", workflowConditionFailure("condition_invalid_config", "condition cases must be a list")
	}
	for _, item := range cases {
		c, ok := asWorkflowObject(item)
		if !ok {
			return "", workflowConditionFailure("condition_invalid_config", "condition case must be an object")
		}
		id := c.S("id")
		if id == "" {
			return "", workflowConditionFailure("condition_invalid_config", "condition case is missing an id")
		}
		logic := c.S("logic")
		if logic != "" && logic != "and" && logic != "or" {
			return "", workflowConditionFailure("condition_invalid_config", "condition case has unknown logic %s", logic)
		}
		rawRules, present := c["conditions"]
		if !present || rawRules == nil {
			continue // an authored shell with no rule yet falls through
		}
		rules, ok := rawRules.([]any)
		if !ok {
			return "", workflowConditionFailure("condition_invalid_config", "condition case conditions must be a list")
		}
		if len(rules) == 0 {
			continue
		}
		// Evaluate every rule before combining them, as desktop does: an or-case whose first rule
		// matches must still report an unset variable in its second rule instead of skipping it.
		verdicts := make([]bool, len(rules))
		for i, rawRule := range rules {
			verdict, err := workflowConditionRule(rawRule, states)
			if err != nil {
				return "", err
			}
			verdicts[i] = verdict
		}
		matched := logic != "or"
		for _, verdict := range verdicts {
			if logic == "or" {
				matched = matched || verdict
			} else {
				matched = matched && verdict
			}
		}
		if matched {
			return id, nil
		}
	}
	return workflowBranchElse, nil
}

// workflowConditionRule resolves a rule's variableSelector and expected value, then applies its
// operator. A malformed rule is an error, not a no-match.
func workflowConditionRule(raw any, states map[string]workflowNodeState) (bool, error) {
	r, ok := asWorkflowObject(raw)
	if !ok {
		return false, workflowConditionFailure("condition_invalid_rule", "condition rule must be an object")
	}
	selector, ok := parseWorkflowSelector(r["variableSelector"])
	if !ok {
		return false, workflowConditionFailure("condition_invalid_rule",
			"condition rule selector is not a node id and root variable")
	}
	operatorName := r.S("operator")
	operator, ok := workflowConditionOperators[operatorName]
	if !ok {
		return false, workflowConditionFailure("condition_invalid_rule",
			"condition rule has unknown operator %s", operatorName)
	}
	actual, present, err := resolveWorkflowSelector(selector, states)
	if err != nil {
		return false, err
	}
	expected, hasExpected, err := resolveWorkflowExpected(r, states)
	if err != nil {
		return false, err
	}
	return operator(&workflowConditionOperand{
		actual:      actual,
		present:     present,
		expected:    expected,
		hasExpected: hasExpected,
		selector:    selector.qualified(),
		operator:    operatorName,
	})
}
