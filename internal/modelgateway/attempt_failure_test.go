package modelgateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestModelAttemptsRecordSafeFailureOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, media, body, outcome, code string
		status                           int
	}{
		{"rate limit", "application/json", `{"error":"synthetic secret diagnostic"}`, "failed", "model_rate_limited", 429},
		{"authentication", "application/json", `{"error":"synthetic secret diagnostic"}`, "failed", "model_authentication_failed", 401},
		{"provider failure", "application/json", `{"error":"synthetic secret diagnostic"}`, "failed", "model_upstream_failed", 500},
		{"invalid media", "text/plain", "synthetic secret diagnostic", "failed", "model_upstream_invalid_response", 200},
		{"invalid json", "application/json", "synthetic secret diagnostic", "failed", "model_upstream_invalid_response", 200},
		{"json error", "application/json", `{"error":"synthetic secret diagnostic"}`, "failed", "model_upstream_failed", 200},
		{"stream error", "text/event-stream", "data: {\"error\":{\"message\":\"synthetic secret diagnostic\"}}\n\n", "failed", "model_upstream_failed", 200},
		{"invalid stream", "text/event-stream", "data: synthetic secret diagnostic\n\n", "failed", "model_upstream_failed", 200},
		{"oversized stream", "text/event-stream", "data: " + strings.Repeat("x", maxModelEventBytes), "failed", "model_event_limit_exceeded", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, policy := metricService(t, "openai-completions", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.media)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			w := httptest.NewRecorder()
			service.ModelHandler(w, metricRequest(t, "openai-completions", `{"model":"synthetic-model"}`))
			result := onlyAttempt(t, policy)
			if result.Outcome != test.outcome || result.ErrorCode != test.code || result.TerminalObserved || result.Usage.Availability != "unavailable" || result.FirstOutputMS != nil || result.HTTPStatus == nil || *result.HTTPStatus != test.status {
				t.Fatalf("unexpected failure observation: %+v usage=%+v", result, result.Usage)
			}
			if strings.Contains(w.Body.String(), "synthetic secret diagnostic") {
				t.Fatal("private failure text escaped forwarding boundary")
			}
		})
	}
}

type metricTransport func(*http.Request) (*http.Response, error)

func (f metricTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenModelStream struct{ once bool }

func (r *brokenModelStream) Read(data []byte) (int, error) {
	if r.once {
		return 0, errors.New("synthetic private transport detail")
	}
	r.once = true
	return copy(data, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"), nil
}

func (*brokenModelStream) Close() error { return nil }

func TestModelAttemptsTransportAndDownstreamInterruptions(t *testing.T) {
	for _, test := range []struct {
		name, outcome, code string
		transport           metricTransport
		writer              http.ResponseWriter
		first               bool
	}{
		{"dial failure", "failed", "model_upstream_unavailable", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic private transport detail")
		}, httptest.NewRecorder(), false},
		{"stream interruption", "interrupted", "model_stream_unavailable", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &brokenModelStream{}}, nil
		}, httptest.NewRecorder(), true},
		{"downstream failure", "interrupted", "model_downstream_unavailable", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\ndata: [DONE]\n\n"))}, nil
		}, &metricBrokenWriter{ResponseRecorder: httptest.NewRecorder()}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, policy := metricService(t, "openai-completions", func(http.ResponseWriter, *http.Request) {
				t.Error("unexpected original transport")
			})
			service.options.Upstream = &http.Client{Transport: test.transport}
			service.ModelHandler(test.writer, metricRequest(t, "openai-completions", `{"model":"synthetic-model"}`))
			result := onlyAttempt(t, policy)
			if result.Outcome != test.outcome || result.ErrorCode != test.code || (result.FirstOutputMS != nil) != test.first || result.TerminalObserved {
				t.Fatalf("interruption lost evidence: %+v", result)
			}
		})
	}
}

type metricBrokenWriter struct{ *httptest.ResponseRecorder }

func (*metricBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic downstream failure")
}

func TestModelAttemptsRejectedRequestsNeverDispatchOrBegin(t *testing.T) {
	service, policy := metricService(t, "openai-completions", func(http.ResponseWriter, *http.Request) {
		t.Error("invalid model request reached network")
	})
	for _, body := range []string{`{"model":"other"}`, `invalid`, `{"model":"synthetic-model"} {}`} {
		service.ModelHandler(httptest.NewRecorder(), metricRequest(t, "openai-completions", body))
	}
	if policy.beginCalls != 0 || len(policy.finishes) != 0 {
		t.Fatal("rejected request counted as a model attempt")
	}
}

func TestModelUsageCompletenessDoesNotInventMissingFields(t *testing.T) {
	for _, test := range []struct {
		name, protocol, availability string
		frames                       []string
	}{
		{"start is not final usage", "anthropic-messages", "partial", []string{`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1}}}`, `{"type":"message_stop"}`}},
		{"invalid later usage clears complete", "openai-completions", "partial", []string{`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, `{"choices":[],"usage":"malformed"}`, `[DONE]`}},
		{"empty final snapshot is unknown", "openai-completions", "unavailable", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, `{"choices":[],"usage":{}}`, `[DONE]`}},
		{"all cleared is unavailable", "openai-completions", "unavailable", []string{`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`, `{"choices":[],"usage":{"prompt_tokens":null,"completion_tokens":null}}`, `[DONE]`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := modelAttempt{observer: usageObserver{protocol: test.protocol}}
			for _, frame := range test.frames {
				a.observer.observeEvent([]byte("data: " + frame + "\n\n"))
			}
			if got := a.usage(); got.Availability != test.availability {
				t.Fatalf("usage=%+v expected=%s", got, test.availability)
			}
		})
	}
}

func TestModelAttemptAuthorizationObservationFailureIsInterrupted(t *testing.T) {
	service, policy := metricService(t, "openai-completions", func(http.ResponseWriter, *http.Request) {})
	a, err := service.beginAttempt(t.Context(), &core.ModelGrant{Protocol: "openai-completions"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.Join(errAuthorizationWatch, errors.New("synthetic unavailable database")))
	service.finishAttempt(ctx, a)
	if result := onlyAttempt(t, policy); result.Outcome != "interrupted" || result.ErrorCode != "model_authorization_unavailable" {
		t.Fatalf("authorization observation failure mislabeled: %+v", result)
	}
}
