package modelgateway

import (
	"reflect"
	"testing"
)

func count(value int64) *int64 { return &value }

func TestUsageSnapshotsPreserveCountersWithoutSumming(t *testing.T) {
	for _, test := range []struct {
		name, protocol string
		frames         []string
		want           observedUsage
		terminal       bool
		finalUsage     bool
	}{
		{
			name: "openai repeated final usage", protocol: "openai-completions",
			frames: []string{
				`{"choices":[{"delta":{"content":"hello"}}],"usage":null}`,
				`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":10,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":3}}}`,
				`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":10,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":3}}}`,
				`[DONE]`,
			},
			want: observedUsage{input: count(120), output: count(10), total: count(130), cacheRead: count(80), reasoning: count(3), seen: true}, terminal: true, finalUsage: true,
		},
		{
			name: "latest openai snapshot does not retain earlier details", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":10},"completion_tokens_details":{"reasoning_tokens":2}}}`, `{"choices":[],"usage":{"prompt_tokens":25,"completion_tokens":4}}`, `[DONE]`},
			want:   observedUsage{input: count(25), output: count(4), seen: true}, finalUsage: true, terminal: true,
		},
		{
			name: "latest nonfinal openai snapshot loses final coverage", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3}}`, `{"choices":[{"delta":{}}],"usage":{"prompt_tokens":25,"completion_tokens":4}}`, `[DONE]`},
			want:   observedUsage{input: count(25), output: count(4), seen: true}, terminal: true,
		},
		{
			name: "anthropic cumulative delta", protocol: "anthropic-messages",
			frames: []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":30}}}`,
				`{"type":"message_delta","delta":{},"usage":{"output_tokens":10}}`,
				`{"type":"message_delta","delta":{},"usage":{"output_tokens":15}}`,
				`{"type":"message_delta","delta":{},"usage":{"output_tokens":15}}`,
				`{"type":"message_stop"}`,
			},
			want: observedUsage{input: count(20), output: count(15), cacheRead: count(100), cacheWrite: count(30), seen: true}, terminal: true, finalUsage: true,
		},
		{
			name: "missing stays unknown", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":null}`, `[DONE]`}, terminal: true,
		},
		{
			name: "explicit zero is reported", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`},
			want:   observedUsage{input: count(0), output: count(0), total: count(0), cacheRead: count(0), reasoning: count(0), seen: true}, finalUsage: true,
		},
		{
			name: "invalid counters do not become measured zero", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":-1,"completion_tokens":1.5,"total_tokens":9223372036854775808,"prompt_tokens_details":{"cached_tokens":"8"},"completion_tokens_details":{"reasoning_tokens":true}}}`},
			want:   observedUsage{invalid: true}, finalUsage: true,
		},
		{
			name: "malformed later snapshot clears old counter", protocol: "anthropic-messages",
			frames: []string{`{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1}}}`, `{"type":"message_delta","delta":{},"usage":{"output_tokens":"invalid"}}`},
			want:   observedUsage{input: count(20), seen: true, invalid: true}, finalUsage: true,
		},
		{
			name: "wrong protocol terminal is not completion", protocol: "anthropic-messages",
			frames: []string{`[DONE]`},
		},
		{
			name: "documented cache write and thinking details", protocol: "anthropic-messages",
			frames: []string{`{"type":"message_delta","delta":{},"usage":{"input_tokens":25,"output_tokens":348,"output_tokens_details":{"thinking_tokens":312}}}`, `{"type":"message_stop"}`},
			want:   observedUsage{input: count(25), output: count(348), reasoning: count(312), seen: true}, finalUsage: true, terminal: true,
		},
		{
			name: "openai cache writes", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"prompt_tokens_details":{"cache_write_tokens":10}}}`},
			want:   observedUsage{input: count(20), output: count(3), cacheWrite: count(10), seen: true}, finalUsage: true,
		},
		{
			name: "unsafe browser integer stays unknown", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":9007199254740992,"completion_tokens":3}}`},
			want:   observedUsage{output: count(3), seen: true, invalid: true}, finalUsage: true,
		},
		{
			name: "provider extensions do not become accounting", protocol: "openai-completions",
			frames: []string{`{"choices":[],"usage":{"cost":123,"input_tokens":10,"output_tokens":20,"provider":"secret"}}`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := usageObserver{protocol: test.protocol}
			for _, frame := range test.frames {
				observer.observeEvent([]byte("data: " + frame + "\n\n"))
			}
			if !reflect.DeepEqual(observer.usage, test.want) || observer.terminal != test.terminal || observer.finalUsage != test.finalUsage {
				t.Fatalf("observer=%+v usage=%+v, want usage=%+v terminal=%t final=%t", observer, observer.usage, test.want, test.terminal, test.finalUsage)
			}
		})
	}
}

