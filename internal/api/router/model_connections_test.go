package router

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
)

type forbiddenCredentialReader struct{ t *testing.T }

func (r forbiddenCredentialReader) Read([]byte) (int, error) {
	r.t.Fatal("Cloud read a model credential body")
	return 0, nil
}

func TestCloudCredentialRouteRejectsBeforeReadingBody(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := core.NewAuthenticator("test-model", []core.TrustedKey{{ID: "service", Issuer: "test", Kind: "service", Role: "gateway", Key: pub}, {ID: "user", Issuer: "test", Kind: "user", Key: pub}})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(kind, role, subject, caller, kid string) string {
		claims := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "test", Subject: subject, Audience: jwt.ClaimStrings{"test-model"}, IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, Kind: kind, Role: role, Caller: caller, Source: "test-model"}
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		token.Header["kid"] = kid
		raw, e := token.SignedString(priv)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	engine := New(nil, auth, zap.NewNop())
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/me/model-connections/00000000-0000-4000-8000-000000000001/credential", forbiddenCredentialReader{t: t})
		req.Header.Set("Authorization", "Bearer "+sign("service", "gateway", "gateway", "", "service"))
		req.Header.Set("X-Ora-User-Token", sign("user", "", "person", "gateway", "user"))
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d", method, res.Code)
		}
	}
}

func TestModelArrayStrictBoundary(t *testing.T) {
	valid := map[string]any{"id": "vendor/model-v1", "name": "Model", "contextWindow": json.Number("128000"), "maxTokens": json.Number("8192")}
	if !validField("models", []any{valid}) {
		t.Fatal("valid model array refused")
	}
	for _, field := range []string{"contextWindow", "maxTokens"} {
		copy := map[string]any{}
		for key, value := range valid {
			copy[key] = value
		}
		copy[field] = json.Number("1.5")
		if validField("models", []any{copy}) {
			t.Fatalf("fractional %s accepted", field)
		}
	}
	valid["authorization"] = "unrecognized"
	if validField("models", []any{valid}) {
		t.Fatal("unknown nested field accepted")
	}
	if validField("enabled", "true") {
		t.Fatal("string enabled accepted")
	}
}
