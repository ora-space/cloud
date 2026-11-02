package workflow

import "strings"

type declaration struct{ typ, writer string }

// validateDeclarations mirrors desktop's base-then-topological derived catalog.
// This is static publication typing; no variable values or execution state live here.
func (s *scope) validateDeclarations(globals map[string]string) error {
	catalog := map[string]declaration{}
	for key, typ := range globals {
		catalog[key] = declaration{typ: typ, writer: strings.SplitN(key, ".", 2)[0]}
	}
	declare := func(n *node, name, typ string) {
		key := n.id + "." + name
		if _, exists := catalog[key]; !exists {
			catalog[key] = declaration{typ: typ, writer: n.id}
		}
	}
	for _, n := range s.nodes {
		switch n.kind {
		case "start":
			declare(n, "input", "string")
			for name, typ := range n.inputs {
				declare(n, name, typ)
			}
		case "condition", "iteration", "aggregator":
		default:
			declare(n, "output", "string")
		}
		if n.kind == "agent" && n.structured {
			declare(n, "structured_output", "object")
		}
		if n.kind == "loop" {
			cfg, _ := (Node{data: n.data}).loopConfig()
			for _, output := range cfg.Outputs {
				declare(n, output.Name, "any")
			}
		}
	}
	for _, n := range s.order {
		if n.kind == "aggregator" {
			typ := ""
			ancestors := s.ancestors(n.id)
			for _, parts := range n.aggregator {
				if s.byID[parts[0]] != nil && !ancestors[parts[0]] {
					return invalid("invalid_aggregator", n.id, "aggregatorConfig.variables", "producer must be a static predecessor")
				}
				def, exists := catalog[qualified(parts)]
				if !exists {
					return invalid("invalid_aggregator", n.id, "aggregatorConfig.variables", "selector is not declared")
				}
				if typ != "" && typ != def.typ {
					return invalid("invalid_aggregator", n.id, "aggregatorConfig.variables", "selectors must have the same declared type")
				}
				typ = def.typ
			}
			declare(n, "output", typ)
		}
		if n.kind == "iteration" {
			source, exists := catalog[qualified(n.iterator)]
			if !exists || !strings.HasPrefix(source.typ, "array") {
				return invalid("invalid_iteration", n.id, "iterationConfig.iteratorSelector", "iterator must be a declared array")
			}
			if writer := s.byID[source.writer]; writer != nil && writer.owner == n.id {
				return invalid("invalid_iteration", n.id, "iterationConfig.iteratorSelector", "iterator must be outside its own region")
			}
			member := s.byID[n.collect[0]]
			collected, exists := catalog[qualified(n.collect)]
			if member == nil || member.owner != n.id || !exists {
				return invalid("invalid_iteration", n.id, "iterationConfig.collectSelector", "collect must be a declared root variable of a region member")
			}
			item := "any"
			switch source.typ {
			case "array[string]", "array[number]", "array[object]", "array[boolean]", "array[file]":
				item = source.typ[6 : len(source.typ)-1]
			}
			declare(n, "item", item)
			declare(n, "index", "number")
			declare(n, "output", "array["+collected.typ+"]")
			declare(n, "entries", "array[object]")
			declare(n, "failed_count", "number")
		}
	}
	return nil
}
