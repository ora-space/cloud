package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Field paths contain only schema-owned names and indices, never operator-provided values.
func invalidField(field, reason string) error {
	if field == "" {
		field = "configuration"
	}
	return fmt.Errorf("invalid deployment egress configuration: %s: %s", field, reason)
}

func indexedField(field string, index int) string {
	return field + "[" + strconv.Itoa(index) + "]"
}

func decodingError(err error) error {
	var typed *json.UnmarshalTypeError
	if errors.As(err, &typed) {
		var field string
		for _, part := range strings.Split(typed.Field, ".") {
			if index, err := strconv.Atoi(part); err == nil {
				field = indexedField(field, index)
			} else if field == "" {
				field = part
			} else {
				field += "." + part
			}
		}
		return invalidField(field, "invalid type")
	}
	if name, err := strconv.Unquote(strings.TrimPrefix(err.Error(), "json: unknown field ")); err == nil && schemaKey(name) {
		return invalidField(name, "field not allowed here")
	}
	return invalidField("configuration", "invalid JSON structure")
}

// uniqueJSON rejects duplicate keys, null fields and excessive nesting before typed decoding.
// Unknown keys are not echoed: arbitrary user-controlled keys can themselves contain secrets.
func uniqueJSON(d *json.Decoder, field string, depth int) error {
	if depth > 16 {
		return invalidField(field, "excessive nesting")
	}
	t, err := d.Token()
	if err != nil {
		return invalidField(field, "invalid JSON")
	}
	if t == nil {
		return invalidField(field, "null not allowed")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return invalidField(field, "invalid JSON key")
			}
			s, ok := key.(string)
			if !ok || !schemaKey(s) {
				return invalidField(field, "unknown field")
			}
			child := s
			if field != "" {
				child = field + "." + s
			}
			if seen[s] {
				return invalidField(child, "duplicate key")
			}
			seen[s] = true
			if err := uniqueJSON(d, child, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for index := 0; d.More(); index++ {
			if err := uniqueJSON(d, indexedField(field, index), depth+1); err != nil {
				return err
			}
		}
	default:
		return invalidField(field, "unexpected delimiter")
	}
	if _, err := d.Token(); err != nil {
		return invalidField(field, "invalid JSON delimiter")
	}
	return nil
}

// encoding/json otherwise accepts case-insensitive aliases for tagged fields.
func schemaKey(key string) bool {
	switch key {
	case "schemaVersion", "mode", "resolver", "targets", "topology", "transport", "address", "host", "port", "allowPrivate", "allowedCidrs", "staticAddresses", "clusterCidrs", "hostAddresses":
		return true
	default:
		return false
	}
}
