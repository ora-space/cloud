package credentialstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client-side failure classes. A *RefusalError unwraps to one of these, so callers branch with
// errors.Is and never parse text.
var (
	// ErrUnavailable reports that the Store could not be reached, answered 5xx, or answered
	// something unreadable. The outcome of the call is unknown to the caller.
	ErrUnavailable = errors.New("credential store unavailable")
	// ErrAlreadyRedeemed reports that the authorization was redeemed before.
	ErrAlreadyRedeemed = errors.New("credential store: authorization already redeemed")
	// ErrInvalidAuthorization reports an authorization the Store refused to verify.
	ErrInvalidAuthorization = errors.New("credential store: invalid authorization")
	// ErrCredentialNotFound reports that no version of the credential was ever stored.
	ErrCredentialNotFound = errors.New("credential store: credential not found")
	// ErrCredentialUnavailable reports a revoked credential or a version past retention.
	ErrCredentialUnavailable = errors.New("credential store: credential unavailable")
	// ErrIdentityRefused reports that the Store refused this client's certificate identity.
	ErrIdentityRefused = errors.New("credential store: client identity refused")
	// ErrRejected reports any other refusal (malformed request and the like).
	ErrRejected = errors.New("credential store: request rejected")
)

// maxResponseBody bounds every response the clients read.
const maxResponseBody = 64 << 10

// DefaultClientTimeout bounds one call when ClientConfig.Timeout is zero.
const DefaultClientTimeout = 10 * time.Second

// RefusalError is a refusal the Store reported with a status and a stable code.
type RefusalError struct {
	Status int
	Code   string
}

// Error reports the status and code only; response bodies never enter error text.
func (e *RefusalError) Error() string {
	return fmt.Sprintf("credential store refused the request: %s (status %d)", e.Code, e.Status)
}

// Unwrap maps the code to its failure class.
func (e *RefusalError) Unwrap() error {
	switch {
	case e.Code == CodeAlreadyRedeemed:
		return ErrAlreadyRedeemed
	case e.Code == CodeInvalidAuthorization:
		return ErrInvalidAuthorization
	case e.Code == CodeCredentialNotFound:
		return ErrCredentialNotFound
	case e.Code == CodeCredentialUnavailable:
		return ErrCredentialUnavailable
	case e.Code == CodeIdentityRequired || e.Code == CodeIdentityForbidden || e.Code == CodeExecutorMismatch:
		return ErrIdentityRefused
	case e.Status >= http.StatusInternalServerError:
		return ErrUnavailable
	default:
		return ErrRejected
	}
}

// ClientConfig configures a management or redemption client.
type ClientConfig struct {
	// BaseURL is the listener's https base URL, without query or fragment.
	BaseURL string
	// TLS presents the caller's service certificate and trusts the Store's CA. MinVersion is
	// raised to TLS 1.3.
	TLS *tls.Config
	// Timeout bounds one call; zero selects DefaultClientTimeout.
	Timeout time.Duration
}

// client is the shared mTLS transport of both clients.
type client struct {
	base string
	http *http.Client
}

func newClient(cfg *ClientConfig) (*client, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("credential store base URL must be an https URL without credentials, query or fragment")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.RootCAs == nil {
		return nil, errors.New("credential store client requires a client certificate and a trusted CA")
	}
	tlsConfig := cfg.TLS.Clone()
	tlsConfig.MinVersion = tls.VersionTLS13
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultClientTimeout
	}
	return &client{
		base: strings.TrimSuffix(parsed.String(), "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, ForceAttemptHTTP2: true},
			// A redirect would send the request, possibly with an authorization, somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// do performs one call and decodes the 200 body into out.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode credential store request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("build credential store request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil || len(raw) > maxResponseBody {
		return fmt.Errorf("%w: response unreadable or too large", ErrUnavailable)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &refusal) != nil || refusal.Code == "" {
			if response.StatusCode >= http.StatusInternalServerError {
				return fmt.Errorf("%w: status %d", ErrUnavailable, response.StatusCode)
			}
			return &RefusalError{Status: response.StatusCode, Code: CodeInvalidRequest}
		}
		return &RefusalError{Status: response.StatusCode, Code: refusal.Code}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: response is not the expected document", ErrUnavailable)
	}
	return nil
}

// ManagementClient is Cloud's client of the management listener. It presents the credential
// manager certificate and can only read metadata and revoke.
type ManagementClient struct{ c *client }

// NewManagementClient returns a ready management client.
func NewManagementClient(cfg *ClientConfig) (*ManagementClient, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &ManagementClient{c: c}, nil
}

// Get returns the metadata of one credential. An unknown id is a zero CurrentVersion, not an
// error.
func (m *ManagementClient) Get(ctx context.Context, credentialID string) (Metadata, error) {
	if !validUUID(credentialID) {
		return Metadata{}, errors.New("credential id is not a canonical UUID")
	}
	var out Metadata
	if err := m.c.do(ctx, http.MethodGet, "/v1/credentials/"+credentialID, nil, &out); err != nil {
		return Metadata{}, err
	}
	if out.CredentialID != credentialID || out.CurrentVersion < 0 {
		return Metadata{}, fmt.Errorf("%w: metadata does not describe the requested credential", ErrUnavailable)
	}
	return out, nil
}

// Revoke revokes one credential and destroys its ciphertexts. It is idempotent, so Cloud retries
// it until it succeeds.
func (m *ManagementClient) Revoke(ctx context.Context, credentialID string) error {
	if !validUUID(credentialID) {
		return errors.New("credential id is not a canonical UUID")
	}
	var out struct {
		Revoked bool `json:"revoked"`
	}
	if err := m.c.do(ctx, http.MethodPost, "/v1/credentials/"+credentialID+"/revoke", struct{}{}, &out); err != nil {
		return err
	}
	if !out.Revoked {
		return fmt.Errorf("%w: revocation was not confirmed", ErrUnavailable)
	}
	return nil
}

// RedemptionClient is the Repo Executor's client of the redemption listener. It presents the
// executor certificate.
type RedemptionClient struct{ c *client }

// NewRedemptionClient returns a ready redemption client.
func NewRedemptionClient(cfg *ClientConfig) (*RedemptionClient, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &RedemptionClient{c: c}, nil
}

// Credential is a redeemed plaintext credential. Its String and GoString methods redact, so an
// accidental log statement cannot print it; callers must still keep it out of logs and errors.
type Credential struct {
	Username string
	Token    string
	Version  int64
}

// String redacts the credential.
func (c Credential) String() string {
	return fmt.Sprintf("credentialstore.Credential{Version: %d, [redacted]}", c.Version)
}

// GoString redacts the credential for %#v.
func (c Credential) GoString() string { return c.String() }

// Redeem exchanges one execution authorization for the plaintext it names. A second call with the
// same authorization fails with ErrAlreadyRedeemed; a transport failure is ErrUnavailable, after
// which the authorization may or may not have been spent.
func (r *RedemptionClient) Redeem(ctx context.Context, authorization string) (Credential, error) {
	var out redemptionResponse
	if err := r.c.do(ctx, http.MethodPost, "/v1/redemptions", redemptionRequest{Authorization: authorization}, &out); err != nil {
		return Credential{}, err
	}
	if out.Token == "" || out.Username == "" || out.Version < 1 {
		return Credential{}, fmt.Errorf("%w: redemption response is incomplete", ErrUnavailable)
	}
	return Credential(out), nil
}
