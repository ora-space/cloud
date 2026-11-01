package workflow

import "fmt"

func validateNode(n *node) error {
	r := newReader(n.data, "invalid_node", n.id, "data")
	for _, key := range []string{"title", "description", "instruction", "input"} {
		r.str(key)
	}
	if n.kind == "start" {
		n.inputs = startFields(r)
	} else {
		for _, v := range r.elements("inputVariables") {
			for _, key := range []string{"name", "displayName", "fieldType", "valueType"} {
				v.str(key)
			}
			v.boolean("required")
			v.strings("options")
			if v.value["maxLength"] != nil {
				v.integer("maxLength", 0, ^uint64(0))
			}
		}
	}
	if n.data["agentConfig"] != nil {
		validateAgent(r.child("agentConfig"), n)
	}
	for _, c := range r.elements("cases") {
		c.str("id")
		c.str("logic")
		if n.kind == "condition" {
			c.requiredString("id")
			validateCondition(c, n, false)
		} else {
			for _, rule := range c.elements("conditions") {
				rule.strings("variableSelector")
				rule.str("operator")
			}
		}
	}
	for _, binding := range r.elements("outputs") {
		binding.str("name")
		parts := binding.strings("variableSelector")
		// Desktop keeps legacy output nodes: incomplete output rows are ignored at parse.
		if n.kind == "output" && binding.value["name"] != nil && selector(parts) {
			n.bindings = append(n.bindings, parts)
			n.selectors = append(n.selectors, parts)
		}
	}
	if n.data["aggregatorConfig"] != nil {
		c := r.child("aggregatorConfig")
		seen := map[string]bool{}
		for i, value := range c.array("variables") {
			item := c.at(object{"selector": value}, fmt.Sprintf("%s.variables[%d]", c.path, i))
			parts := item.strings("selector")
			if n.kind != "aggregator" {
				continue
			}
			if !selector(parts) || len(parts) != 2 {
				item.fail("selector", "aggregator requires root selectors")
			} else {
				key := qualified(parts)
				if seen[key] {
					item.fail("selector", "duplicate aggregator selector")
				}
				seen[key] = true
				n.aggregator = append(n.aggregator, parts)
				n.selectors = append(n.selectors, parts)
			}
		}
	}
	if n.kind == "aggregator" && len(n.aggregator) == 0 {
		r.fail("aggregatorConfig", "aggregator requires at least one selector")
	}
	if n.data["iterationConfig"] != nil {
		c := r.child("iterationConfig")
		for _, key := range []string{"iteratorSelector", "collectSelector"} {
			if c.value[key] != nil {
				c.strings(key)
			}
		}
		strategy := c.str("errorStrategy")
		if c.value["errorStrategy"] != nil && strategy != "fail" && strategy != "continue" {
			c.fail("errorStrategy", "expected fail or continue")
		}
		if c.value["maxIterations"] != nil {
			c.integer("maxIterations", 0, 1<<32-1)
		}
		if n.kind == "iteration" {
			n.iterator, n.collect = c.selector("iteratorSelector"), c.selector("collectSelector")
			if len(n.collect) > 2 {
				c.fail("collectSelector", "collect requires a root variable")
			}
			if c.value["maxIterations"] != nil {
				c.integer("maxIterations", 1, 1<<32-1)
			}
			n.selectors = append(n.selectors, n.iterator, n.collect)
		}
	} else if n.kind == "iteration" {
		r.fail("iterationConfig", "iteration requires configuration")
	}
	if n.kind == "loop" {
		validateLoopConfig(r.child("loopConfig"), n)
	}
	return *r.err
}

// validateCondition also handles Loop.until; runtime evaluation and start-time
// case/edge validation deliberately remain outside publication validation.
func validateCondition(c reader, n *node, loop bool) {
	logic := c.str("logic")
	if c.value["logic"] != nil && logic != "and" && logic != "or" {
		c.fail("logic", "expected and or or")
	}
	for _, rule := range c.elements("conditions") {
		parts := rule.selector("variableSelector")
		op := rule.str("operator")
		switch op {
		case "equals", "not_equals", "contains", "not_contains", "starts_with", "ends_with", "empty", "not_empty",
			"greater_than", "greater_than_or_equal", "less_than", "less_than_or_equal", "is", "is_not", "exists", "not_exists":
		default:
			rule.fail("operator", "unknown comparison operator")
		}
		n.selectors = append(n.selectors, parts)
		if loop {
			n.loopRefs = append(n.loopRefs, parts)
		} else {
			n.bindings = append(n.bindings, parts)
		}
		if text, ok := rule.value["value"].(string); ok {
			n.templates = append(n.templates, text)
		}
	}
}

func validateLoopConfig(c reader, n *node) {
	c.only("maxIterations", "maxIterationsBehavior", "variables", "until", "outputs")
	c.integer("maxIterations", 1, 100)
	if _, exists := c.value["maxIterationsBehavior"]; exists {
		behavior := c.requiredString("maxIterationsBehavior")
		if behavior != "fail" && behavior != "succeed" {
			c.fail("maxIterationsBehavior", "expected fail or succeed")
		}
	}
	until := c.child("until")
	until.only("logic", "conditions")
	until.requiredString("logic")
	until.requiredArray("conditions")
	validateCondition(until, n, true)
	c.requiredArray("variables")
	c.requiredArray("outputs")
	seen := map[string]bool{}
	for _, v := range c.elements("variables") {
		v.only("name", "valueType", "initial", "feedback")
		name, typ := v.requiredString("name"), v.requiredString("valueType")
		if !loopName(name) || seen[name] {
			v.fail("name", "invalid or duplicate carried variable name")
		}
		seen[name] = true
		if !supportedType(typ) {
			v.fail("valueType", "unsupported variable type")
		}
		initial := v.child("initial")
		switch initial.requiredString("kind") {
		case "constant":
			initial.only("kind", "value")
			if _, exists := initial.value["value"]; !exists {
				initial.fail("value", "constant initial value is required")
			}
			if !valueMatches(initial.value["value"], typ) {
				initial.fail("value", "initial value does not match carried type")
			}
		case "variable":
			initial.only("kind", "selector")
			parts := initial.selector("selector")
			n.initial = append(n.initial, parts)
			n.selectors = append(n.selectors, parts)
		default:
			initial.fail("kind", "expected constant or variable")
		}
		parts := v.selector("feedback")
		n.loopRefs = append(n.loopRefs, parts)
		n.selectors = append(n.selectors, parts)
	}
	seen = map[string]bool{}
	for _, v := range c.elements("outputs") {
		v.only("name", "variableSelector")
		name := v.requiredString("name")
		if !loopName(name) || seen[name] {
			v.fail("name", "invalid or duplicate Loop output name")
		}
		seen[name] = true
		parts := v.selector("variableSelector")
		n.loopRefs = append(n.loopRefs, parts)
		n.selectors = append(n.selectors, parts)
	}
}
