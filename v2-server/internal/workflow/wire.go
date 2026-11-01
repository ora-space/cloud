package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// reader checks the typed wire shape without discarding unknown authoring fields.
// Children share the first diagnostic, so a nested failure retains its field path.
type reader struct {
	value              object
	code, nodeID, path string
	err                *error
}

func newReader(value object, code, nodeID, path string) reader {
	var err error
	return reader{value: value, code: code, nodeID: nodeID, path: path, err: &err}
}

func (r reader) fail(field, reason string) {
	if *r.err == nil {
		path := r.path
		if field != "" {
			if path != "" {
				path += "."
			}
			path += field
		}
		*r.err = invalid(r.code, r.nodeID, path, reason)
	}
}

func (r reader) at(value any, path string) reader {
	child := reader{code: r.code, nodeID: r.nodeID, path: path, err: r.err}
	child.value, _ = value.(object)
	if child.value == nil {
		child.fail("", "expected an object")
	}
	return child
}

func (r reader) child(key string) reader { return r.at(r.value[key], r.path+"."+key) }

func (r reader) str(key string) string {
	v := r.value[key]
	if v == nil {
		return ""
	} // serde Option<String>
	s, ok := v.(string)
	if !ok {
		r.fail(key, "expected a string")
	}
	return s
}

func (r reader) requiredString(key string) string {
	if _, ok := r.value[key].(string); !ok {
		r.fail(key, "expected a string")
	}
	return r.str(key)
}

func (r reader) boolean(key string) bool {
	v, exists := r.value[key]
	if !exists {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		r.fail(key, "expected a boolean")
	}
	return b
}

func (r reader) array(key string) []any {
	v, exists := r.value[key]
	if !exists {
		return nil
	} // serde default Vec; explicit null is not a Vec.
	items, ok := v.([]any)
	if !ok {
		r.fail(key, "expected an array")
	}
	return items
}

func (r reader) requiredArray(key string) []any {
	if _, exists := r.value[key]; !exists {
		r.fail(key, "required array is missing")
	}
	return r.array(key)
}

func (r reader) elements(key string) []reader {
	result := []reader{}
	for i, v := range r.array(key) {
		result = append(result, r.at(v, fmt.Sprintf("%s.%s[%d]", r.path, key, i)))
	}
	return result
}

func (r reader) strings(key string) []string {
	result := []string{}
	for _, v := range r.array(key) {
		s, ok := v.(string)
		if !ok {
			r.fail(key, "expected string array elements")
		}
		result = append(result, s)
	}
	return result
}

func uintNumber(v any, max uint64) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseUint(string(n), 10, 64)
	return value, err == nil && value <= max
}

func (r reader) integer(key string, min, max uint64) uint64 {
	value, ok := uintNumber(r.value[key], max)
	if !ok || value < min {
		r.fail(key, fmt.Sprintf("expected an integer in %d..%d", min, max))
	}
	return value
}

func (r reader) only(keys ...string) {
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for _, key := range sortedKeys(r.value) {
		if !allowed[key] {
			r.fail(key, "unknown configuration field")
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func selector(parts []string) bool    { return len(parts) >= 2 && parts[0] != "" && parts[1] != "" }
func qualified(parts []string) string { return strings.Join(parts[:2], ".") }

func (r reader) selector(key string) []string {
	parts := r.strings(key)
	if !selector(parts) {
		r.fail(key, "expected a node ID and root variable")
	}
	return parts
}
