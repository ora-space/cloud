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

// ReasonCodeInvalidRequest and the codes below are the refusal codes the Store reports on the wire.
// They are duplicated here as the client's vocabulary so Cloud maps a refusal to a stable public
// reason code without parsing error text.
const (
	ReasonCodeInvalidRequest  = "invalid_request"
	ReasonCodeRevoked         = "credential_revoked"
	ReasonCodeConflict        = "credential_conflict"
	ReasonCodeNotFound        = "credential_not_found"
	ReasonCodeUnavailable     = "store_unavailable"
	ReasonCodeIdentityRefused = "service_identity_required"
)

// Failure is a refusal the Credential Service itself reported.
//
// It is a distinct type because D6 and D7 require the caller to tell "the Store refused this
// credential" apart from "the Store could not be reached": the first is a stable, explainable
// outcome for the end user, the second is an infrastructure failure. Treating an unreachable Store
// as a refusal — or the reverse — would either mislead the user or silently downgrade the security
// boundary.
type Failure struct {
	// Code is the Store's stable reason code.
	Code string
	// Status is the HTTP status the Store answered with.
	Status int
}

// Error reports the refusal without carrying any response body: error text travels into logs, and
// nothing the Store returns beyond the code is safe to repeat.
func (f *Failure) Error() string {
	return fmt.Sprintf("credential service refused the request with %q (status %d)", f.Code, f.Status)
}

// ReasonCode names the refusal in a form a caller can map to its own public contract.
func (f *Failure) ReasonCode() string { return f.Code }

// ClientConfig wires Cloud's credential ingress to the Store.
type ClientConfig struct {
	// Endpoint is the Store's HTTPS base URL. Plain HTTP is refused: the token crosses this
	// connection in the clear only if TLS is misconfigured, and the client certificate is what
	// carries the service identity (D3).
	Endpoint string
	// TLS must present Cloud's `ingress-writer` certificate and trust the Store's certificate
	// authority.
	TLS *tls.Config
	// Timeout bounds one handoff. The ingress holds no transaction while it runs, but an unbounded
	// wait would turn an unreachable Store into a stuck request rather than an explicit failure (D6).
	Timeout time.Duration
}

// DefaultIngressTimeout bounds one credential handoff.
const DefaultIngressTimeout = 10 * time.Second

// Client is Cloud's ingress-writer. It holds no secret state: each call carries one token to the
// Store and returns nothing but the opaque reference.
//
// There is deliberately no retry. A create or rotate that times out may have succeeded, and
// re-sending it would be exactly the "hide a distributed-consistency problem behind a retry" that
// D4 forbids; recovery is the caller re-issuing the whole request under the same idempotency key.
type Client struct {
	endpoint string
	http     *http.Client
}

// NewClient validates the configuration and returns a ready ingress client.
func NewClient(cfg ClientConfig) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("credential service endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("credential service endpoint must use https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("credential service endpoint must not carry a query or fragment")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.RootCAs == nil {
		return nil, errors.New("credential service requires a client certificate and a trusted certificate authority")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultIngressTimeout
	}
	return &Client{
		endpoint: strings.TrimSuffix(parsed.String(), "/"),
		http:     &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg.TLS}},
	}, nil
}

// Write hands one token to the Store for a credential Cloud has already identified.
//
// credentialID is Cloud's `credential_refs.id` and doubles as the cross-store idempotency key
// (D4): re-issuing the same request returns the original reference instead of creating a second
// secret. The returned reference is opaque to Cloud — D1 puts generation in the Store.
func (c *Client) Write(ctx context.Context, credentialID, token string) (string, error) {
	var out struct {
		SecretRef string `json:"secretRef"`
	}
	request := map[string]string{"credentialId": credentialID, "token": token}
	if err := c.call(ctx, http.MethodPost, "/v1/credentials", request, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.SecretRef) == "" {
		return "", errors.New("credential service returned no secret reference")
	}
	return out.SecretRef, nil
}

// Rotate replaces the material of an existing credential. D4 requires this to happen before Cloud
// advances its metadata pointer, so the previous version stays readable for a bounded grace period.
func (c *Client) Rotate(ctx context.Context, credentialID, token string) error {
	return c.call(ctx, http.MethodPost, "/v1/credentials/"+credentialID+"/rotate", map[string]string{"token": token}, nil)
}

// Revoke destroys the material of a credential Cloud has already marked unusable.
//
// D4 orders revocation the other way round from rotation: Cloud's metadata change is authoritative
// and lands first, so a failure here must not be reported as a failed revocation. Callers treat an
// error as "residual material to reconcile", not as "the credential is still usable".
func (c *Client) Revoke(ctx context.Context, credentialID string) error {
	return c.call(ctx, http.MethodPost, "/v1/credentials/"+credentialID+"/revoke", struct{}{}, nil)
}

// call performs one request and decodes either the response or the Store's refusal code.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		// The request bodies here are a fixed shape holding two strings; a marshal failure means a
		// programming error, not caller input.
		return fmt.Errorf("encode credential service request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build credential service request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// The error is wrapped so a caller can distinguish "unreachable" from "refused"; the URL is
		// in it, and a URL never carries a token because tokens only ever travel in the body.
		return fmt.Errorf("credential service is unreachable: %w", err)
	}
	defer response.Body.Close()
	// The response is read with a bound so a compromised or broken Store cannot exhaust Cloud's
	// memory, and it is never logged: it is the peer's text, and D7 keeps peer text out of logs.
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBody))
	if err != nil {
		return fmt.Errorf("read credential service response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code == "" {
			refusal.Code = ReasonCodeUnavailable
		}
		return &Failure{Code: refusal.Code, Status: response.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("credential service returned an unreadable response")
	}
	return nil
}
