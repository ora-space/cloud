package modelgateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestHTTP200StreamErrorFramesNeverExposeUpstreamDiagnostics(t *testing.T) {
	secret := randomSecret(t)
	for _, test := range []struct{ protocol, frame string }{{"anthropic-messages", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"diagnostic " + secret + "\"}}\n\n"}, {"openai-completions", "data: {\"error\":{\"message\":\"diagnostic " + secret + "\"}}\n\n"}} {
		w := httptest.NewRecorder()
		if err := forwardEvents(w, strings.NewReader(test.frame), test.protocol, newResponseIO(context.Background(), w)); err == nil {
			t.Fatal("provider error was treated as completion")
		}
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "diagnostic") {
			t.Fatal("HTTP200 provider diagnostic escaped")
		}
		if !strings.Contains(w.Body.String(), "model_upstream_failed") {
			t.Fatal("missing safe stream error")
		}
	}
}

// blockedWriter simulates a connected client that stops reading. Only a forced deadline can
// release its Write, which exercises the response side that upstream cancellation alone cannot stop.
type blockedWriter struct {
	headers      http.Header
	entered      chan struct{}
	deadline     chan struct{}
	once         sync.Once
	deadlineOnce sync.Once
}

func (w *blockedWriter) Header() http.Header { return w.headers }
func (*blockedWriter) WriteHeader(int)       {}
func (w *blockedWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.deadline
	return 0, errors.New("write deadline")
}
func (*blockedWriter) Flush() {}
func (w *blockedWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.deadlineOnce.Do(func() { close(w.deadline) })
	}
	return nil
}
func (*blockedWriter) SetReadDeadline(time.Time) error { return nil }

func TestRevocationInterruptsBlockedDownstreamAndJoinsWatcher(t *testing.T) {
	c := testCipher(t)
	id := uuid.NewString()
	sealed, err := c.Seal(id, []byte(randomSecret(t)))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"started\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	policy := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", BaseURL: upstream.URL + "/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "test"}}}
	s := testService(t, policy, c, upstream.Client())
	r := httptest.NewRequest("POST", "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+randomSecret(t))
	w := &blockedWriter{headers: http.Header{}, entered: make(chan struct{}), deadline: make(chan struct{})}
	done := make(chan struct{})
	go func() { s.ModelHandler(w, r); close(done) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("downstream did not block")
	}
	policy.mu.Lock()
	policy.active = false
	policy.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked blocked writer retained its watcher")
	}
}

func TestUnsafeSuccessfulMediaTypesAndJSONErrorsAreRejected(t *testing.T) {
	if !responseHasError([]byte(`{"error":{"message":"private diagnostic"}}`)) {
		t.Fatal("JSON error envelope accepted")
	}
	if responseHasError([]byte(`{"choices":[]}`)) {
		t.Fatal("valid protocol JSON rejected")
	}
	w := httptest.NewRecorder()
	if err := forwardEvents(w, strings.NewReader("data: invalid provider diagnostic\n\n"), "openai-completions", newResponseIO(context.Background(), w)); err == nil {
		t.Fatal("invalid SSE accepted")
	}
	if strings.Contains(w.Body.String(), "provider diagnostic") {
		t.Fatal("non-JSON SSE diagnostics escaped")
	}
}
