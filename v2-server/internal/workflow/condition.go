package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// The Condition runtime follows desktop's condition.rs: cases are tried in authored order, the
// first case whose rules hold wins, and the implicit else branch is taken when none does.

type conditionRule struct {
	selector []string
	operator string
	// value is the authored comparison operand; nil when the rule has none (JSON null included,
	// as serde reads it into None).
	value any
}

type conditionCase struct {
	id, logic string
	rules     []conditionRule
}

// conditionCases compiles a Condition node's cases like desktop's ConditionConfig::from_wire.
// Publication already checked them, so an error here means the frozen graph is unusable.
func (n Node) conditionCases() ([]conditionCase, error) {
	items, ok := n.data["cases"].([]any)
	if !ok {
		return nil, fmt.Errorf("condition node has no executable cases")
	}
	cases := make([]conditionCase, 0, len(items))
	for _, item := range items {
		raw, _ := item.(object)
		id, hasID := raw["id"].(string)
		if !hasID {
			return nil, fmt.Errorf("condition case is missing an id")
		}
		logic := "and"
		if l, set := raw["logic"].(string); set {
			if l != "and" && l != "or" {
				return nil, fmt.Errorf("condition case has unknown logic %s", l)
			}
			logic = l
		}
		c := conditionCase{id: id, logic: logic}
		rules, _ := raw["conditions"].([]any)
		for _, r := range rules {
			rule, _ := r.(object)
			op, _ := rule["operator"].(string)
			if !comparisonOperator(op) {
				return nil, fmt.Errorf("condition rule has unknown operator %s", op)
			}
			parts := []string{}
			selectorItems, _ := rule["variableSelector"].([]any)
			for _, p := range selectorItems {
				s, _ := p.(string)
				parts = append(parts, s)
			}
			if !selector(parts) {
				return nil, fmt.Errorf("condition rule selector %q is not a node id and root variable", parts)
			}
			c.rules = append(c.rules, conditionRule{selector: parts, operator: op, value: rule["value"]})
		}
		cases = append(cases, c)
	}
	return cases, nil
}

func comparisonOperator(op string) bool {
	switch op {
	case "equals", "not_equals", "contains", "not_contains", "starts_with", "ends_with", "empty", "not_empty",
		"greater_than", "greater_than_or_equal", "less_than", "less_than_or_equal", "is", "is_not", "exists", "not_exists":
		return true
	}
	return false
}

// evaluateCondition returns the selected branch: a case id or ElseBranch. A rule that reads an
// undeclared, unset (for value operators) or mistyped variable fails the node rather than
// guessing a branch.
func evaluateCondition(cases []conditionCase, pool *VariablePool) (string, error) {
	for _, c := range cases {
		// An authored IF/ELIF shell has no rule until one is added; it must fall through instead
		// of matching vacuous `and` semantics and stealing the else path.
		if len(c.rules) == 0 {
			continue
		}
		// Every rule is evaluated before combining, as desktop does, so an invalid rule fails
		// the node even when an earlier rule already decided the case.
		results := make([]bool, len(c.rules))
		for i, rule := range c.rules {
			matched, err := evaluateRule(rule, pool)
			if err != nil {
				return "", err
			}
			results[i] = matched
		}
		matched := c.logic == "and"
		for _, r := range results {
			if c.logic == "and" {
				matched = matched && r
			} else {
				matched = matched || r
			}
		}
		if matched {
			return c.id, nil
		}
	}
	return ElseBranch, nil
}

