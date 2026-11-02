package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// VariableDefinition declares one pool variable's type and its only writer, as desktop's
// WorkflowVariableDefinition.
type VariableDefinition struct {
	ValueType string `json:"valueType"`
	Writer    string `json:"writer"`
	MaxLength *int   `json:"maxLength,omitempty"`
}

// VariablePool is a run's typed variable store, serialized exactly as desktop's
// WorkflowVariablePool. Values are decoded with json.Number so numbers keep their lexeme.
type VariablePool struct {
	Revision uint64                        `json:"revision"`
	Catalog  map[string]VariableDefinition `json:"catalog"`
	Values   map[string]any                `json:"values"`
}

// PoolError reports a rejected pool read or write. Kind is undeclared, writer, type, length,
// path_missing or path_not_object.
type PoolError struct {
	Kind, Selector, Detail string
}

func (e *PoolError) Error() string {
	switch e.Kind {
	case "undeclared":
		return "workflow variable is not declared: " + e.Selector
	case "writer":
		return "workflow variable is owned by another writer: " + e.Selector
	case "type":
		return fmt.Sprintf("workflow variable value does not match declared type %s: %s", e.Detail, e.Selector)
	case "length":
		return fmt.Sprintf("workflow variable value exceeds maximum length %s: %s", e.Detail, e.Selector)
	case "path_missing":
		return fmt.Sprintf("workflow variable has no object path segment %s: %s", e.Detail, e.Selector)
	default:
		return fmt.Sprintf("workflow variable is not an object, so path %s cannot be resolved: %s", e.Detail, e.Selector)
	}
}

func newPool() VariablePool {
	return VariablePool{Catalog: map[string]VariableDefinition{}, Values: map[string]any{}}
}

// NewPool builds the run's catalog and initial values from the root graph, mirroring
// WorkflowVariablePool::from_graph: globals, then base node variables, then derived types.
func NewPool(g *Graph) VariablePool {
	p := newPool()
	globals, _ := g.doc.raw["globalVariables"].([]any)
	if _, exists := g.doc.raw["globalVariables"]; !exists {
		globals, _ = g.doc.raw["global_variables"].([]any)
	}
	for _, item := range globals {
		v, _ := item.(object)
		name, _ := v["name"].(string)
		name = strings.TrimSpace(name)
		typ, _ := v["valueType"].(string)
		writer, _, _ := strings.Cut(name, ".")
		p.Declare(name, typ, writer)
		if v["value"] != nil {
			p.Values[name] = v["value"]
		}
	}
	p.Declare("sys.workflow_id", "string", "sys")
	p.Declare("sys.timestamp", "number", "sys")
	for _, n := range g.root.nodes {
		switch n.kind {
		case "start", "condition", "iteration", "aggregator":
		default:
			p.Declare(n.id+".output", "string", n.id)
		}
		view := g.view(n)
		if n.kind == "loop" {
			cfg, _ := view.loopConfig()
			for _, output := range cfg.Outputs {
				p.Declare(n.id+"."+output.Name, "any", n.id)
			}
		}
		switch n.kind {
		case "start":
			p.Declare(n.id+".input", "string", n.id)
			for _, v := range view.StartVariables() {
				key := n.id + "." + v.Name
				p.Declare(key, v.ValueType, n.id)
				def := p.Catalog[key]
				def.MaxLength = v.MaxLength
				p.Catalog[key] = def
				if v.Value != nil {
					if value, ok := normalizeValue(v.Value, v.ValueType); ok {
						p.Values[key] = value
					}
				}
			}
		case "agent":
			if n.structured {
				p.Declare(n.id+".structured_output", "object", n.id)
			}
		}
	}
	// Derived Aggregator and Iteration types reuse the publication catalog, which already
	// resolved them topologically; publication guaranteed they are consistent.
	for _, n := range g.root.order {
		if n.kind != "aggregator" && n.kind != "iteration" {
			continue
		}
		catalog := map[string]declaration{}
		for key, def := range p.Catalog {
			catalog[key] = declaration{typ: def.ValueType, writer: def.Writer}
		}
		if n.kind == "aggregator" && len(n.aggregator) != 0 {
			if def, ok := catalog[qualified(n.aggregator[0])]; ok {
				p.Declare(n.id+".output", def.typ, n.id)
			}
		}
		if n.kind == "iteration" {
			source := catalog[qualified(n.iterator)].typ
			item := "any"
			switch source {
			case "array[string]", "array[number]", "array[object]", "array[boolean]", "array[file]":
				item = source[6 : len(source)-1]
			}
			p.Declare(n.id+".item", item, n.id)
			p.Declare(n.id+".index", "number", n.id)
			p.Declare(n.id+".output", "array["+catalog[qualified(n.collect)].typ+"]", n.id)
			p.Declare(n.id+".entries", "array[object]", n.id)
			p.Declare(n.id+".failed_count", "number", n.id)
		}
	}
	return p
}