func TestOpenRouterFinalUsageRequiresAccountingFrameAndTerminal(t *testing.T) {
	// This is the documented OpenRouter accounting shape, with only synthetic counters.
	const usage = `,"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`
	for _, test := range []struct {
		name, choice, availability string
		terminal                   bool
	}{
		{"stop accounting frame", `[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":"stop","native_finish_reason":"stop"}]`, "complete", true},
		{"tool accounting frame", `[{"delta":{},"finish_reason":"tool_calls"}]`, "complete", true},
		{"length accounting frame", `[{"delta":{},"finish_reason":"length"}]`, "complete", true},
		{"filtered accounting frame", `[{"delta":{},"finish_reason":"content_filter"}]`, "complete", true},
		{"missing terminal stays partial", `[{"delta":{"content":""},"finish_reason":"stop"}]`, "partial", false},
		{"missing finish reason", `[{"delta":{"content":""}}]`, "partial", true},
		{"null finish reason", `[{"delta":{"content":""},"finish_reason":null}]`, "partial", true},
		{"error finish reason", `[{"delta":{"content":""},"finish_reason":"error"}]`, "partial", true},
		{"unknown finish reason", `[{"delta":{"content":""},"finish_reason":"unknown"}]`, "partial", true},
		{"missing delta", `[{"finish_reason":"stop"}]`, "partial", true},
		{"content is not accounting", `[{"delta":{"content":"synthetic"},"finish_reason":"stop"}]`, "partial", true},
		{"reasoning is not accounting", `[{"delta":{"reasoning":"synthetic"},"finish_reason":"stop"}]`, "partial", true},
		{"unfinished choice", `[{"delta":{},"finish_reason":"stop"},{"delta":{},"finish_reason":null}]`, "partial", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := modelAttempt{observer: usageObserver{protocol: "openai-completions"}}
			frame := []byte("data: {\"choices\":" + test.choice + usage + "\n\n")
			// Duplicate accounting frames replace counters instead of adding a second request's use.
			a.observer.observeEvent(frame)
			a.observer.observeEvent(frame)
			if test.terminal {
				a.observer.observeEvent([]byte("data: [DONE]\n\n"))
			}
			observed := a.usage()
			if observed.Availability != test.availability || *observed.InputTokens != 20 || *observed.OutputTokens != 3 || *observed.TotalTokens != 23 {
				t.Fatalf("accounting availability=%s counters=%+v", observed.Availability, observed)
			}
		})
	}
}

func TestSemanticOutputIgnoresEnvelopeAndEmptyChunks(t *testing.T) {
	for _, test := range []struct {
		protocol, payload string
		want              bool
	}{
		{"openai-completions", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"openai-completions", `{"choices":[{"delta":{"content":" "}}]}`, true},
		{"openai-completions", `{"choices":[{"delta":{"refusal":"declined"}}]}`, true},
		{"openai-completions", `{"choices":[{"delta":{"reasoning":"thought"}}]}`, true},
		{"openai-completions", `{"choices":[{"delta":{"reasoning_content":"thought"}}]}`, true},
		{"openai-completions", `{"choices":[{"delta":{"reasoning":"","reasoning_content":""}}]}`, false},
		{"openai-completions", `{"choices":[{"delta":{"tool_calls":[{"id":"call","function":{}}]}}]}`, false},
		{"openai-completions", `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"read"}}]}}]}`, true},
		{"openai-completions", `{"choices":[{"delta":{"function_call":{"arguments":"{"}}}]}`, true},
		{"openai-completions", `{"choices":[],"usage":{"prompt_tokens":5}}`, false},
		{"anthropic-messages", `{"type":"ping"}`, false},
		{"anthropic-messages", `{"type":"message_start","message":{"content":[],"usage":{"input_tokens":5}}}`, false},
		{"anthropic-messages", `{"type":"content_block_start","content_block":{"type":"text","text":""}}`, false},
		{"anthropic-messages", `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"signature"}}`, false},
		{"anthropic-messages", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, true},
		{"anthropic-messages", `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thought"}}`, true},
		{"anthropic-messages", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}`, true},
		{"anthropic-messages", `{"type":"content_block_start","content_block":{"type":"tool_use","name":"read","input":{}}}`, true},
	} {
		t.Run(test.protocol+"/"+test.payload, func(t *testing.T) {
			observer := usageObserver{protocol: test.protocol}
			if got := observer.observeEvent([]byte("data: " + test.payload + "\n\n")); got != test.want {
				t.Fatalf("semantic output=%t, want %t", got, test.want)
			}
		})
	}
}

func TestNonStreamedUsageAndSSEFraming(t *testing.T) {
	for _, test := range []struct{ protocol, body string }{
		{"openai-completions", `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`},
		{"anthropic-messages", `{"type":"message","content":[{"type":"thinking","thinking":"thought"}],"usage":{"input_tokens":4,"output_tokens":2}}`},
	} {
		observer := usageObserver{protocol: test.protocol}
		if !observer.observeJSON([]byte(test.body)) || !observer.terminal || !observer.finalUsage || *observer.usage.input != 4 || *observer.usage.output != 2 {
			t.Fatalf("nonstreamed observation mismatch: %+v", observer)
		}
	}
	observer := usageObserver{protocol: "openai-completions"}
	if !observer.observeEvent([]byte("\ufeff: comment\r\nevent: message\r\ndata: {\r\ndata: \"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\r\n\r\n")) {
		t.Fatal("CRLF/multiline SSE framing lost semantic output")
	}
}