func evaluateRule(rule conditionRule, pool *VariablePool) (bool, error) {
	key := qualified(rule.selector)
	actual, assigned, err := pool.Resolve(rule.selector)
	if err != nil {
		return false, conditionPoolError(err, rule.operator)
	}
	var expected any
	hasExpected := rule.value != nil
	if hasExpected {
		if expected, err = comparisonValue(rule.value, pool, rule.operator); err != nil {
			return false, err
		}
	}
	switch rule.operator {
	case "exists":
		return assigned, nil
	case "not_exists":
		return !assigned, nil
	case "empty":
		return isEmpty(actual, assigned), nil
	case "not_empty":
		return !isEmpty(actual, assigned), nil
	}
	if !assigned {
		return false, fmt.Errorf("condition variable %s has not been assigned yet", key)
	}
	if !hasExpected {
		return false, typeMismatch(key, rule.operator, typeLabel(actual), "a comparison value")
	}
	mismatched := func() error { return typeMismatch(key, rule.operator, typeLabel(actual), typeLabel(expected)) }
	switch rule.operator {
	case "equals", "is":
		return equalValues(actual, expected), nil
	case "not_equals", "is_not":
		return !equalValues(actual, expected), nil
	case "contains", "not_contains":
		found, ok := containsValue(actual, expected)
		if !ok {
			return false, mismatched()
		}
		return found == (rule.operator == "contains"), nil
	case "starts_with", "ends_with":
		text, ok1 := actual.(string)
		affix, ok2 := expected.(string)
		if !ok1 || !ok2 {
			return false, mismatched()
		}
		if rule.operator == "starts_with" {
			return strings.HasPrefix(text, affix), nil
		}
		return strings.HasSuffix(text, affix), nil
	default: // the ordering operators
		left, ok1 := number(actual)
		right, ok2 := number(expected)
		if !ok1 || !ok2 {
			return false, mismatched()
		}
		switch rule.operator {
		case "greater_than":
			return left > right, nil
		case "greater_than_or_equal":
			return left >= right, nil
		case "less_than":
			return left < right, nil
		default:
			return left <= right, nil
		}
	}
}

// comparisonValue expands a Dify-style operand. A value that is exactly one {{#node.var#}}
// keeps the bound value's JSON type so numeric and boolean compares stay typed; a mixed template
// renders as a string.
func comparisonValue(value any, pool *VariablePool, operator string) (any, error) {
	text, isString := value.(string)
	if !isString || !strings.Contains(text, "{{#") {
		return value, nil
	}
	trimmed := strings.TrimSpace(text)
	if inner, ok := strings.CutPrefix(trimmed, "{{#"); ok {
		if inner, ok = strings.CutSuffix(inner, "#}}"); ok && !strings.Contains(inner, "{{#") && !strings.Contains(inner, "#}}") {
			if parts := strings.Split(inner, "."); selector(parts) {
				resolved, assigned, err := pool.Resolve(parts)
				if err != nil {
					return nil, conditionPoolError(err, operator)
				}
				if !assigned {
					return nil, fmt.Errorf("condition variable %s has not been assigned yet", qualified(parts))
				}
				return resolved, nil
			}
		}
	}
	rendered, err := RenderTemplate(text, pool)
	if err != nil {
		return nil, fmt.Errorf("condition comparison value: %w", err)
	}
	return rendered, nil
}

func conditionPoolError(err error, operator string) error {
	if poolErr, ok := err.(*PoolError); ok && poolErr.Kind == "path_not_object" {
		return typeMismatch(poolErr.Selector, operator, poolErr.Detail, "object")
	}
	return fmt.Errorf("condition reads %w", err)
}

func typeMismatch(selector, operator, actual, expected string) error {
	return fmt.Errorf("operator %s cannot compare variable %s of type %s against %s", operator, selector, actual, expected)
}

// isEmpty counts unassigned, null and empty strings, arrays and objects as empty.
func isEmpty(value any, assigned bool) bool {
	if !assigned || value == nil {
		return true
	}
	switch v := value.(type) {
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case object:
		return len(v) == 0
	}
	return false
}

// number reads a JSON number across integer and float lexemes.
func number(value any) (float64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil
}

// equalValues compares numbers by value (5 equals 5.0) and everything else structurally.
func equalValues(actual, expected any) bool {
	left, ok1 := number(actual)
	right, ok2 := number(expected)
	if ok1 && ok2 {
		return left == right
	}
	return reflect.DeepEqual(actual, expected)
}

// containsValue is substring containment for strings and element membership for arrays; false
// in the second result means the operator does not apply to these types.
func containsValue(actual, expected any) (bool, bool) {
	switch v := actual.(type) {
	case string:
		needle, ok := expected.(string)
		return ok && strings.Contains(v, needle), ok
	case []any:
		for _, item := range v {
			if equalValues(item, expected) {
				return true, true
			}
		}
		return false, true
	}
	return false, false
}

// typeLabel names a value's JSON type in mismatch messages, distinguishing integers.
func typeLabel(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		if valueMatches(v, "integer") {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}