// Declare adds a definition unless the selector is already declared (first declaration wins).
func (p *VariablePool) Declare(selector, valueType, writer string) {
	if _, exists := p.Catalog[selector]; !exists {
		p.Catalog[selector] = VariableDefinition{ValueType: valueType, Writer: writer}
	}
}

// Resolve reads a declared variable and an optional nested object path. A declared but
// unassigned variable returns (nil, false, nil).
func (p *VariablePool) Resolve(parts []string) (any, bool, error) {
	key := qualified(parts)
	if _, declared := p.Catalog[key]; !declared {
		return nil, false, &PoolError{Kind: "undeclared", Selector: key}
	}
	value, assigned := p.Values[key]
	if !assigned {
		return nil, false, nil
	}
	for _, segment := range parts[2:] {
		m, isObject := value.(map[string]any)
		if !isObject {
			return nil, false, &PoolError{Kind: "path_not_object", Selector: key, Detail: strings.Join(parts[2:], ".")}
		}
		if value, assigned = m[segment]; !assigned {
			return nil, false, &PoolError{Kind: "path_missing", Selector: key, Detail: segment}
		}
	}
	return value, true, nil
}

// Set writes a variable through its declared writer after type and length checks.
func (p *VariablePool) Set(selector, writer string, value any) error {
	def, declared := p.Catalog[selector]
	if !declared {
		return &PoolError{Kind: "undeclared", Selector: selector}
	}
	if def.Writer != writer {
		return &PoolError{Kind: "writer", Selector: selector}
	}
	normalized, ok := normalizeValue(value, def.ValueType)
	if !ok {
		return &PoolError{Kind: "type", Selector: selector, Detail: def.ValueType}
	}
	if text, isString := normalized.(string); isString && def.MaxLength != nil && utf8.RuneCountInString(text) > *def.MaxLength {
		return &PoolError{Kind: "length", Selector: selector, Detail: fmt.Sprint(*def.MaxLength)}
	}
	p.Values[selector] = normalized
	p.Revision++
	return nil
}

// write sets a variable only when it is declared, reporting whether the pool changed; this is
// desktop's write_pool_variable used by completion bookkeeping.
func (p *VariablePool) write(selector, writer string, value any) (bool, error) {
	if _, declared := p.Catalog[selector]; !declared {
		return false, nil
	}
	return true, p.Set(selector, writer, value)
}

// normalizeValue canonicalizes file references and rejects values outside their type.
func normalizeValue(value any, valueType string) (any, bool) {
	switch valueType {
	case "file":
		return canonicalFile(value)
	case "array[file]":
		items, ok := value.([]any)
		if !ok {
			return nil, false
		}
		result := make([]any, len(items))
		for i, item := range items {
			if result[i], ok = canonicalFile(item); !ok {
				return nil, false
			}
		}
		return result, true
	}
	if !valueMatches(value, valueType) {
		return nil, false
	}
	return value, true
}

func canonicalFile(value any) (any, bool) {
	path, ok := value.(string)
	if ref, isObject := value.(map[string]any); isObject {
		if ref["kind"] != "workspace_file" {
			return nil, false
		}
		path, ok = ref["path"].(string)
	}
	if !ok || !portableFile(path) {
		return nil, false
	}
	parts := []string{}
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment != "." {
			parts = append(parts, segment)
		}
	}
	return map[string]any{"kind": "workspace_file", "path": strings.Join(parts, "/")}, true
}

// RenderTemplate expands {{#node.variable.path#}} from the pool: strings verbatim, other values
// as JSON. Unterminated, malformed, undeclared or unassigned references are errors, never blanks.
func RenderTemplate(template string, p *VariablePool) (string, error) {
	var out strings.Builder
	rest := template
	for {
		before, after, found := strings.Cut(rest, "{{#")
		out.WriteString(before)
		if !found {
			return out.String(), nil
		}
		text, remaining, closed := strings.Cut(after, "#}}")
		if !closed {
			return "", fmt.Errorf("template has an unterminated variable reference")
		}
		parts := strings.Split(text, ".")
		if !selector(parts) {
			return "", fmt.Errorf("template selector %s is not a node id and root variable", text)
		}
		value, assigned, err := p.Resolve(parts)
		if err != nil {
			return "", fmt.Errorf("template variable: %w", err)
		}
		if !assigned {
			return "", fmt.Errorf("template variable %s has not been assigned yet", qualified(parts))
		}
		out.WriteString(renderValue(value))
		rest = remaining
	}
}

func renderValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
