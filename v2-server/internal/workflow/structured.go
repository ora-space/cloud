package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Structured output follows desktop's structured_output.rs: the Agent's final answer must carry
// one JSON object that satisfies the node's schema subset (type, properties, required,
// additionalProperties, items). The raw answer stays the node's output either way.

// ParseStructuredOutput extracts the JSON object from an Agent's final answer and validates it
// against schema. The error message is the one recorded on a structured_output failure.
func ParseStructuredOutput(answer string, schema map[string]any) (map[string]any, error) {
	value, err := extractJSONObject(answer)
	if err != nil {
		return nil, err
	}
	if err := validateStructured(value, schema, "$"); err != nil {
		return nil, err
	}
	return value, nil
}

// extractJSONObject tries, in order: the whole answer as a bare object, the first fence body that
// parses as an object, then the first balanced object embedded in prose. The last fallback
// tolerates models that wrap the object in commentary; the schema still gates its shape.
func extractJSONObject(text string) (map[string]any, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		if v, ok := decodeObject(trimmed); ok {
			return v, nil
		}
	}
	for _, block := range fencedBlocks(trimmed) {
		if v, ok := decodeObject(block); ok {
			return v, nil
		}
	}
	for i, ch := range trimmed {
		if ch != '{' {
			continue
		}
		if end := objectBodyEnd(trimmed[i:]); end > 0 {
			if v, ok := decodeObject(trimmed[i : i+end]); ok {
				return v, nil
			}
		}
	}
	reason := "no JSON object found in the final response"
	if strings.HasPrefix(trimmed, "{") {
		reason = "text is not valid JSON"
	}
	return nil, fmt.Errorf("final assistant output is not a JSON object: %s", reason)
}

// decodeObject parses text that is exactly one JSON object (surrounding whitespace allowed),
// keeping number lexemes as the pool does.
func decodeObject(text string) (map[string]any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || strings.TrimSpace(text[decoder.InputOffset():]) != "" {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	return obj, ok
}

// objectBodyEnd is the byte length of the balanced object opening text, ignoring braces inside
// strings, or 0 when it never closes.
func objectBodyEnd(text string) int {
	depth, inString, escaped := 0, false, false
	for i, ch := range text {
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// fencedBlocks collects every ``` or ~~~ fence body in order; non-JSON fences fail to decode and
// are skipped whatever their language tag.
func fencedBlocks(text string) []string {
	blocks := []string{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		marker, ok := fenceMarker(lines[i])
		if !ok {
			continue
		}
		var block strings.Builder
		for i++; i < len(lines); i++ {
			if m, closing := fenceMarker(lines[i]); closing && m == marker {
				break
			}
			block.WriteString(lines[i])
			block.WriteString("\n")
		}
		blocks = append(blocks, block.String())
	}
	return blocks
}

func fenceMarker(line string) (rune, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, false
	}
	marker := rune(trimmed[0])
	count := len(trimmed) - len(strings.TrimLeft(trimmed, string(marker)))
	return marker, count >= 3
}

// validateStructured checks a value against the schema subset, reporting the first violation
// with a JSON-path-like location.
func validateStructured(value any, schema map[string]any, path string) error {
	if expected, ok := schema["type"].(string); ok {
		matches := (expected == "null" && value == nil) || valueMatches(value, expected)
		if !matches {
			return fmt.Errorf("structured output at %s has type %s, expected %s", path, typeLabel(value), expected)
		}
	}
	obj, isObject := value.(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if isObject {
		for _, name := range sortedKeys(properties) {
			if v, present := obj[name]; present {
				child, _ := properties[name].(map[string]any)
				if err := validateStructured(v, child, path+"."+name); err != nil {
					return err
				}
			}
		}
		required, _ := schema["required"].([]any)
		for _, r := range required {
			name, _ := r.(string)
			if _, present := obj[name]; !present {
				return fmt.Errorf("structured output at %s is missing required property %s", path, name)
			}
		}
		if schema["additionalProperties"] == false {
			for _, name := range sortedKeys(obj) {
				if _, declared := properties[name]; !declared {
					return fmt.Errorf("structured output at %s has unexpected property %s", path, name)
				}
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		if array, isArray := value.([]any); isArray {
			for i, item := range array {
				if err := validateStructured(item, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// StructuredExample builds a valid sample value from the schema vocabulary, for the prompt's
// output contract, as desktop's structured_output_example.
func StructuredExample(schema map[string]any) any {
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		properties, _ := schema["properties"].(map[string]any)
		result := map[string]any{}
		for name, p := range properties {
			child, _ := p.(map[string]any)
			result[name] = StructuredExample(child)
		}
		return result
	case "array":
		if items, ok := schema["items"].(map[string]any); ok {
			return []any{StructuredExample(items)}
		}
		return []any{}
	case "string", "any", "array[any]":
		if typ == "array[any]" {
			return []any{"text"}
		}
		return "text"
	case "number":
		return json.Number("1.5")
	case "integer":
		return json.Number("1")
	case "boolean":
		return true
	case "secret":
		return "token-value"
	case "file":
		return map[string]any{"kind": "workspace_file", "path": "docs/example.txt"}
	case "array[string]":
		return []any{"text"}
	case "array[number]":
		return []any{json.Number("1.5")}
	case "array[object]":
		return []any{map[string]any{}}
	case "array[boolean]":
		return []any{true}
	case "array[file]":
		return []any{map[string]any{"kind": "workspace_file", "path": "docs/example.txt"}}
	}
	return nil
}
