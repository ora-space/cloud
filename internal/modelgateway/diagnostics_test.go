package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wanglongan587/cloud/internal/core"
)

type failingResolver struct{}

func (failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("resolver diagnostic must remain private")
}

type transportFailure struct{ err error }

func (t transportFailure) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func TestProxyDistinguishesPolicyAndResolutionFailuresWithoutDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, code string
		resolver   addressResolver
	}{
		{"private", "model_endpoint_forbidden", fixedResolver{netip.MustParseAddr("198.18.0.11")}},
		{"resolution", "model_endpoint_resolution_failed", failingResolver{}},
		{"empty", "model_endpoint_resolution_failed", fixedResolver{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := secureDialer{resolver: test.resolver}
			_, dialErr := d.dial(context.Background(), "tcp", "models.example.invalid:443")
			if dialErr == nil {
				t.Fatal("failure did not exercise the dial policy")
			}
			cipher := testCipher(t)
			id := uuid.NewString()
			secret := randomSecret(t)
			sealed, err := cipher.Seal(id, []byte(secret))
			if err != nil {
				t.Fatal(err)
			}
			store := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", BaseURL: "https://models.example.invalid/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "selected"}}}
			s := testService(t, store, cipher, &http.Client{Transport: transportFailure{dialErr}})
			logCore, logs := observer.New(zap.WarnLevel)
			s.options.Logger = zap.New(logCore)
			r := httptest.NewRequest(http.MethodPost, "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"selected"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+randomSecret(t))
			w := httptest.NewRecorder()
			s.ModelHandler(w, r)
			var fault struct{ Code string }
			if err := json.Unmarshal(w.Body.Bytes(), &fault); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusBadGateway || fault.Code != test.code {
				t.Fatalf("status/code = %d/%s, want 502/%s", w.Code, fault.Code, test.code)
			}
			entries := logs.All()
			if len(entries) != 1 || entries[0].Message != "model upstream request rejected" || entries[0].ContextMap()["code"] != test.code || len(entries[0].Context) != 1 {
				t.Fatal("safe diagnostic log did not contain exactly the stable reason")
			}
			for _, private := range []string{secret, "198.18.0.11", "resolver diagnostic"} {
				if strings.Contains(w.Body.String(), private) {
					t.Fatal("unsafe diagnostic escaped")
				}
			}
		})
	}
}
