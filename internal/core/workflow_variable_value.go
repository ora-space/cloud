package core

import (
	"math"
	"strings"
)

// The workflow variable value vocabulary, ported from desktop's variable_value.rs so a graph's global
// declarations are accepted or refused on Cloud by the same rules desktop applies. The publish gate
// and the run's ambient pool read these. Most normalizations here are the accept/reject decision, not
// a rewrite: the frozen snapshot keeps the authored value verbatim. The one rewrite is the publish
// gate's normalizeWorkflowGlobals, which stores the normalized file and array[file] reference so a
// condition reads the same shape desktop's variable pool produces.

// workflowSupportedVariableType reports whether a wire type belongs to the workflow variable
// vocabulary, desktop's is_supported_variable_type.
func workflowSupportedVariableType(valueType string) bool {
	switch valueType {
	case "string", "number", "integer", "boolean", "secret", "file", "object", "any",
		"array", "array[string]", "array[number]", "array[object]", "array[boolean]",
		"array[file]", "array[any]":
		return true
	}
	return false
}

// normalizeWorkflowValue reports whether a value is acceptable for its declared type, mirroring
// desktop's normalize_workflow_value: file and array[file] also migrate a legacy path string into a
// canonical workspace-file reference, and every other type checks the value against the type. The
// normalized value is returned for the file types so normalizeWorkflowGlobals can store it; callers
// that only validate discard it.
func normalizeWorkflowValue(value any, valueType string) (any, bool) {
	switch valueType {
	case "file":
		return normalizeWorkflowFileReference(value)
	case "array[file]":
		list, ok := value.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, len(list))
		for i, item := range list {
			reference, ok := normalizeWorkflowFileReference(item)
			if !ok {
				return nil, false
			}
			out[i] = reference
		}
		return out, true
	default:
		if workflowValueMatchesType(value, valueType) {
			return value, true
		}
		return nil, false
	}
}

// workflowValueMatchesType checks a value against one workflow variable declaration, desktop's
// workflow_value_matches_type. Cloud decodes every document number as float64, so "integer" is the
// whole-float64 test; the Go-native int spellings a hand-built document can carry are read through
// workflowNumberOf the same way.
func workflowValueMatchesType(value any, valueType string) bool {
	switch valueType {
	case "string", "secret":
		_, ok := value.(string)
		return ok
	case "file":
		return isWorkflowFileReference(value)
	case "integer":
		return workflowIsInteger(value)
	case "number":
		return workflowIsNumber(value)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := asWorkflowObject(value)
		return ok
	case "any":
		return true
	case "array", "array[any]":
		_, ok := value.([]any)
		return ok
	}
	if itemType, ok := strings.CutPrefix(valueType, "array["); ok && strings.HasSuffix(itemType, "]") {
		list, isList := value.([]any)
		if !isList {
			return false
		}
		elementType := itemType[:len(itemType)-1]
		for _, item := range list {
			if !workflowValueMatchesType(item, elementType) {
				return false
			}
		}
		return true
	}
	return false
}

// workflowIsNumber reports whether the value is a finite JSON number, the frontend's Number.isFinite
// test for a "number" declaration.
func workflowIsNumber(value any) bool {
	n, ok := workflowNumberOf(value)
	return ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}

// workflowIsInteger reports whether the value is a whole JSON number, the frontend's Number.isInteger
// test for an "integer" declaration. Cloud cannot tell 5 from 5.0 (both decode as float64), so any
// integral float64 counts, which is also what the frontend accepts.
func workflowIsInteger(value any) bool {
	n, ok := workflowNumberOf(value)
	return ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n == math.Trunc(n)
}

// normalizeWorkflowFileReference converts a legacy path string or canonical object into a safe
// workspace-relative reference, mirroring the frontend's normalizeFileReference (which itself mirrors
// desktop's PortableRelativePath). It rejects empty, absolute, drive-letter and parent-traversal
// paths and Windows reserved device names, and canonicalizes separators to "/".
func normalizeWorkflowFileReference(value any) (Object, bool) {
	rawPath, ok := readWorkflowFilePath(value)
	if !ok || rawPath == "" || workflowIsDrivePrefixed(rawPath) {
		return nil, false
	}
	if strings.HasPrefix(rawPath, "/") || strings.HasPrefix(rawPath, "\\") {
		return nil, false
	}
	segments := []string{}
	for _, segment := range strings.FieldsFunc(rawPath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." || workflowIsDrivePrefixed(segment) || isWindowsReservedName(segment) {
			return nil, false
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return nil, false
	}
	return Object{"kind": "workspace_file", "path": strings.Join(segments, "/")}, true
}

// isWorkflowFileReference reports whether the value is already a canonical safe workspace-relative
// file reference, desktop's is_file_reference: an object with exactly kind and path, a workspace_file
// kind, and a path in canonical relative form.
func isWorkflowFileReference(value any) bool {
	object, ok := asWorkflowObject(value)
	if !ok || len(object) != 2 {
		return false
	}
	if object.S("kind") != "workspace_file" {
		return false
	}
	path, ok := object["path"].(string)
	if !ok {
		return false
	}
	normalized, ok := normalizeWorkflowFileReference(object)
	return ok && normalized.S("path") == path
}

// readWorkflowFilePath reads the path out of either accepted file-reference spelling: a legacy path
// string, or a canonical object whose kind is workspace_file.
func readWorkflowFilePath(value any) (string, bool) {
	if text, ok := value.(string); ok {
		return text, true
	}
	object, ok := asWorkflowObject(value)
	if !ok || object.S("kind") != "workspace_file" {
		return "", false
	}
	path, ok := object["path"].(string)
	return path, ok
}

// workflowIsDrivePrefixed reports whether a path begins with a Windows drive letter, the frontend's
// /^[A-Za-z]:/ test.
func workflowIsDrivePrefixed(s string) bool {
	return len(s) >= 2 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1] == ':'
}

// isWindowsReservedName mirrors the frontend's portable-path rejection for Win32 device aliases: the
// segment's stem before its first dot, with trailing dots and spaces trimmed, must not be a reserved
// device name.
func isWindowsReservedName(segment string) bool {
	stem := segment
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && len(stem) == 4 {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}
