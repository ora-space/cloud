package modelgateway

import "encoding/json"

func object(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func array(raw json.RawMessage) bool {
	var value []json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

// validProtocolJSON permits provider extensions but requires the successful wire envelope.
// Nonstandard HTTP200 diagnostics must not become SDK parse errors that include private bodies.
func validProtocolJSON(data []byte, protocol string) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil || value == nil {
		return false
	}
	if protocol == "anthropic-messages" {
		var kind string
		return json.Unmarshal(value["type"], &kind) == nil && kind == "message" && array(value["content"])
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(value["choices"], &choices) != nil || len(choices) == 0 {
		return false
	}
	for _, choice := range choices {
		if !object(choice["message"]) {
			return false
		}
	}
	return true
}

func validProtocolEvent(data []byte, protocol string) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil || value == nil {
		return false
	}
	if protocol == "openai-completions" {
		var choices []map[string]json.RawMessage
		if json.Unmarshal(value["choices"], &choices) != nil || choices == nil {
			return false
		}
		for _, choice := range choices {
			if !object(choice["delta"]) {
				return false
			}
		}
		return true
	}
	var kind string
	if json.Unmarshal(value["type"], &kind) != nil {
		return false
	}
	switch kind {
	case "message_start":
		return object(value["message"])
	case "content_block_start":
		return object(value["content_block"])
	case "content_block_delta", "message_delta":
		return object(value["delta"])
	case "content_block_stop", "message_stop", "ping":
		return true
	default:
		return false
	}
}
