package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// attemptPolicy injects persistence failures without replacing the real HTTP/TLS boundary.
type attemptPolicy struct {
	*fakePolicy
	beginError    error
	finishErrors  int
	beginCalls    int
	finishes      []core.ModelAttemptFinish
	finishIDs     []string
	finishContext []bool
	streamed      bool
}

func (p *attemptPolicy) BeginModelAttempt(ctx context.Context, grant *core.ModelGrant, id string, streamed bool) error {
	p.mu.Lock()
	p.beginCalls++
	p.streamed = streamed
	p.mu.Unlock()
	if p.beginError != nil {
		return p.beginError
	}
	return p.fakePolicy.BeginModelAttempt(ctx, grant, id, streamed)
}

func (p *attemptPolicy) FinishModelAttempt(ctx context.Context, id string, result *core.ModelAttemptFinish) error {
	p.mu.Lock()
	deadline, bounded := ctx.Deadline()
	p.finishContext = append(p.finishContext, ctx.Err() == nil && bounded && time.Until(deadline) <= attemptPersistenceTimeout)
	p.finishes = append(p.finishes, *result)
	p.finishIDs = append(p.finishIDs, id)
	fail := len(p.finishes) <= p.finishErrors
	p.mu.Unlock()
	if fail {
		return errors.New("synthetic persistence failure")
	}
	return p.fakePolicy.FinishModelAttempt(ctx, id, result)
}

func metricService(t *testing.T, protocol string, handler http.HandlerFunc) (*Service, *attemptPolicy) {
	t.Helper()
	cipher := testCipher(t)
	id := uuid.NewString()
	sealed, err := cipher.Seal(id, []byte(randomSecret(t)))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	policy := &attemptPolicy{fakePolicy: &fakePolicy{active: true, grant: core.ModelGrant{
		ID: uuid.NewString(), BindingID: uuid.NewString(), RunID: uuid.NewString(), ExecutionID: uuid.NewString(),
		CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed,
		Protocol: protocol, BaseURL: upstream.URL + "/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "synthetic-model"},
	}}}
	service := testService(t, policy, cipher, upstream.Client())
	return service, policy
}

func metricRequest(t *testing.T, protocol, body string) *http.Request {
	t.Helper()
	path := "/runtime/openai/v1/chat/completions"
	if protocol == "anthropic-messages" {
		path = "/runtime/anthropic/v1/messages"
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+randomSecret(t))
	return r
}

func onlyAttempt(t *testing.T, policy *attemptPolicy) core.ModelAttemptFinish {
	t.Helper()
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if policy.beginCalls != 1 || len(policy.attempts) != 1 {
		t.Fatalf("begin=%d attempts=%d, expected exactly one dispatch", policy.beginCalls, len(policy.attempts))
	}
	for _, result := range policy.attempts {
		return result
	}
	t.Fatal("missing attempt")
	return core.ModelAttemptFinish{}
}

func TestModelAttemptsObserveSuccessfulProtocolsWithoutChangingBytes(t *testing.T) {
	for _, test := range []struct {
		name, protocol, contentType, response, availability, outcome string
		input, output                                                *int64
		semantic, terminal                                           bool
	}{
		{"json", "openai-completions", "application/json", ` {"choices":[{"message":{"content":"synthetic output"}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":1}},"extension":{"private":"do not store"}} `, "complete", "succeeded", count(12), count(3), true, true},
		{"no usage", "openai-completions", "application/json", `{"choices":[{"message":{"content":"hello"}}]}`, "unavailable", "succeeded", nil, nil, true, true},
		{"reasoning before content", "openai-completions", "text/event-stream", "data: {\"choices\":[{\"delta\":{\"reasoning\":\"thought\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n", "unavailable", "succeeded", nil, nil, true, true},
		{"sse", "openai-completions", "text/event-stream", ": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", "complete", "succeeded", count(12), count(3), true, true},
		{"missing terminal", "openai-completions", "text/event-stream", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n", "partial", "incomplete", count(12), count(3), false, false},
		{"anthropic", "anthropic-messages", "text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1,\"cache_read_input_tokens\":8,\"cache_creation_input_tokens\":2}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thought\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "complete", "succeeded", count(12), count(3), true, true},
		{"anthropic missing final usage", "anthropic-messages", "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "partial", "succeeded", count(12), count(1), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var policy *attemptPolicy
			service, store := metricService(t, test.protocol, func(w http.ResponseWriter, _ *http.Request) {
				policy.mu.Lock()
				started := len(policy.attempts) == 1
				for _, result := range policy.attempts {
					started = started && result.Outcome == "started"
				}
				policy.mu.Unlock()
				if !started {
					t.Error("network effect occurred before durable intent")
				}
				w.Header().Set("X-Private-Provider-Header", "do not store")
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.response)
			})
			policy = store
			base := time.Now()
			var calls int
			service.options.Now = func() time.Time {
				calls++
				if calls == 1 {
					return base
				}
				if calls == 2 && test.semantic {
					return base.Add(40 * time.Millisecond)
				}
				return base.Add(75 * time.Millisecond)
			}
			w := httptest.NewRecorder()
			service.ModelHandler(w, metricRequest(t, test.protocol, `{"model":"synthetic-model","stream":true,"messages":[]}`))
			if w.Code != 200 || w.Body.String() != test.response {
				t.Fatalf("forwarding changed: status=%d", w.Code)
			}
			result := onlyAttempt(t, policy)
			if result.Outcome != test.outcome || result.TerminalObserved != test.terminal || result.Usage.Availability != test.availability || !reflect.DeepEqual(result.Usage.InputTokens, test.input) || !reflect.DeepEqual(result.Usage.OutputTokens, test.output) {
				t.Fatalf("unexpected attempt: %+v usage=%+v", result, result.Usage)
			}
			if result.DurationMS != 75 || (result.FirstOutputMS != nil) != test.semantic || test.semantic && *result.FirstOutputMS != 40 {
				t.Fatalf("timing mismatch: %+v", result)
			}
			if result.HTTPStatus == nil || *result.HTTPStatus != 200 {
				t.Fatal("missing actual upstream HTTP status")
			}
			encoded, err := json.Marshal(result)
			if err != nil || strings.Contains(string(encoded), "do not store") || strings.Contains(string(encoded), "synthetic output") {
				t.Fatal("provider content entered telemetry")
			}
		})
	}
}

