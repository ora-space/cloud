package credentialstore

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

func TestSubmissionHappyPath(t *testing.T) {
	h := newHarness(t)
	grant := h.submission()
	status, out, header := h.put(t, testCredentialID, h.signSubmission(t, grant), testOrigin, `{"token":"`+canaryToken+`","username":"octocat"}`)
	if status != http.StatusOK || out["storeVersion"] != "1" || len(out) != 1 {
		t.Fatalf("submit = %d %v, want 200 {storeVersion:1} only", status, out)
	}
	if header.Get("Access-Control-Allow-Origin") != testOrigin || header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("CORS headers = %v, want the exact origin and no credentials", header)
	}
	metadata, err := h.managementClient(t, h.managerCert).Get(context.Background(), testCredentialID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := Metadata{CredentialID: testCredentialID, CurrentVersion: 1, SubmissionID: grant.SubmissionID}
	if metadata != want {
		t.Fatalf("metadata = %+v, want %+v", metadata, want)
	}
	raw, err := os.ReadFile(filepath.Join(h.root, credentialsDir, testCredentialID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canaryToken) || strings.Contains(string(raw), "octocat") {
		t.Fatal("the credential record holds plaintext")
	}
	if _, err := os.Stat(filepath.Join(h.root, submissionsDir, grant.SubmissionID)); err != nil {
		t.Fatalf("submission claim missing: %v", err)
	}
	got, err := h.redemptionClient(t, h.executorCert).Redeem(context.Background(), h.signAuthorization(t, h.authorization()))
	if err != nil || got.Username != "octocat" || got.Token != canaryToken {
		t.Fatalf("Redeem = %v, %v; want the submitted username and token", got, err)
	}
}

func TestSubmissionDefaultsUsername(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	got, err := h.redemptionClient(t, h.executorCert).Redeem(context.Background(), h.signAuthorization(t, h.authorization()))
	if err != nil || got.Username != DefaultUsername {
		t.Fatalf("Redeem = %v, %v; want username %q", got, err, DefaultUsername)
	}
}

func TestSubmissionRefusals(t *testing.T) {
	body := `{"token":"` + canaryToken + `"}`
	cases := []struct {
		name string
		// prepare may store earlier versions and returns the request to send.
		prepare    func(t *testing.T, h *harness) (credentialID, authorization, origin, body string)
		wantStatus int
		wantCode   string
	}{
		{"missing grant", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, "", testOrigin, body
		}, 401, CodeInvalidSubmission},
		{"replayed submission id", func(t *testing.T, h *harness) (string, string, string, string) {
			grant := h.storeVersion(t, 1)
			grant.ExpectedStoreVersion = 2 // even a grant re-signed for the next version cannot reuse its id
			return testCredentialID, h.signSubmission(t, grant), testOrigin, body
		}, 409, CodeSubmissionReplayed},
		{"expired grant", func(t *testing.T, h *harness) (string, string, string, string) {
			token := h.signSubmission(t, h.submission())
			h.clock.Advance(5*time.Minute + time.Second)
			return testCredentialID, token, testOrigin, body
		}, 401, CodeSubmissionExpired},
		{"tampered grant", func(t *testing.T, h *harness) (string, string, string, string) {
			token := []byte(h.signSubmission(t, h.submission()))
			token[3] ^= 0x01
			return testCredentialID, string(token), testOrigin, body
		}, 401, CodeInvalidSubmission},
		{"grant signed with the execution key", func(t *testing.T, h *harness) (string, string, string, string) {
			token, err := cloneauth.SignSubmission(h.executionKey, h.submission())
			if err != nil {
				t.Fatal(err)
			}
			return testCredentialID, token, testOrigin, body
		}, 401, CodeInvalidSubmission},
		{"grant for another credential id", func(t *testing.T, h *harness) (string, string, string, string) {
			return "00000000-0000-4000-8000-000000000001", h.signSubmission(t, h.submission()), testOrigin, body
		}, 403, CodeSubmissionMismatch},
		{"execution token used as submission", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signAuthorization(t, h.authorization()), testOrigin, body
		}, 401, CodeInvalidSubmission},
		{"wrong expected version", func(t *testing.T, h *harness) (string, string, string, string) {
			grant := h.submission(func(s *cloneauth.Submission) { s.ExpectedStoreVersion = 2 })
			return testCredentialID, h.signSubmission(t, grant), testOrigin, body
		}, 409, CodeVersionConflict},
		{"stale expected version after a rotation", func(t *testing.T, h *harness) (string, string, string, string) {
			h.storeVersion(t, 1)
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, body
		}, 409, CodeVersionConflict},
		{"another tenant", func(t *testing.T, h *harness) (string, string, string, string) {
			h.storeVersion(t, 1)
			grant := h.submission(func(s *cloneauth.Submission) {
				s.ExpectedStoreVersion = 2
				s.TenantID = "00000000-0000-4000-8000-000000000002"
			})
			return testCredentialID, h.signSubmission(t, grant), testOrigin, body
		}, 403, CodeSubmissionMismatch},
		{"revoked credential", func(t *testing.T, h *harness) (string, string, string, string) {
			if err := h.managementClient(t, h.managerCert).Revoke(context.Background(), testCredentialID); err != nil {
				t.Fatal(err)
			}
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, body
		}, 410, CodeCredentialRevoked},
		{"disallowed origin", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), "https://evil.example", body
		}, 403, CodeOriginForbidden},
		{"oversize body", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"` + strings.Repeat("a", maxRequestBody) + `"}`
		}, 413, CodeRequestTooLarge},
		{"token too long", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"` + strings.Repeat("a", maxTokenLength+1) + `"}`
		}, 400, CodeInvalidRequest},
		{"token with newline", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"abc\ndef"}`
		}, 400, CodeInvalidRequest},
		{"token with NUL", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"abc\u0000"}`
		}, 400, CodeInvalidRequest},
		{"empty token", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":""}`
		}, 400, CodeInvalidRequest},
		{"username with colon", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"t","username":"a:b"}`
		}, 400, CodeInvalidRequest},
		{"unknown field", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"t","version":9}`
		}, 400, CodeInvalidRequest},
		{"trailing JSON value", func(t *testing.T, h *harness) (string, string, string, string) {
			return testCredentialID, h.signSubmission(t, h.submission()), testOrigin, `{"token":"t"} {}`
		}, 400, CodeInvalidRequest},
		{"non-canonical credential id", func(t *testing.T, h *harness) (string, string, string, string) {
			return strings.ToUpper(testCredentialID), h.signSubmission(t, h.submission()), testOrigin, body
		}, 400, CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id, authorization, origin, payload := tc.prepare(t, h)
			status, out, header := h.put(t, id, authorization, origin, payload)
			if status != tc.wantStatus || out["code"] != tc.wantCode || len(out) != 1 {
				t.Fatalf("submit = %d %v, want %d {code:%s}", status, out, tc.wantStatus, tc.wantCode)
			}
			if header.Get("Access-Control-Allow-Credentials") != "" {
				t.Fatal("Access-Control-Allow-Credentials must never be set")
			}
		})
	}
}

