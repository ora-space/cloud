package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// Adapted from desktop condition.rs tests: the same pools, cases and expected branches.

func reviewPool(t *testing.T) *VariablePool {
	t.Helper()
	p := newPool()
	p.Declare("review.structured_output", "object", "review")
	p.Declare("review.text", "string", "review")
	if err := p.Set("review.structured_output", "review", map[string]any{"approved": true, "score": json.Number("92")}); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("review.text", "review", "审查完成"); err != nil {
		t.Fatal(err)
	}
	return &p
}

func rule(selector string, operator string, value any) conditionRule {
	return conditionRule{selector: strings.Split(selector, "."), operator: operator, value: value}
}

func oneCase(logic string, rules ...conditionRule) []conditionCase {
	return []conditionCase{{id: "c", logic: logic, rules: rules}}
}

func TestConditionPicksTheFirstMatchingCaseElseFallsBack(t *testing.T) {
	cases := []conditionCase{
		{id: "approved", logic: "and", rules: []conditionRule{rule("review.structured_output.approved", "is", true)}},
		{id: "score-hi", logic: "and", rules: []conditionRule{rule("review.structured_output.score", "greater_than", json.Number("200"))}},
	}
	if got, err := evaluateCondition(cases, reviewPool(t)); err != nil || got != "approved" {
		t.Fatalf("got %q %v", got, err)
	}
	rejected := reviewPool(t)
	_ = rejected.Set("review.structured_output", "review", map[string]any{"approved": false, "score": json.Number("10")})
	if got, err := evaluateCondition(cases, rejected); err != nil || got != ElseBranch {
		t.Fatalf("got %q %v, want else", got, err)
	}
	// A new IF shell without rules falls through instead of matching vacuously.
	if got, err := evaluateCondition([]conditionCase{{id: "case-1", logic: "and"}}, reviewPool(t)); err != nil || got != ElseBranch {
		t.Fatalf("empty case: %q %v", got, err)
	}
}

func TestConditionOperators(t *testing.T) {
	pool := reviewPool(t)
	pool.Declare("start.limit", "number", "start")
	pool.Declare("start.token", "string", "start")
	pool.Declare("start.tags", "array[string]", "start")
	_ = pool.Set("start.limit", "start", json.Number("90"))
	_ = pool.Set("start.token", "start", "审查")
	_ = pool.Set("start.tags", "start", []any{"a", "b"})
	matches := map[string][]conditionCase{
		"and needs all":            oneCase("and", rule("review.text", "not_empty", nil), rule("review.structured_output.score", "greater_than", json.Number("90"))),
		"or needs any":             oneCase("or", rule("review.text", "contains", "不存在"), rule("review.structured_output.score", "greater_than_or_equal", json.Number("92"))),
		"equals":                   oneCase("and", rule("review.text", "equals", "审查完成")),
		"contains":                 oneCase("and", rule("review.text", "contains", "完成")),
		"starts_with":              oneCase("and", rule("review.text", "starts_with", "审查")),
		"ends_with":                oneCase("and", rule("review.text", "ends_with", "完成")),
		"not_equals":               oneCase("and", rule("review.text", "not_equals", "别的")),
		"number across lexemes":    oneCase("and", rule("review.structured_output.score", "greater_than_or_equal", json.Number("92.0"))),
		"less_than":                oneCase("and", rule("review.structured_output.score", "less_than", json.Number("100"))),
		"numeric not_equals":       oneCase("and", rule("review.structured_output.score", "not_equals", json.Number("0"))),
		"placeholder keeps type":   oneCase("and", rule("review.structured_output.score", "greater_than", "{{#start.limit#}}")),
		"mixed template is string": oneCase("and", rule("review.text", "contains", "{{#start.token#}}完")),
		"array membership":         oneCase("and", rule("start.tags", "contains", "b")),
		"unset tolerated":          oneCase("and", rule("start.unset", "not_exists", nil)),
	}
	pool.Declare("start.unset", "string", "start")
	for name, cases := range matches {
		if got, err := evaluateCondition(cases, pool); err != nil || got != "c" {
			t.Errorf("%s: got %q %v, want c", name, got, err)
		}
	}
}

func TestConditionFailuresNeverGuessABranch(t *testing.T) {
	pool := reviewPool(t)
	pool.Declare("agent-a.text", "string", "agent-a")
	failures := map[string]struct {
		cases []conditionCase
		want  string
	}{
		"unset value":         {oneCase("and", rule("agent-a.text", "equals", "x")), "agent-a.text has not been assigned yet"},
		"string op on object": {oneCase("and", rule("review.structured_output", "contains", "approved")), "cannot compare variable review.structured_output of type object"},
		"undeclared":          {oneCase("and", rule("ghost.text", "exists", nil)), "not declared: ghost.text"},
		"missing operand":     {oneCase("and", rule("review.text", "equals", nil)), "against a comparison value"},
		// Every rule is evaluated before combining: the invalid second rule fails the case.
		"later invalid rule": {oneCase("or", rule("review.text", "not_empty", nil), rule("agent-a.text", "equals", "x")), "has not been assigned"},
	}
	for name, c := range failures {
		if got, err := evaluateCondition(c.cases, pool); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %q %v, want error containing %q", name, got, err, c.want)
		}
	}
}

func TestConditionCasesCompileFromTheWire(t *testing.T) {
	g, err := ParseGraph(`{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"cond","data":{"kind":"condition","cases":[
	 {"id":"approved","logic":"or","conditions":[{"variableSelector":["start","input"],"operator":"not_empty"},{"variableSelector":["start","input"],"operator":"is","value":null}]},
	 {"id":"plain","conditions":[]}]}}],"edges":[{"source":"start","target":"cond"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := g.Node("cond")
	cases, err := n.conditionCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].id != "approved" || cases[0].logic != "or" || len(cases[0].rules) != 2 || cases[1].logic != "and" {
		t.Fatalf("cases = %+v", cases)
	}
	// A JSON null operand reads as no operand, as serde's Option does.
	if cases[0].rules[1].value != nil {
		t.Fatalf("null operand = %#v", cases[0].rules[1].value)
	}
}
