package modelgateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

type fakePolicy struct {
	mu           sync.Mutex
	grant        core.ModelGrant
	active       bool
	issuedDigest string
	sealed       []byte
	credentialID string
}

func (f *fakePolicy) ModelCredential(context.Context, *core.Claims, string) (core.Object, error) {
	return core.Object{"version": int64(1)}, nil
}

func (f *fakePolicy) PutModelCredential(_ context.Context, _ *core.Claims, _ string, _ int64, _, id string, sealed []byte, _ string) (core.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sealed = append([]byte(nil), sealed...)
	f.credentialID = id
	return core.Object{"credentialConfigured": true, "version": int64(2)}, nil
}

func (f *fakePolicy) ClearModelCredential(context.Context, *core.Claims, string, int64, string) (core.Object, error) {
	return core.Object{"credentialConfigured": false, "version": int64(2)}, nil
}

func (f *fakePolicy) CreateModelGrant(_ context.Context, _ core.ModelRuntimeScope, _, _, digest string) (core.ModelGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issuedDigest = digest
	return f.grant, nil
}

func (f *fakePolicy) RenewModelGrant(context.Context, core.ModelRuntimeScope, string, string) (core.ModelGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grant, nil
}

func (f *fakePolicy) RevokeModelGrant(context.Context, core.ModelRuntimeScope, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = false
	return nil
}

func (f *fakePolicy) ResolveModelGrant(context.Context, string) (core.ModelGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active {
		return core.ModelGrant{}, &core.Fault{Status: 403, Code: "model_grant_revoked"}
	}
	return f.grant, nil
}

type fakeVerifier struct{}

func (fakeVerifier) Verify(raw, kind string) (*core.Claims, error) {
	if raw == "" {
		return nil, errors.New("invalid")
	}
	if kind == "service" {
		return &core.Claims{Role: "gateway"}, nil
	}
	return &core.Claims{Source: "test", Caller: ""}, nil
}

func randomSecret(t *testing.T) string {
	t.Helper()
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewCipher(key, "test-v1")
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testService(t *testing.T, policy Store, cipher *Cipher, client *http.Client) *Service {
	t.Helper()
	s, err := New(&Options{Store: policy, Auth: fakeVerifier{}, Cipher: cipher, PublicOrigin: "https://models.example.invalid:8443", Upstream: client, RecheckInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestCredentialWriteOnlyBoundaryAndStrictDecode(t *testing.T) {
	c := testCipher(t)
	store := &fakePolicy{active: true}
	s := testService(t, store, c, http.DefaultClient)
	secret := randomSecret(t)
	connection := uuid.NewString()
	path := "/api/v1/me/model-connections/" + connection + "/credential"
	for _, test := range []struct {
		name, body string
		want       int
	}{{"valid", `{"version":1,"apiKey":"` + secret + `"}`, 200}, {"unknown", `{"version":1,"apiKey":"` + secret + `","userId":"forged"}`, 400}, {"trailing", `{"version":1,"apiKey":"` + secret + `"} {}`, 400}, {"version", `{"apiKey":"` + secret + `"}`, 428}} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer verified-service")
			r.Header.Set("X-Ora-User-Token", "verified-user")
			r.Header.Set("Idempotency-Key", uuid.NewString())
			w := httptest.NewRecorder()
			s.CredentialHandler(w, r)
			if w.Code != test.want {
				t.Fatalf("status %d, wanted %d", w.Code, test.want)
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("secret escaped write-only response")
			}
		})
	}
	opened, err := c.Open(store.credentialID, "test-v1", store.sealed)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(opened)
	if string(opened) != secret {
		t.Fatal("encrypted record failed round trip")
	}
	if strings.Contains(string(store.sealed), secret) {
		t.Fatal("credential stored as plaintext")
	}
}

func TestProxyPreservesBothProtocolStreamsAndReplacesAuthentication(t *testing.T) {
	for _, protocol := range []string{"openai-completions", "anthropic-messages"} {
		t.Run(protocol, func(t *testing.T) {
			secret := randomSecret(t)
			token := randomSecret(t)
			c := testCipher(t)
			id := uuid.NewString()
			sealed, err := c.Seal(id, []byte(secret))
			if err != nil {
				t.Fatal(err)
			}
			requestSeen := make(chan struct{}, 1)
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Api-Key") != "" {
					t.Error("upstream authentication was not replaced")
				}
				wantPath := "/v1/chat/completions"
				if protocol == "anthropic-messages" {
					wantPath = "/compatible/v1/messages"
				}
				if r.URL.Path != wantPath {
					t.Errorf("path %s expected %s", r.URL.Path, wantPath)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "vendor/model" {
					t.Error("slash model ID changed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				frame := "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"
				if protocol == "anthropic-messages" {
					frame = "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n"
				}
				_, _ = io.WriteString(w, frame)
				w.(http.Flusher).Flush()
				requestSeen <- struct{}{}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()
			base := upstream.URL + "/v1"
			path := "/runtime/openai/v1/chat/completions"
			if protocol == "anthropic-messages" {
				base = upstream.URL + "/compatible"
				path = "/runtime/anthropic/v1/messages"
			}
			store := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: protocol, BaseURL: base, AuthMode: "bearer", Model: core.ModelDefinition{ID: "vendor/model"}}}
			s := testService(t, store, c, upstream.Client())
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"vendor/model","stream":true,"messages":[],"tools":[]}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			if protocol == "anthropic-messages" {
				r.Header.Set("X-Api-Key", token)
			}
			w := httptest.NewRecorder()
			s.ModelHandler(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "first") || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("stream mismatch, status %d", w.Code)
			}
			select {
			case <-requestSeen:
			default:
				t.Fatal("request not forwarded")
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("upstream key entered output")
			}
		})
	}
}

func TestProxyCancelsActiveStreamWhenDurableAuthorizationIsRevoked(t *testing.T) {
	c := testCipher(t)
	id := uuid.NewString()
	sealed, err := c.Seal(id, []byte(randomSecret(t)))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"started\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer upstream.Close()
	store := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", BaseURL: upstream.URL + "/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "test"}}}
	s := testService(t, store, c, upstream.Client())
	r := httptest.NewRequest(http.MethodPost, "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+randomSecret(t))
	done := make(chan struct{})
	go func() { s.ModelHandler(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	store.mu.Lock()
	store.active = false
	store.mu.Unlock()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel upstream")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request owner did not join watcher")
	}
}

func TestProxyRejectsModelMismatchAndSuppressesUpstreamErrors(t *testing.T) {
	c := testCipher(t)
	id := uuid.NewString()
	secret := randomSecret(t)
	sealed, err := c.Seal(id, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "diagnostic "+secret)
	}))
	defer upstream.Close()
	store := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", BaseURL: upstream.URL + "/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "selected"}}}
	s := testService(t, store, c, upstream.Client())
	for _, test := range []struct {
		model string
		want  int
	}{{"other", 403}, {"selected", 502}} {
		r := httptest.NewRequest("POST", "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"`+test.model+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+randomSecret(t))
		w := httptest.NewRecorder()
		s.ModelHandler(w, r)
		if w.Code != test.want {
			t.Fatalf("status %d expected %d", w.Code, test.want)
		}
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "diagnostic") {
			t.Fatal("raw upstream diagnostic escaped")
		}
	}
}
