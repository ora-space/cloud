package idaas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/gateway"
)

func TestDeploymentEndpointsUseTLSAndPreserveProtocol(t *testing.T) {
	var tokenCalls, userCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		form := readForm(t, r)
		if r.Method != http.MethodPost {
			t.Error("provider request must remain POST")
		}
		switch r.URL.Path {
		case "/custom/token":
			tokenCalls++
			if form["client_secret"] != "synthetic-secret" || form["code"] != "one-code" || form["grant_type"] != "authorization_code" {
				t.Error("token protocol changed")
			}
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token"}`))
		case "/custom/profile":
			userCalls++
			if len(form) != 1 || form["access_token"] != "synthetic-token" {
				t.Error("userinfo protocol changed")
			}
			_, _ = w.Write([]byte(`{"uuid":"employee-uuid","userName":"Example","globalUserID":"174022309561388"}`))
		default:
			t.Error("request used default rather than deployment endpoint")
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := NewHTTPClient(time.Second)
	client.Transport = server.Client().Transport
	a, err := New(&Options{
		BaseURL: "https://unused.invalid", AuthorizeURL: "https://sso.example.org/custom/authorize",
		TokenURL: server.URL + "/custom/token", UserInfoURL: server.URL + "/custom/profile",
		ClientID: "app", ClientSecret: "synthetic-secret", DisplayNameField: "userName", HTTP: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.AuthorizationURL(gateway.AuthorizationRequest{State: "state", CallbackURL: "https://ora.example.org/auth/callback/huawei-idaas"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Host != "sso.example.org" || u.Path != "/custom/authorize" || u.Query().Get("state") != "state" || u.Query().Get("scope") != Scope {
		t.Fatal("authorize override lost protocol bindings")
	}
	identity, err := a.Exchange(context.Background(), "one-code", "", "https://ora.example.org/auth/callback/huawei-idaas")
	if err != nil || identity.Source != Source || identity.Subject != "employee-uuid" || tokenCalls != 1 || userCalls != 1 {
		t.Fatalf("custom endpoints did not complete the existing identity flow: %v", err)
	}
}

func TestEndpointOverridesRejectUnsafeURLs(t *testing.T) {
	for _, raw := range []string{
		"http://sso.example.org/token", "/token", "https://user:secret@sso.example.org/token",
		"https://sso.example.org/token?secret=value", "https://sso.example.org/token?", "https://sso.example.org/token#fragment", "https://sso.example.org/token#", "https://",
	} {
		for _, field := range []string{"authorize", "token", "userinfo"} {
			o := &Options{ClientID: "app", ClientSecret: "synthetic-secret", HTTP: NewHTTPClient(time.Second)}
			switch field {
			case "authorize":
				o.AuthorizeURL = raw
			case "token":
				o.TokenURL = raw
			case "userinfo":
				o.UserInfoURL = raw
			}
			if _, err := New(o); err == nil {
				t.Errorf("unsafe %s endpoint accepted", field)
			}
		}
	}
}
