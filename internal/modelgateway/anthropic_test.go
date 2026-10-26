package modelgateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

const pinnedClaudeBetas = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24"

// The first request the bundled Claude Code 2.1.303 sent for claude-haiku-5-5 when run directly
// with the adapter's alias and model environment, so it carries no Agent SDK additions.
const pinnedClaudeHaikuBetas = "interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,claude-code-20250219,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,effort-2025-11-24"

// Each pinned CLI's exact first-request header for a Claude model is forwarded unchanged.
func TestPinnedCLIClaudeModelBetas(t *testing.T) {
	for name, betas := range map[string]string{
		// Claude Code 2.1.303 run directly with the adapter's alias and model environment, with
		// claude-haiku-5-5; the Agent SDK's own additions are covered separately below.
		"claude": pinnedClaudeHaikuBetas,
		// OpenCode 1.19.3 through @ai-sdk/anthropic, with claude-haiku-5-5.
		"opencode": "thinking-binding-controls-2026-08-01,structured-outputs-2025-11-13",
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			service, _ := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Anthropic-Beta") != betas {
					t.Error("pinned betas changed while forwarding")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[{\"type\":\"text\",\"text\":\"{}\"}],\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}")
			}))
			request := httptest.NewRequest(http.MethodPost, "/runtime/anthropic/v1/messages", strings.NewReader("{\"model\":\"fixture-model\",\"messages\":[],\"max_tokens\":16}"))
			request.Header.Set("X-Api-Key", strings.Repeat("r", 40))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Anthropic-Version", anthropicVersion)
			request.Header.Set("Anthropic-Beta", betas)
			response := httptest.NewRecorder()
			service.ModelHandler(response, request)
			if response.Code != 200 || calls.Load() != 1 {
				t.Fatalf("pinned %s betas rejected: status=%d calls=%d", name, response.Code, calls.Load())
			}
		})
	}
}

// Agent SDK query creation enables these additional capabilities on the same pinned native CLI.
func TestPinnedClaudeAgentSDKCapabilities(t *testing.T) {
	betas := pinnedClaudeBetas + ",structured-outputs-2025-12-15,thinking-display-updates-2026-08-18"
	var calls atomic.Int64
	service, _ := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Anthropic-Beta") != betas || r.URL.RawQuery != "beta=true" {
			t.Error("SDK capabilities changed while forwarding")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[{\"type\":\"text\",\"text\":\"{}\"}],\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}")
	}))
	request := httptest.NewRequest(http.MethodPost, "/runtime/anthropic/v1/messages?beta=true", strings.NewReader("{\"model\":\"fixture-model\",\"messages\":[],\"max_tokens\":16}"))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Version", anthropicVersion)
	request.Header.Set("Anthropic-Beta", betas)
	response := httptest.NewRecorder()
	service.ModelHandler(response, request)
	if response.Code != 200 || calls.Load() != 1 {
		t.Fatalf("pinned SDK request rejected: status=%d calls=%d", response.Code, calls.Load())
	}
}

// The documented production combination: the same CLI started through Agent SDK 0.3.303 / ACP
// adapter 0.94.0 for claude-haiku-5-5 declares the direct run's betas plus the SDK's additions.
// The header is composed from those two captured sets rather than captured end to end, so this
// pins that the gateway accepts and forwards the union unchanged, not the client's header order.
func TestPinnedClaudeACPHaikuBetaCombination(t *testing.T) {
	betas := pinnedClaudeHaikuBetas + ",structured-outputs-2025-12-15,thinking-display-updates-2026-08-18"
	var calls atomic.Int64
	service, _ := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Anthropic-Beta") != betas || r.URL.RawQuery != "beta=true" {
			t.Error("ACP and haiku betas changed while forwarding")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[{\"type\":\"text\",\"text\":\"{}\"}],\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}")
	}))
	request := httptest.NewRequest(http.MethodPost, "/runtime/anthropic/v1/messages?beta=true", strings.NewReader("{\"model\":\"fixture-model\",\"messages\":[],\"max_tokens\":16}"))
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Version", anthropicVersion)
	request.Header.Set("Anthropic-Beta", betas)
	response := httptest.NewRecorder()
	service.ModelHandler(response, request)
	if response.Code != 200 || calls.Load() != 1 {
		t.Fatalf("ACP haiku betas rejected: status=%d calls=%d", response.Code, calls.Load())
	}
}

