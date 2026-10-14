package modelgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

type pendingPolicy struct {
	*fakePolicy
	started chan struct{}
	calls   atomic.Int64
}

func (p *pendingPolicy) ResolveModelGrant(ctx context.Context, digest string) (core.ModelGrant, error) {
	if p.calls.Add(1) == 1 {
		return p.fakePolicy.ResolveModelGrant(ctx, digest)
	}
	close(p.started)
	<-ctx.Done()
	return core.ModelGrant{}, ctx.Err()
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu            sync.Mutex
	writeDeadline time.Time
}

func (w *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeDeadline = deadline
	return nil
}
func (*deadlineRecorder) SetReadDeadline(time.Time) error { return nil }

func TestNormalJSONCompletionDoesNotBecomeRevocationDuringPendingLookup(t *testing.T) {
	c := testCipher(t)
	id := uuid.NewString()
	sealed, err := c.Seal(id, []byte(randomSecret(t)))
	if err != nil {
		t.Fatal(err)
	}
	policy := &pendingPolicy{fakePolicy: &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", AuthMode: "bearer", Model: core.ModelDefinition{ID: "test"}}}, started: make(chan struct{})}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-policy.started
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"complete"}}],"extension":true}`)
	}))
	defer upstream.Close()
	policy.grant.BaseURL = upstream.URL + "/v1"
	service := testService(t, policy, c, upstream.Client())
	r := httptest.NewRequest("POST", "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+randomSecret(t))
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { service.ModelHandler(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("normal completion retained a pending lookup")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "complete") {
		t.Fatal("valid JSON response truncated")
	}
	w.mu.Lock()
	deadline := w.writeDeadline
	w.mu.Unlock()
	if deadline.IsZero() || !deadline.After(time.Now()) {
		t.Fatal("normal completion lost its bounded final flush")
	}
}
