package idaas

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/gateway"
)

type recordedRequests struct {
	token map[string]string
	user  map[string]string
}

func readForm(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	if r.Header.Get("Authorization") != "" {
		t.Errorf("client_secret_post must not send an Authorization header, got %q", r.Header.Get("Authorization"))
	}
	if r.URL.RawQuery != "" {
		t.Errorf("client_secret_post must not put credentials in the query, got %q", r.URL.RawQuery)
	}
	if e := r.ParseForm(); e != nil {
		t.Error(e)
		return nil
	}
	out := map[string]string{}
	for key, values := range r.PostForm {
		if len(values) != 1 {
			t.Errorf("%s has %d values", key, len(values))
		}
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func testAdapter(t *testing.T, tokenStatus int, tokenBody string, userStatus int, userBody string) (*Authenticator, *recordedRequests) {
	t.Helper()
	recorded := &recordedRequests{}
	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected token request: %s %q %q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Accept"))
		}
		recorded.token = readForm(t, r)
		w.WriteHeader(tokenStatus)
		_, _ = w.Write([]byte(tokenBody))
	})
	mux.HandleFunc(userInfoPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("unexpected userinfo request: %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		recorded.user = readForm(t, r)
		w.WriteHeader(userStatus)
		_, _ = w.Write([]byte(userBody))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	a, e := New(&Options{BaseURL: server.URL, ClientID: "client", ClientSecret: "secret-value", DisplayNameField: "userName", HTTP: NewHTTPClient(2 * time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	return a, recorded
}

func TestAuthorizationURLBindsDocumentedIDaaSParameters(t *testing.T) {
	a, _ := testAdapter(t, 200, `{}`, 200, `{}`)
	raw, e := a.AuthorizationURL(gateway.AuthorizationRequest{State: "state-1", CodeChallenge: "challenge-1", CallbackURL: "https://cloud.huawei.com/auth/callback/huawei-idaas"})
	if e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	if u.Path != authorizePath {
		t.Fatalf("authorize path = %q want %q", u.Path, authorizePath)
	}
	want := map[string]string{
		"client_id": "client", "response_type": "code", "redirect_uri": "https://cloud.huawei.com/auth/callback/huawei-idaas",
		"scope": Scope, "state": "state-1",
	}
	got := map[string]string{}
	for key, values := range u.Query() {
		if len(values) != 1 {
			t.Errorf("%s has %d values", key, len(values))
		}
		got[key] = u.Query().Get(key)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("authorize query = %v want %v", got, want)
	}
	if strings.Contains(raw, "secret-value") || strings.Contains(raw, "challenge-1") {
		t.Fatal("authorize URL must not contain the client secret or PKCE challenge")
	}
	if _, e = a.AuthorizationURL(gateway.AuthorizationRequest{State: "state-1", CodeChallenge: "challenge-1", CallbackURL: "https://cloud.huawei.com/cb"}); e != nil {
		t.Fatalf("documented authorize parameters must be sufficient: %v", e)
	}
	if _, e = a.AuthorizationURL(gateway.AuthorizationRequest{State: "state-1"}); e == nil {
		t.Fatal("missing callback must be rejected")
	}
}

func TestExchangeUsesClientSecretPostAndReturnsCorporateIdentity(t *testing.T) {
	a, recorded := testAdapter(t, 200, `{"access_token":"provider-token","token_type":"Bearer","refresh_token":"discard-me","expires_in":"1800"}`, 200, `{"uuid":" uuid~dGVzdDE = ","userName":"  Wang Longan  ","globalUserID":"174022309561388","tenantId":"111","employeeNumber":"30000000","email":"private@example.com"}`)
	identity, e := a.Exchange(context.Background(), "authorization-code", "pkce-verifier", "https://cloud.huawei.com/auth/callback/huawei-idaas")
	if e != nil {
		t.Fatal(e)
	}
	want := gateway.VerifiedIdentity{Source: Source, Subject: "uuid~dGVzdDE =", DisplayName: "Wang Longan"}
	if identity != want {
		t.Fatalf("identity = %+v want %+v", identity, want)
	}
	wantToken := map[string]string{
		"client_id": "client", "client_secret": "secret-value",
		"grant_type": "authorization_code", "code": "authorization-code",
	}
	if !maps.Equal(recorded.token, wantToken) {
		t.Fatalf("token request = %v want client_secret_post fields", recorded.token)
	}
	wantUser := map[string]string{"access_token": "provider-token"}
	if !maps.Equal(recorded.user, wantUser) {
		t.Fatalf("userinfo request = %v want access_token only", recorded.user)
	}
}

func TestExchangeFallsBackToUUIDAndRejectsProviderAnswers(t *testing.T) {
	cases := []struct {
		name                          string
		tokenStatus, userStatus       int
		tokenBody, userBody           string
		want                          gateway.VerifiedIdentity
		providerRejected, unavailable bool
	}{
		{"display field absent", 200, 200, `{"access_token":"tok","token_type":"Bearer"}`, `{"uuid":"w1","email":"ignored@example.com"}`, gateway.VerifiedIdentity{Source: Source, Subject: "w1", DisplayName: "w1"}, false, false},
		{"display field wrong type", 200, 200, `{"access_token":"tok"}`, `{"uuid":"w2","userName":42}`, gateway.VerifiedIdentity{Source: Source, Subject: "w2", DisplayName: "w2"}, false, false},
		{"token oauth error", 200, 200, `{"error":"invalid_request","error_description":"secret provider detail"}`, `{}`, gateway.VerifiedIdentity{}, true, false},
		{"token error code", 200, 200, `{"errorCode":"E_10009","errorDesc":"secret provider detail"}`, `{}`, gateway.VerifiedIdentity{}, true, false},
		{"non-bearer token", 200, 200, `{"access_token":"tok","token_type":"mac"}`, `{}`, gateway.VerifiedIdentity{}, true, false},
		{"userinfo oauth error", 200, 200, `{"access_token":"tok"}`, `{"error":"invalid_request"}`, gateway.VerifiedIdentity{}, true, false},
		{"userinfo error code", 200, 200, `{"access_token":"tok"}`, `{"errorCode":"E_10012"}`, gateway.VerifiedIdentity{}, true, false},
		{"missing uuid", 200, 200, `{"access_token":"tok"}`, `{"userName":"Name"}`, gateway.VerifiedIdentity{}, true, false},
		{"blank uuid", 200, 200, `{"access_token":"tok"}`, `{"uuid":"   "}`, gateway.VerifiedIdentity{}, true, false},
		{"padded uuid", 200, 200, `{"access_token":"tok"}`, `{"uuid":" w3 "}`, gateway.VerifiedIdentity{Source: Source, Subject: "w3", DisplayName: "w3"}, false, false},
		{"documented padded uuid", 200, 200, `{"access_token":"tok"}`, `{"uuid":"uuid~dGVzdDE = "}`, gateway.VerifiedIdentity{Source: Source, Subject: "uuid~dGVzdDE =", DisplayName: "uuid~dGVzdDE ="}, false, false},
		{"token 401", 401, 200, `{}`, `{}`, gateway.VerifiedIdentity{}, true, false},
		{"provider 429", 429, 200, `{}`, `{}`, gateway.VerifiedIdentity{}, false, true},
		{"provider 500", 500, 200, `{}`, `{}`, gateway.VerifiedIdentity{}, false, true},
		{"malformed success", 200, 200, `not-json`, `{}`, gateway.VerifiedIdentity{}, true, false},
		{"oversized success", 200, 200, `{"access_token":"` + strings.Repeat("x", maxResponse) + `"}`, `{}`, gateway.VerifiedIdentity{}, true, false},
	}
	for _, tc := range cases {
		a, _ := testAdapter(t, tc.tokenStatus, tc.tokenBody, tc.userStatus, tc.userBody)
		got, e := a.Exchange(context.Background(), "sensitive-authorization-code", "sensitive-pkce-verifier", "https://cloud.huawei.com/auth/callback/huawei-idaas")
		switch {
		case tc.providerRejected && !errors.Is(e, gateway.ErrProviderRejected):
			t.Errorf("%s: expected provider rejection, got %v", tc.name, e)
		case tc.unavailable && (e == nil || errors.Is(e, gateway.ErrProviderRejected)):
			t.Errorf("%s: expected infrastructure failure, got %v", tc.name, e)
		case !tc.providerRejected && !tc.unavailable && (e != nil || got != tc.want):
			t.Errorf("%s: got (%+v,%v), want %+v", tc.name, got, e, tc.want)
		}
		if e != nil && (strings.Contains(e.Error(), "secret provider detail") || strings.Contains(e.Error(), "secret-value") || strings.Contains(e.Error(), "sensitive-authorization-code") || strings.Contains(e.Error(), "sensitive-pkce-verifier")) {
			t.Errorf("%s: error leaked provider detail: %v", tc.name, e)
		}
	}
}

func TestExchangeBoundsResponsesAndPropagatesCancellation(t *testing.T) {
	a, _ := testAdapter(t, 200, `{"access_token":"`+strings.Repeat("x", maxResponse)+`"}`, 200, `{}`)
	if _, e := a.Exchange(context.Background(), "code", "verifier", "https://cloud.huawei.com/cb"); !errors.Is(e, gateway.ErrProviderRejected) {
		t.Fatalf("oversized response must be a provider rejection: %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := a.Exchange(ctx, "code", "verifier", "https://cloud.huawei.com/cb"); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation must propagate: %v", e)
	}
}

func TestHTTPClientTimesOutAndRefusesProviderRedirects(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	a, e := New(&Options{BaseURL: redirector.URL, ClientID: "client", ClientSecret: "secret", HTTP: NewHTTPClient(time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.Exchange(context.Background(), "code", "verifier", "https://cloud.huawei.com/cb"); !errors.Is(e, gateway.ErrProviderRejected) {
		t.Fatalf("redirect response must be rejected, got %v", e)
	}
	if redirected.Load() != 0 {
		t.Fatal("provider client followed a redirect")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(blocking.Close)
	a, e = New(&Options{BaseURL: blocking.URL, ClientID: "client", ClientSecret: "secret", HTTP: NewHTTPClient(20 * time.Millisecond)})
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.Exchange(context.Background(), "code", "verifier", "https://cloud.huawei.com/cb")
	<-started
	close(release)
	if e == nil || errors.Is(e, gateway.ErrProviderRejected) {
		t.Fatalf("provider timeout must be an infrastructure failure, got %v", e)
	}
}

func TestNewRejectsUnsafeOrIncompleteOptions(t *testing.T) {
	cases := []Options{
		{ClientSecret: "secret", HTTP: NewHTTPClient(time.Second)},
		{ClientID: "client", HTTP: NewHTTPClient(time.Second)},
		{ClientID: "client", ClientSecret: "secret", HTTP: &http.Client{}},
		{BaseURL: "https://user:password@example.com", ClientID: "client", ClientSecret: "secret", HTTP: NewHTTPClient(time.Second)},
		{BaseURL: "https://example.com/path", ClientID: "client", ClientSecret: "secret", HTTP: NewHTTPClient(time.Second)},
		{ClientID: "client", ClientSecret: "secret", DisplayNameField: " userName ", HTTP: NewHTTPClient(time.Second)},
	}
	for i := range cases {
		if _, e := New(&cases[i]); e == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}