func TestIngressCORSPreflight(t *testing.T) {
	h := newHarness(t)
	preflight := func(origin, method string) (int, http.Header) {
		request, err := http.NewRequest(http.MethodOptions, h.ingress.URL+"/v1/credentials/"+testCredentialID+"/versions", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("Access-Control-Request-Method", method)
		request.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		status, _, header := do(t, h.httpClient(t), request)
		return status, header
	}
	status, header := preflight(testOrigin, http.MethodPut)
	if status != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", status)
	}
	wantHeaders := map[string]string{
		"Access-Control-Allow-Origin":      testOrigin,
		"Access-Control-Allow-Methods":     "PUT",
		"Access-Control-Allow-Headers":     "Authorization, Content-Type",
		"Access-Control-Allow-Credentials": "",
	}
	for name, want := range wantHeaders {
		if got := header.Get(name); got != want {
			t.Fatalf("preflight %s = %q, want %q", name, got, want)
		}
	}
	if status, header := preflight("https://evil.example", http.MethodPut); status != http.StatusForbidden || header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed preflight = %d with ACAO %q, want 403 and no ACAO", status, header.Get("Access-Control-Allow-Origin"))
	}
	if status, _ := preflight(testOrigin, http.MethodGet); status != http.StatusBadRequest {
		t.Fatalf("preflight for GET = %d, want 400", status)
	}
}

func TestIngressServesNoOtherRoute(t *testing.T) {
	h := newHarness(t)
	h.storeVersion(t, 1)
	for _, target := range []struct{ method, path string }{
		{http.MethodGet, "/v1/credentials/" + testCredentialID + "/versions"},
		{http.MethodGet, "/v1/credentials/" + testCredentialID},
		{http.MethodPost, "/v1/redemptions"},
		{http.MethodPost, "/v1/credentials/" + testCredentialID + "/revoke"},
	} {
		request, err := http.NewRequest(target.method, h.ingress.URL+target.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		status, out, _ := do(t, h.httpClient(t), request)
		if status != http.StatusNotFound || out["code"] != CodeNotFound {
			t.Fatalf("%s %s = %d %v, want 404", target.method, target.path, status, out)
		}
	}
}

func TestIngressRateLimit(t *testing.T) {
	h := newHarness(t, withRate(10, 5))
	send := func() int {
		status, _, _ := h.put(t, testCredentialID, "", testOrigin, `{}`)
		return status
	}
	for i := range 5 {
		if status := send(); status != http.StatusUnauthorized {
			t.Fatalf("request %d = %d, want 401 (within burst)", i+1, status)
		}
	}
	if status := send(); status != http.StatusTooManyRequests {
		t.Fatalf("request over burst = %d, want 429", status)
	}
	// 10 per minute refills one token every 6 seconds.
	h.clock.Advance(5 * time.Second)
	if status := send(); status != http.StatusTooManyRequests {
		t.Fatalf("request before refill = %d, want 429", status)
	}
	h.clock.Advance(time.Second)
	if status := send(); status != http.StatusUnauthorized {
		t.Fatalf("request after refill = %d, want 401", status)
	}
}

func TestRateLimiterIsPerKey(t *testing.T) {
	clock := &testClock{now: testStart}
	limiter, err := newRateLimiter(60, 1, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !limiter.allow("a") || limiter.allow("a") {
		t.Fatal("key a: want one request admitted, the second refused")
	}
	if !limiter.allow("b") {
		t.Fatal("key b must have its own bucket")
	}
	if _, err := newRateLimiter(0, 1, clock.Now); err == nil {
		t.Fatal("a zero rate must be refused")
	}
}
