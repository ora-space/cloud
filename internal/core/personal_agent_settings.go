package core

import (
	"bytes"
	"encoding/json"
	"math"

	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// resolveAgentSettings validates user values against the already compiled declaration. Only
// declared scalar fields cross this boundary; defaults are resolved before values are frozen.
func resolveAgentSettings(declaration *pluginmarket.CompiledAgentConfiguration, values Object) Object {
	require(values != nil && len(jsonText(values)) <= 256<<10, 400, "invalid_agent_settings")
	known := make(map[string]bool, len(declaration.Settings))
	out := Object{}
	for _, setting := range declaration.Settings {
		known[setting.ID] = true
		value, present := values[setting.ID]
		if !present && setting.Default != nil {
			// The compiler preserves numeric literals in its private representation. Normalize
			// through JSON so supplied values and defaults use the same scalar validation.
			var err error
			value, err = agentSettingDefault(setting.Default)
			require(err == nil, 500, "internal_error")
			present = true
		}
		if !present {
			require(!setting.Required, 400, "agent_setting_required")
			continue
		}
		require(validAgentSettingValue(setting.SettingType, value), 400, "invalid_agent_setting_type")
		out[setting.ID] = value
	}
	for key := range values {
		require(known[key], 400, "unknown_agent_setting")
	}
	return out
}

// agentSettingDefault converts the declaration compiler's JSON scalar to its public value.
func agentSettingDefault(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeAgentJSONValue(encoded)
}

// decodeAgentJSONValue preserves numeric literals across defaults, storage, run freezing and
// idempotent replay. Decoding into float64 here would silently round valid integers above
// JavaScript's safe range.
func decodeAgentJSONValue(encoded []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	err := decoder.Decode(&decoded)
	return decoded, err
}

// validAgentSettingValue rejects nulls, objects and coercions even for optional fields.
func validAgentSettingValue(kind string, value any) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		var number float64
		switch v := value.(type) {
		case json.Number:
			parsed, err := v.Float64()
			if err != nil {
				return false
			}
			number = parsed
		case float64:
			number = v
		case int:
			number = float64(v)
		case int64:
			number = float64(v)
		default:
			return false
		}
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return false
	}
}
