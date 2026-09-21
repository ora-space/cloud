// Package idaas adapts Huawei IDaaS Authorization Code with PKCE to the Gateway's
// provider-neutral Authenticator contract. Provider tokens and profile documents never leave this
// package; callers receive only the stable corporate identity Cloud understands.
package idaas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/gateway"
)

const (
	// Source is the immutable Cloud identity namespace for Huawei corporate accounts.
	Source = "huawei-corp"
	// DefaultBaseURL is the production Huawei IDaaS origin. Deployments still configure the
	// environment explicitly so beta and production cannot be confused accidentally.
	DefaultBaseURL = "https://uniportal.huawei.com"
	// Scope is the fixed minimum IDaaS profile permission used by both authorization and userinfo.
	Scope         = "base.profile"
	authorizePath = "/saaslogin1/oauth2/authorize"
	tokenPath     = "/saaslogin1/oauth2/accesstoken" // #nosec G101 -- endpoint path, not a credential.
	userInfoPath  = "/saaslogin1/oauth2/userinfo"
	maxResponse   = 64 << 10
)

// Options configures one Huawei IDaaS application. DisplayNameField names an optional top-level
// string in the userinfo response; uuid is used when that field is absent, empty, or not a string.
type Options struct {
	BaseURL          string
	ClientID         string
	ClientSecret     string
	DisplayNameField string
	HTTP             *http.Client
}

// Authenticator implements gateway.Authenticator for Huawei IDaaS.
type Authenticator struct {
	baseURL          *url.URL
	clientID         string
	clientSecret     string
	displayNameField string
	http             *http.Client
}

// New validates an IDaaS adapter. The HTTP client must have a total timeout because token and
// userinfo calls run on the interactive login path.
func New(o *Options) (*Authenticator, error) {
	if o == nil || o.ClientID == "" || o.ClientSecret == "" {
		return nil, errors.New("IDaaS client ID and secret are required")
	}
	if o.HTTP == nil || o.HTTP.Timeout <= 0 {
		return nil, errors.New("IDaaS HTTP client with a timeout is required")
	}
	base := o.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, e := url.Parse(base)
	if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("IDaaS base URL must be an HTTP(S) origin")
	}
	if o.DisplayNameField != "" && (strings.TrimSpace(o.DisplayNameField) != o.DisplayNameField || len(o.DisplayNameField) > 128) {
		return nil, errors.New("IDaaS display name field must be a trimmed top-level field name of at most 128 bytes")
	}
	return &Authenticator{baseURL: u, clientID: o.ClientID, clientSecret: o.ClientSecret, displayNameField: o.DisplayNameField, http: o.HTTP}, nil
}

// AuthorizationURL builds the IDaaS authorize redirect and binds state, callback, and PKCE S256.
func (a *Authenticator) AuthorizationURL(request gateway.AuthorizationRequest) (string, error) {
	if request.State == "" || request.CodeChallenge == "" || request.CallbackURL == "" {
		return "", errors.New("state, code challenge and callback URL are required")
	}
	u := *a.baseURL
	u.Path = authorizePath
	q := url.Values{}
	q.Set("client_id", a.clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", request.CallbackURL)
	q.Set("scope", Scope)
	q.Set("display", "page")
	q.Set("state", request.State)
	q.Set("code_challenge", request.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange redeems one code with its PKCE verifier, reads the corporate profile, and discards all
// provider credentials before returning the normalized identity fields.
func (a *Authenticator) Exchange(ctx context.Context, code, codeVerifier, callbackURL string) (gateway.VerifiedIdentity, error) {
	if code == "" || codeVerifier == "" || callbackURL == "" {
		return gateway.VerifiedIdentity{}, fmt.Errorf("%w: code, verifier and callback are required", gateway.ErrProviderRejected)
	}
	token, e := a.exchangeCode(ctx, code, codeVerifier, callbackURL)
	if e != nil {
		return gateway.VerifiedIdentity{}, e
	}
	return a.readUser(ctx, token)
}

type tokenRequest struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ErrorCode   string `json:"errorCode"`
}

func (a *Authenticator) exchangeCode(ctx context.Context, code, verifier, callbackURL string) (string, error) {
	in := tokenRequest{ClientID: a.clientID, ClientSecret: a.clientSecret, RedirectURI: callbackURL, GrantType: "authorization_code", Code: code, CodeVerifier: verifier}
	var out tokenResponse
	status, e := a.postJSON(ctx, tokenPath, in, &out)
	if e != nil {
		return "", e
	}
	if status != http.StatusOK || out.ErrorCode != "" || out.AccessToken == "" {
		return "", fmt.Errorf("%w: token exchange rejected", gateway.ErrProviderRejected)
	}
	return out.AccessToken, nil
}

type userInfoRequest struct {
	ClientID    string `json:"client_id"`
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
}

func (a *Authenticator) readUser(ctx context.Context, token string) (gateway.VerifiedIdentity, error) {
	var out map[string]json.RawMessage
	status, e := a.postJSON(ctx, userInfoPath, userInfoRequest{ClientID: a.clientID, AccessToken: token, Scope: Scope}, &out)
	if e != nil {
		return gateway.VerifiedIdentity{}, e
	}
	if status != http.StatusOK || rawString(out["errorCode"]) != "" {
		return gateway.VerifiedIdentity{}, fmt.Errorf("%w: userinfo rejected", gateway.ErrProviderRejected)
	}
	uuid := rawString(out["uuid"])
	if uuid == "" || strings.TrimSpace(uuid) != uuid {
		return gateway.VerifiedIdentity{}, fmt.Errorf("%w: missing stable uuid", gateway.ErrProviderRejected)
	}
	name := uuid
	if configured := strings.TrimSpace(rawString(out[a.displayNameField])); a.displayNameField != "" && configured != "" {
		name = configured
	}
	return gateway.VerifiedIdentity{Source: Source, Subject: uuid, DisplayName: name}, nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func (a *Authenticator) postJSON(ctx context.Context, path string, in, out any) (int, error) {
	body, e := json.Marshal(in)
	if e != nil {
		return 0, fmt.Errorf("encode IDaaS request: %w", e)
	}
	u := *a.baseURL
	u.Path = path
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if e != nil {
		return 0, fmt.Errorf("build IDaaS request: %w", e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, e := a.http.Do(req)
	if e != nil {
		return 0, fmt.Errorf("IDaaS request: %w", e)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, e := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if e != nil {
		return 0, fmt.Errorf("read IDaaS response: %w", e)
	}
	if len(responseBody) > maxResponse {
		return 0, errors.New("IDaaS response exceeds size limit")
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return resp.StatusCode, fmt.Errorf("IDaaS unavailable with status %d", resp.StatusCode)
	}
	if e = json.Unmarshal(responseBody, out); e != nil {
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, errors.New("IDaaS returned malformed JSON")
	}
	return resp.StatusCode, nil
}

// NewHTTPClient returns a bounded provider client that refuses redirects from token and userinfo
// endpoints. Browser navigation, not this client, owns the authorize redirect chain.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
