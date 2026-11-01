package workflow

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

func supportedType(t string) bool {
	switch t {
	case "string", "number", "integer", "boolean", "secret", "file", "object", "any", "array",
		"array[string]", "array[number]", "array[object]", "array[boolean]", "array[file]", "array[any]":
		return true
	}
	return false
}

// valueMatches includes desktop's legacy file normalization admission, without
// modifying the original JSON. Runtime value normalization is a separate concern.
func valueMatches(v any, t string) bool {
	switch t {
	case "any":
		return true
	case "string", "secret":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		// serde_json represents negative zero as f64, not an integer.
		if !ok || n == "-0" {
			return false
		}
		_, signedErr := strconv.ParseInt(string(n), 10, 64)
		_, unsignedErr := strconv.ParseUint(string(n), 10, 64)
		return signedErr == nil || unsignedErr == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(object)
		return ok
	case "file":
		path, ok := v.(string)
		if ref, isObject := v.(object); isObject {
			if ref["kind"] != "workspace_file" {
				return false
			}
			path, ok = ref["path"].(string)
		}
		return ok && portableFile(path)
	default:
		items, ok := v.([]any)
		if !ok {
			return false
		}
		if t == "array" || t == "array[any]" {
			return true
		}
		if !strings.HasPrefix(t, "array[") || !strings.HasSuffix(t, "]") {
			return false
		}
		for _, item := range items {
			if !valueMatches(item, t[6:len(t)-1]) {
				return false
			}
		}
		return true
	}
}

// portableFile follows ora-utils PortableRelativePath, independent of the host OS.
func portableFile(path string) bool {
	if strings.ContainsRune(path, 0) || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return false
	}
	nonempty := false
	for _, segment := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "." {
			continue
		}
		if segment == ".." {
			return false
		}
		if len(segment) >= 2 && segment[1] == ':' && ((segment[0] >= 'a' && segment[0] <= 'z') || (segment[0] >= 'A' && segment[0] <= 'Z')) {
			return false
		}
		stem := strings.ToUpper(strings.TrimRight(strings.SplitN(segment, ".", 2)[0], " ."))
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" {
			return false
		}
		if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return false
		}
		nonempty = true
	}
	return nonempty
}

func validateGlobals(raw object, hasLoops bool) (map[string]string, error) {
	r := newReader(raw, "invalid_global_variables", "", "")
	key := "global_variables"
	if _, exists := raw["globalVariables"]; exists {
		key = "globalVariables"
		if _, duplicate := raw["global_variables"]; duplicate && !hasLoops {
			r.fail(key, "duplicate global variable field")
		}
	}
	catalog := map[string]string{}
	for _, v := range r.elements(key) {
		name, typ := strings.TrimSpace(v.str("name")), v.str("valueType")
		if !strings.Contains(name, ".") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
			v.fail("name", "expected a qualified variable name")
		}
		if _, exists := catalog[name]; exists {
			v.fail("name", "duplicate global variable")
		}
		if !supportedType(typ) {
			v.fail("valueType", "unsupported variable type")
		}
		value := v.value["value"]
		if value == nil && name != "sys.workflow_id" && name != "sys.timestamp" {
			v.fail("value", "global requires an initial value")
		}
		if value != nil && !valueMatches(value, typ) {
			v.fail("value", "value does not match declared type")
		}
		catalog[name] = typ
	}
	catalog["sys.workflow_id"], catalog["sys.timestamp"] = "string", "number"
	return catalog, *r.err
}

func startFields(r reader) map[string]string {
	result := map[string]string{}
	for _, v := range r.elements("inputVariables") {
		name, typ := strings.TrimSpace(v.str("name")), v.str("valueType")
		if name == "" || strings.Contains(name, ".") {
			v.fail("name", "variable names must be non-empty and cannot contain dots")
		}
		if _, exists := result[name]; exists {
			v.fail("name", "duplicate Start variable")
		}
		if !supportedType(typ) {
			v.fail("valueType", "unsupported variable type")
		}
		v.str("displayName")
		v.boolean("required")
		field := v.str("fieldType")
		if v.value["fieldType"] == nil {
			switch typ {
			case "string", "secret":
				field = "text-input"
			case "number", "integer":
				field = "number"
			case "boolean":
				field = "checkbox"
			case "file":
				field = "file"
			case "array[file]":
				field = "file-list"
			default:
				field = "json"
			}
		} else if !fieldProduces(field, typ) {
			v.fail("fieldType", "field does not produce the declared variable type")
		}
		options := v.strings("options")
		seen := map[string]bool{}
		for i, option := range options {
			options[i] = strings.TrimSpace(option)
			if field == "select" && (options[i] == "" || seen[options[i]]) {
				v.fail("options", "select options must be non-empty and unique")
			}
			seen[options[i]] = true
		}
		if (field == "select" && len(options) == 0) || (field != "select" && len(options) != 0) {
			v.fail("options", "options are required only for select fields")
		}
		var max uint64
		if v.value["maxLength"] != nil {
			max = v.integer("maxLength", 1, ^uint64(0))
			if field != "text-input" && field != "paragraph" {
				v.fail("maxLength", "only text fields support maxLength")
			}
		}
		if value := v.value["value"]; value != nil {
			if !valueMatches(value, typ) {
				v.fail("value", "value does not match declared type")
			}
			if text, ok := value.(string); ok {
				if max > 0 && uint64(utf8.RuneCountInString(text)) > max {
					v.fail("value", "initial value exceeds maxLength")
				}
				if field == "select" && !seen[text] {
					v.fail("value", "initial value is not a select option")
				}
			}
		}
		result[name] = typ
	}
	return result
}

func fieldProduces(field, typ string) bool {
	switch field {
	case "text-input", "paragraph", "select":
		return typ == "string"
	case "number":
		return typ == "number"
	case "checkbox":
		return typ == "boolean"
	case "file":
		return typ == "file"
	case "file-list":
		return typ == "array[file]"
	case "json":
		return typ == "object" || typ == "any" || typ == "array" || (strings.HasPrefix(typ, "array[") && typ != "array[file]" && supportedType(typ))
	}
	return false
}
