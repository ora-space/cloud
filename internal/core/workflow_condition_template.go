package core

import "strings"

// Condition expected-value templates, ported from desktop's engine/variable_template.rs and
// engine/condition.rs. A rule's expected value may reference the run's variable pool, and the form
// of the reference decides the result type: a value that is only `{{#node.var#}}` keeps the bound
// JSON type, while a template mixed with literal text always renders a string. Resolution is eager,
// so an unset variable or a malformed placeholder fails the condition instead of comparing text that
// was never expanded.

// resolveWorkflowExpected expands a template expected value. The bool reports whether the rule
// carried a comparison value; a missing `value` and an explicit JSON null are both reported as
// absent, because desktop's WireConditionRule.value is Option<Value> and serde reads a null as None,
// which a comparison operator then reports as a type mismatch rather than comparing against null.
func resolveWorkflowExpected(rule Object, states map[string]workflowNodeState) (expected any, hasExpected bool, err error) {
	raw, present := rule["value"]
	if !present || raw == nil {
		return nil, false, nil
	}
	text, isText := raw.(string)
	if !isText || !strings.Contains(text, "{{#") {
		return raw, true, nil
	}
	if selector, ok := exclusiveWorkflowTemplate(text); ok {
		value, assigned, err := resolveWorkflowSelector(selector, states)
		if err != nil {
			return nil, false, err
		}
		if !assigned {
			return nil, false, workflowConditionFailure("condition_variable_unset",
				"condition variable %s has not been assigned yet", selector.qualified())
		}
		return value, true, nil
	}
	rendered, err := renderWorkflowTemplate(text, states)
	if err != nil {
		return nil, false, err
	}
	return rendered, true, nil
}

// exclusiveWorkflowTemplate reads a string that is exactly one `{{#selector#}}` placeholder. It
// returns false for anything else — including a malformed one, which the mixed renderer then reports
// with the same selector diagnosis desktop's template renderer gives.
func exclusiveWorkflowTemplate(text string) (workflowSelector, bool) {
	trimmed := strings.TrimSpace(text)
	inner, ok := strings.CutPrefix(trimmed, "{{#")
	if !ok {
		return workflowSelector{}, false
	}
	inner, ok = strings.CutSuffix(inner, "#}}")
	if !ok || strings.Contains(inner, "{{#") || strings.Contains(inner, "#}}") {
		return workflowSelector{}, false
	}
	return buildWorkflowSelector(strings.Split(inner, "."))
}

// workflowTemplateSelectors returns the placeholder selectors a `value` template references, so the
// publish gate can hold a template to the same declaredRoots contract as `variableSelector`. It
// reads every `{{#node.root#}}` occurrence the renderer expands; a placeholder that is not a
// well-formed selector is skipped, because the renderer already reports that shape as an error at
// evaluation time and the gate's only concern here is a reference desktop would never declare.
func workflowTemplateSelectors(text string) []workflowSelector {
	var selectors []workflowSelector
	rest := text
	for {
		open := strings.Index(rest, "{{#")
		if open < 0 {
			return selectors
		}
		afterOpen := rest[open+len("{{#"):]
		closeAt := strings.Index(afterOpen, "#}}")
		if closeAt < 0 {
			return selectors
		}
		if selector, ok := buildWorkflowSelector(strings.Split(afterOpen[:closeAt], ".")); ok {
			selectors = append(selectors, selector)
		}
		rest = afterOpen[closeAt+len("#}}"):]
	}
}

// renderWorkflowTemplate renders every placeholder in a mixed template: strings verbatim, every
// other value as JSON. An unterminated placeholder or an invalid selector is an error, never a
// silent blank, matching desktop's render_variable_template.
func renderWorkflowTemplate(text string, states map[string]workflowNodeState) (string, error) {
	var rendered strings.Builder
	rest := text
	for {
		open := strings.Index(rest, "{{#")
		if open < 0 {
			rendered.WriteString(rest)
			return rendered.String(), nil
		}
		rendered.WriteString(rest[:open])
		afterOpen := rest[open+len("{{#"):]
		closeAt := strings.Index(afterOpen, "#}}")
		if closeAt < 0 {
			return "", workflowConditionFailure("condition_invalid_template",
				"condition template has an unterminated variable reference")
		}
		selectorText := afterOpen[:closeAt]
		selector, ok := buildWorkflowSelector(strings.Split(selectorText, "."))
		if !ok {
			return "", workflowConditionFailure("condition_invalid_template",
				"condition template selector %s is not a node id and root variable", selectorText)
		}
		value, assigned, err := resolveWorkflowSelector(selector, states)
		if err != nil {
			return "", err
		}
		if !assigned {
			return "", workflowConditionFailure("condition_variable_unset",
				"condition variable %s has not been assigned yet", selector.qualified())
		}
		if text, ok := value.(string); ok {
			rendered.WriteString(text)
		} else {
			// canonicalJSON renders an integer-valued float as its integer spelling (2.0 -> 2), while
			// desktop's serde_json keeps the JSON number spelling (2.0). Cloud cannot tell 2 from 2.0
			// after decoding, so a mixed template is not byte-identical to desktop for such values;
			// the rendered values still compare equal as JSON.
			rendered.WriteString(canonicalJSON(value))
		}
		rest = afterOpen[closeAt+len("#}}"):]
	}
}