func TestModelAttemptsPersistenceFencesAndIdempotentFinalization(t *testing.T) {
	for _, test := range []struct {
		name                 string
		beginFails           bool
		finishFails          int
		wantStatus, finishes int
		wantOutcome          string
	}{
		{"begin fails closed", true, 0, 503, 0, ""},
		{"finish retries same result", false, 2, 200, 3, "succeeded"},
		{"unresolved finish stays started", false, 3, 200, 3, "started"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			service, policy := metricService(t, "openai-completions", func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
			})
			if test.beginFails {
				policy.beginError = errors.New("synthetic private database diagnostic")
			}
			policy.finishErrors = test.finishFails
			w := httptest.NewRecorder()
			service.ModelHandler(w, metricRequest(t, "openai-completions", `{"model":"synthetic-model"}`))
			if w.Code != test.wantStatus || len(policy.finishes) != test.finishes || strings.Contains(w.Body.String(), "diagnostic") {
				t.Fatalf("status=%d finalizations=%d", w.Code, len(policy.finishes))
			}
			if test.beginFails {
				if upstreamCalls.Load() != 0 || len(policy.attempts) != 0 {
					t.Fatal("failed intent still dispatched")
				}
				return
			}
			if upstreamCalls.Load() != 1 || onlyAttempt(t, policy).Outcome != test.wantOutcome {
				t.Fatal("finalization retried upstream or falsely completed attempt")
			}
			for i, finish := range policy.finishes {
				if !policy.finishContext[i] || policy.finishIDs[i] != policy.finishIDs[0] || !reflect.DeepEqual(finish, policy.finishes[0]) {
					t.Fatal("retry changed its immutable identity, payload or context bound")
				}
			}
		})
	}
}

func TestModelAttemptsNeverOverrideStreamOptions(t *testing.T) {
	for _, options := range []string{"", `,"stream_options":{"include_usage":true,"extension":1}`, `,"stream_options":{"include_usage":false}`} {
		service, policy := metricService(t, "openai-completions", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var value map[string]json.RawMessage
			_ = json.Unmarshal(body, &value)
			if options == "" && value["stream_options"] != nil {
				t.Error("injected unsupported usage option")
			}
			if options != "" {
				var original map[string]json.RawMessage
				_ = json.Unmarshal([]byte("{"+strings.TrimPrefix(options, ",")+"}"), &original)
				if !reflect.DeepEqual(jsonObject(value["stream_options"]), jsonObject(original["stream_options"])) {
					t.Error("overrode caller stream options")
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
		service.ModelHandler(httptest.NewRecorder(), metricRequest(t, "openai-completions", `{"model":"synthetic-model","stream":true`+options+`}`))
		if !policy.streamed || onlyAttempt(t, policy).Usage.Availability != "unavailable" {
			t.Fatal("missing usage was fabricated")
		}
	}
}

func TestModelAttemptsCancellationPersistsAfterRequestEnds(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "client cancellation", true: "grant revocation"}[revoke], func(t *testing.T) {
			started := make(chan struct{})
			service, policy := metricService(t, "anthropic-messages", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := metricRequest(t, "anthropic-messages", `{"model":"synthetic-model","stream":true}`).WithContext(ctx)
			writer := &signallingWriter{ResponseRecorder: httptest.NewRecorder(), observed: started}
			done := make(chan struct{})
			go func() { service.ModelHandler(writer, r); close(done) }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("first usage frame not observed")
			}
			if revoke {
				policy.mu.Lock()
				policy.active = false
				policy.mu.Unlock()
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled request did not finish")
			}
			result := onlyAttempt(t, policy)
			if result.Outcome != "cancelled" || result.TerminalObserved || result.FirstOutputMS != nil || result.Usage.Availability != "partial" || !policy.finishContext[0] {
				t.Fatalf("cancellation lost partial evidence: %+v usage=%+v", result, result.Usage)
			}
			wantCode := "model_request_cancelled"
			if revoke {
				wantCode = "model_authorization_revoked"
			}
			if result.ErrorCode != wantCode {
				t.Fatalf("code=%s expected=%s", result.ErrorCode, wantCode)
			}
		})
	}
}

type signallingWriter struct {
	*httptest.ResponseRecorder
	observed chan struct{}
	once     sync.Once
}

func (w *signallingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	w.once.Do(func() { close(w.observed) })
	return n, err
}