// anthropicFixture keeps the upstream key separate from the workload's short-lived token.
func anthropicFixture(t *testing.T, handler http.Handler) (*Service, *fakePolicy) {
	t.Helper()
	cipher := testCipher(t)
	id := uuid.NewString()
	sealed, err := cipher.Seal(id, []byte("synthetic-upstream-key"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	policy := &fakePolicy{active: true, grant: core.ModelGrant{
		CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "anthropic-messages",
		BaseURL: upstream.URL + "/compatible/v1", AuthMode: "x-api-key",
		Model: core.ModelDefinition{ID: "fixture-model", Name: "Fixture Model", ContextWindow: 128000, MaxTokens: 8192},
	}}
	return testService(t, policy, cipher, upstream.Client()), policy
}

func TestClaudeMessagesQueryHeadersAndToolStream(t *testing.T) {
	var requests atomic.Int64
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"file.txt\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	service, policy := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/compatible/v1/messages" || r.URL.RawQuery != "beta=true" {
			t.Errorf("request escaped the protocol endpoint: %s", r.URL.RequestURI())
		}
		if r.Header.Get("Anthropic-Beta") != pinnedClaudeBetas || r.Header.Get("Anthropic-Version") != anthropicVersion {
			t.Error("pinned protocol headers were not preserved")
		}
		if r.Header.Get("X-Api-Key") != "synthetic-upstream-key" || r.Header.Get("Authorization") != "" {
			t.Error("runtime authentication reached the upstream")
		}
		for _, header := range []string{"X-App", "X-Claude-Code-Session-Id", "X-Injected", "Anthropic-Dangerous-Direct-Browser-Access"} {
			if r.Header.Get(header) != "" {
				t.Errorf("unapproved header forwarded: %s", header)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	request := httptest.NewRequest(http.MethodPost, "/runtime/anthropic/v1/messages?beta=true",
		strings.NewReader("{\"model\":\"fixture-model\",\"stream\":true,\"messages\":[],\"tools\":[{\"name\":\"Read\",\"input_schema\":{\"type\":\"object\"}}]}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	request.Header.Set("Anthropic-Version", anthropicVersion)
	request.Header.Set("Anthropic-Beta", pinnedClaudeBetas)
	request.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	request.Header.Set("X-App", "cli")
	request.Header.Set("X-Claude-Code-Session-Id", "private-session")
	request.Header.Set("X-Injected", "must-not-forward")
	response := httptest.NewRecorder()
	service.ModelHandler(response, request)
	if response.Code != 200 || response.Body.String() != stream || requests.Load() != 1 {
		t.Fatalf("tool stream changed or retried: status=%d requests=%d", response.Code, requests.Load())
	}
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if len(policy.attempts) != 1 {
		t.Fatal("one inference was not one recorded attempt")
	}
	for _, attempt := range policy.attempts {
		if attempt.Outcome != "succeeded" {
			t.Fatalf("unexpected outcome: %s", attempt.Outcome)
		}
	}
}

func TestClaudeProtocolRejectsUnknownSurface(t *testing.T) {
	var forwarded atomic.Int64
	service, _ := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(500)
	}))
	for _, test := range []struct {
		name, method, path, header, value string
		want                              int
	}{
		{"extra-query", "POST", "/runtime/anthropic/v1/messages?beta=true&target=other", "", "", 404},
		{"duplicate-query", "POST", "/runtime/anthropic/v1/messages?beta=true&beta=true", "", "", 404},
		{"encoded-query", "POST", "/runtime/anthropic/v1/messages?%62eta=true", "", "", 404},
		{"wrong-beta", "POST", "/runtime/anthropic/v1/messages?beta=false", "", "", 404},
		{"empty-query", "POST", "/runtime/anthropic/v1/messages?", "", "", 404},
		{"encoded-path", "POST", "/runtime/anthropic/v1/%6dessages", "", "", 404},
		{"counting", "POST", "/runtime/anthropic/v1/messages/count_tokens", "", "", 404},
		{"responses", "POST", "/runtime/anthropic/v1/responses", "", "", 404},
		{"method", "PUT", "/runtime/anthropic/v1/messages", "", "", 404},
		{"openai-query", "POST", "/runtime/openai/v1/chat/completions?beta=true", "", "", 404},
		{"discovery-query", "GET", "/runtime/anthropic/v1/models?limit=1000&after_id=other", "", "", 404},
		{"unknown-header", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Organization", "other", 400},
		{"unknown-beta", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Beta", "unknown-2026-01-01", 400},
		{"duplicate-beta", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Beta", "effort-2025-11-24,effort-2025-11-24", 400},
		{"empty-beta", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Beta", "", 400},
		{"oversize-beta", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Beta", strings.Repeat("x", 1025), 400},
		{"version", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Version", "unknown", 400},
		{"browser-marker", "POST", "/runtime/anthropic/v1/messages", "Anthropic-Dangerous-Direct-Browser-Access", "false", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader("{\"model\":\"fixture-model\",\"messages\":[]}"))
			request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
			request.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				request.Header.Set(test.header, test.value)
			}
			response := httptest.NewRecorder()
			service.ModelHandler(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d", response.Code, test.want)
			}
		})
	}
	if forwarded.Load() != 0 {
		t.Fatal("a rejected request reached upstream")
	}
}

func TestFrozenAnthropicDiscoveryUsesOnlyCurrentGrant(t *testing.T) {
	service, policy := anthropicFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("frozen model listing attempted upstream discovery")
		w.WriteHeader(500)
	}))
	for _, query := range []string{"", "?limit=1000"} {
		request := httptest.NewRequest(http.MethodGet, "/runtime/anthropic/v1/models"+query, nil)
		request.Header.Set("X-Api-Key", strings.Repeat("r", 40))
		response := httptest.NewRecorder()
		service.ModelHandler(response, request)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(response.Code)
		}
		var listing core.Object
		if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
			t.Fatal(err)
		}
		if listing["has_more"] != false || listing["first_id"] != "fixture-model" || listing["last_id"] != "fixture-model" {
			t.Fatal("listing did not describe exactly the frozen model")
		}
		for _, forbidden := range []string{"credential", "synthetic-upstream-key", "compatible", "baseUrl"} {
			if strings.Contains(response.Body.String(), forbidden) {
				t.Fatal("private grant material in listing")
			}
		}
	}
	policy.mu.Lock()
	policy.active = false
	attempts := len(policy.attempts)
	policy.mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/runtime/anthropic/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+strings.Repeat("r", 40))
	response := httptest.NewRecorder()
	service.ModelHandler(response, request)
	if response.Code != 403 || attempts != 0 {
		t.Fatalf("revoked discovery=%d attempts=%d", response.Code, attempts)
	}
}
