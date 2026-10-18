package modelgateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// JSON numbers reach browser clients; larger integers cannot remain exact across that boundary.
const maxObservedTokens int64 = 1<<53 - 1

// observedUsage contains only allowlisted numeric counters. Provider text, metadata and headers
// never enter the attempt ledger. Nil means unreported or invalid, including on a completed call.
type observedUsage struct {
	input, output, total, cacheRead, cacheWrite, reasoning *int64
	seen, invalid                                          bool
}

// usageObserver consumes cumulative snapshots rather than adding them: repeated final frames and
// Anthropic message_delta updates must not multiply usage. It owns no response body after a call.
type usageObserver struct {
	protocol   string
	usage      observedUsage
	terminal   bool
	finalUsage bool
}

func jsonObject(raw []byte) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	return value
}

func jsonString(raw []byte) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// replaceCounter preserves absence across partial snapshots but clears a malformed new value;
// retaining an earlier cumulative value in that case would pretend it is the final counter.
func (u *observedUsage) replaceCounter(target **int64, fields map[string]json.RawMessage, key string) {
	raw, present := fields[key]
	if !present {
		return
	}
	*target = nil
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 || value > maxObservedTokens {
		u.invalid = true
		return
	}
	u.seen = true
	*target = &value
}

func (o *usageObserver) observeUsage(raw []byte) {
	fields := jsonObject(raw)
	if fields == nil {
		if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			o.usage.invalid = true
		}
		return
	}
	// OpenAI usage is one complete request snapshot. Optional counters absent from a newer
	// snapshot must not inherit an older value and appear to have final coverage.
	if o.protocol == "openai-completions" {
		o.usage = observedUsage{invalid: o.usage.invalid}
		o.finalUsage = false
	}
	u := &o.usage
	if o.protocol == "anthropic-messages" {
		u.replaceCounter(&u.input, fields, "input_tokens")
		u.replaceCounter(&u.output, fields, "output_tokens")
		u.replaceCounter(&u.cacheRead, fields, "cache_read_input_tokens")
		u.replaceCounter(&u.cacheWrite, fields, "cache_creation_input_tokens")
		u.replaceCounter(&u.reasoning, jsonObject(fields["output_tokens_details"]), "thinking_tokens")
		return
	}
	u.replaceCounter(&u.input, fields, "prompt_tokens")
	u.replaceCounter(&u.output, fields, "completion_tokens")
	u.replaceCounter(&u.total, fields, "total_tokens")
	u.replaceCounter(&u.cacheRead, jsonObject(fields["prompt_tokens_details"]), "cached_tokens")
	u.replaceCounter(&u.cacheWrite, jsonObject(fields["prompt_tokens_details"]), "cache_write_tokens")
	u.replaceCounter(&u.reasoning, jsonObject(fields["completion_tokens_details"]), "reasoning_tokens")
}

// observeJSON is called only after the existing success-envelope validation. For a non-streamed
// response semantic output becomes observable only once the complete JSON body has been received.
func (o *usageObserver) observeJSON(data []byte) bool {
	value := jsonObject(data)
	o.observeUsage(value["usage"])
	o.terminal = true
	o.finalUsage = true
	if o.protocol == "anthropic-messages" {
		var blocks []json.RawMessage
		_ = json.Unmarshal(value["content"], &blocks)
		for _, block := range blocks {
			if anthropicOutput(jsonObject(block)) {
				return true
			}
		}
		return false
	}
	return openAIOutput(value, "message")
}

// eventPayload follows the same SSE normalization as the forwarding validator. Comments and ping
// frames contain no semantic output and cannot start the first-output timer.
func eventPayload(event []byte) []byte {
	var data []string
	normalized := strings.TrimPrefix(string(event), "\uFEFF")
	normalized = strings.ReplaceAll(strings.ReplaceAll(normalized, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(normalized, "\n") {
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	return []byte(strings.Join(data, "\n"))
}

func (o *usageObserver) observeEvent(event []byte) bool {
	payload := eventPayload(event)
	if o.protocol == "openai-completions" && bytes.Equal(payload, []byte("[DONE]")) {
		o.terminal = true
		return false
	}
	value := jsonObject(payload)
	if o.protocol == "openai-completions" {
		o.observeUsage(value["usage"])
		if jsonObject(value["usage"]) != nil {
			o.finalUsage = openAIFinalUsage(value)
		}
		return openAIOutput(value, "delta")
	}
	switch jsonString(value["type"]) {
	case "message_start":
		o.observeUsage(jsonObject(value["message"])["usage"])
	case "message_delta":
		o.observeUsage(value["usage"])
		if _, ok := jsonObject(value["usage"])["output_tokens"]; ok {
			o.finalUsage = true
		}
	case "message_stop":
		o.terminal = true
	case "content_block_start":
		return anthropicOutput(jsonObject(value["content_block"]))
	case "content_block_delta":
		return anthropicOutput(jsonObject(value["delta"]))
	}
	return false
}

// OpenRouter's documented accounting frame repeats terminal choices with a content-free delta,
// unlike OpenAI's empty choices array. Both are final usage candidates; [DONE] remains a separate
// requirement, so an interrupted stream cannot become complete from this frame alone.
func openAIFinalUsage(value map[string]json.RawMessage) bool {
	fields := jsonObject(value["usage"])
	if fields["prompt_tokens"] == nil || fields["completion_tokens"] == nil {
		return false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(value["choices"], &choices) != nil || choices == nil {
		return false
	}
	for _, choice := range choices {
		switch jsonString(choice["finish_reason"]) {
		case "stop", "length", "tool_calls", "content_filter":
		default:
			return false
		}
		if jsonObject(choice["delta"]) == nil {
			return false
		}
	}
	return !openAIOutput(value, "delta")
}

func openAIOutput(value map[string]json.RawMessage, field string) bool {
	var choices []map[string]json.RawMessage
	_ = json.Unmarshal(value["choices"], &choices)
	for _, choice := range choices {
		message := jsonObject(choice[field])
		if jsonString(message["content"]) != "" || jsonString(message["refusal"]) != "" || jsonString(message["reasoning"]) != "" || jsonString(message["reasoning_content"]) != "" {
			return true
		}
		var calls []map[string]json.RawMessage
		_ = json.Unmarshal(message["tool_calls"], &calls)
		for _, call := range calls {
			if functionOutput(jsonObject(call["function"])) {
				return true
			}
		}
		if functionOutput(jsonObject(message["function_call"])) {
			return true
		}
	}
	return false
}

func functionOutput(value map[string]json.RawMessage) bool {
	return jsonString(value["name"]) != "" || jsonString(value["arguments"]) != ""
}

func anthropicOutput(value map[string]json.RawMessage) bool {
	switch jsonString(value["type"]) {
	case "text", "text_delta":
		return jsonString(value["text"]) != ""
	case "thinking", "thinking_delta":
		return jsonString(value["thinking"]) != ""
	case "input_json_delta":
		return jsonString(value["partial_json"]) != ""
	case "tool_use", "server_tool_use":
		return jsonString(value["name"]) != ""
	}
	return false
}
