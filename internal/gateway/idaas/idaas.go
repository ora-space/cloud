// Package idaas adapts Huawei IDaaS 2.0 Authorization Code (client_secret_post) to the Gateway's
// provider-neutral Authenticator contract. Provider tokens and profile documents never leave this
// package; callers receive only the stable corporate identity Cloud understands.
package idaas

import (
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
	authorizePath = "/saaslogin1/oauth2/v1/authorize"
	tokenPath     = "/saaslogin1/oauth2/v1/token" // #nosec G101 -- endpoint path, not a credential.
	userInfoPath  = "/saaslogin1/oauth2/v1/userinfo"
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

// AuthorizationURL builds the IDaaS 2.0 authorize redirect. PKCE is omitted because that mode is
// off by default and this adapter authenticates as a confidential client with client_secret_post.
// display is omitted so IDaaS keeps its adaptive default.
func (a *Authenticator) AuthorizationURL(request gateway.AuthorizationRequest) (string, error) {
	if request.State == "" || request.CallbackURL == "" {
		return "", errors.New("state and callback URL are required")
	}
	u := *a.baseURL
	u.Path = authorizePath
	q := url.Values{}
	q.Set("client_id", a.clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", request.CallbackURL)
	q.Set("scope", Scope)
	q.Set("state", request.State)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange redeems one code with client_secret_post, reads the corporate profile, and discards all
// provider credentials. The PKCE verifier argument exists because the Authenticator interface is
// provider-neutral; IDaaS 2.0 PKCE mode is not used here. callbackURL is required by that same
// interface and was already bound at authorize; the documented client_secret_post token request
// does not repeat it.
func (a *Authenticator) Exchange(ctx context.Context, code, _, callbackURL string) (gateway.VerifiedIdentity, error) {
	if code == "" || callbackURL == "" {
		return gateway.VerifiedIdentity{}, fmt.Errorf("%w: code and callback are required", gateway.ErrProviderRejected)
	}
	token, e := a.exchangeCode(ctx, code)
	if e != nil {
		return gateway.VerifiedIdentity{}, e
	}
	return a.readUser(ctx, token)
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorCode   string `json:"errorCode"`
}

func (a *Authenticator) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)
	form.Set("code", code)
	var out tokenResponse
	status, e := a.postForm(ctx, tokenPath, form, &out)
	if e != nil {
		return "", e
	}
	if status != http.StatusOK || out.Error != "" || out.ErrorCode != "" || out.AccessToken == "" {
		return "", fmt.Errorf("%w: token exchange rejected", gateway.ErrProviderRejected)
	}
	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer") {
		return "", fmt.Errorf("%w: token exchange rejected", gateway.ErrProviderRejected)
	}
	return out.AccessToken, nil
}

func (a *Authenticator) readUser(ctx context.Context, token string) (gateway.VerifiedIdentity, error) {
	form := url.Values{}
	form.Set("access_token", token)
	var out map[string]json.RawMessage
	status, e := a.postForm(ctx, userInfoPath, form, &out)
	if e != nil {
		return gateway.VerifiedIdentity{}, e
	}
	if status != http.StatusOK || rawString(out["error"]) != "" || rawString(out["errorCode"]) != "" {
		return gateway.VerifiedIdentity{}, fmt.Errorf("%w: userinfo rejected", gateway.ErrProviderRejected)
	}
	// IDaaS 2.0 userinfo examples pad uuid with trailing spaces. Leading/trailing
	// whitespace is not identity-bearing; trim it like display names. Internal
	// spaces stay so the opaque key is otherwise unchanged.
	uuid := strings.TrimSpace(rawString(out["uuid"]))
	if uuid == "" {
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

func (a *Authenticator) postForm(ctx context.Context, path string, form url.Values, out any) (int, error) {
	u := *a.baseURL
	u.Path = path
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if e != nil {
		return 0, fmt.Errorf("build IDaaS request: %w", e)
	}
	// IDaaS 2.0 examples render the same fields on the query string, but they also
	// require Content-Type: application/x-www-form-urlencoded, which only describes
	// the entity-body. RFC 6749 §2.3.1 / §4.1.3 client_secret_post and the GitHub
	// adapter put credentials in the body so client_secret and access_token never
	// appear in URLs or access logs. Query placement and client_secret_basic are
	// rejected alternatives: the former leaks secrets, the latter fails when the
	// registered app expects client_secret_post (error=invalid_request).
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return 0, fmt.Errorf("%w: IDaaS response exceeds size limit", gateway.ErrProviderRejected)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return resp.StatusCode, fmt.Errorf("IDaaS unavailable with status %d", resp.StatusCode)
	}
	if e = json.Unmarshal(responseBody, out); e != nil {
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil
		}
		return resp.StatusCode, fmt.Errorf("%w: IDaaS returned malformed JSON", gateway.ErrProviderRejected)
	}
	return resp.StatusCode, nil
}

// NewHTTPClient returns a bounded provider client that refuses redirects from token and userinfo
// endpoints. Browser navigation, not this client, owns the authorize redirect chain.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
