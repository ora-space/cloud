package cloneexec

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

// DispatcherRefusal is a refusal the clone executor itself reported.
//
// Like the Credential Service's refusals, it is a distinct type so Cloud can tell "the executor
// decided" apart from "the executor could not be reached": the first is an outcome the caller can act
// on, the second is an infrastructure failure, and reporting one as the other would either mislead
// the user or hide an outage.
type DispatcherRefusal struct {
	// Code is the executor's stable reason code.
	Code string
	// Status is the HTTP status the executor answered with.
	Status int
}

// Error reports the refusal without carrying any response body.
func (r *DispatcherRefusal) Error() string {
	return fmt.Sprintf("clone executor refused the job with %q (status %d)", r.Code, r.Status)
}

// ReasonCode names the refusal in a form Cloud can map to its own contract.
func (r *DispatcherRefusal) ReasonCode() string { return r.Code }

// ClientConfig wires Cloud's dispatch path to the executor.
type ClientConfig struct {
	// Endpoint is the executor's HTTPS base URL. Plain HTTP is refused: the authorization crosses this
	// connection, and the client certificate is what carries Cloud's dispatcher identity.
	Endpoint string
	// TLS must present Cloud's `clone-dispatcher` certificate and trust the executor's certificate
	// authority.
	TLS *tls.Config
	// Timeout bounds one dispatch. It must exceed the executor's own clone timeout, or Cloud would
	// abandon clones that are still running and could still succeed — and, because the authorization
	// is spent when the executor redeems it, a clone Cloud abandoned is a clone it cannot retry with
	// the same authority.
	Timeout time.Duration
}

// DefaultDispatchTimeout bounds one dispatch. It is deliberately longer than DefaultCloneTimeout: the
// executor needs room to finish a clone and destroy its material after the clone itself is done.
const DefaultDispatchTimeout = DefaultCloneTimeout + time.Minute

// Client is Cloud's dispatcher.
//
// It carries one job to the executor and returns what the clone produced. It holds no credential
// state: the authorization it sends is minted per job and the token never comes back this way, which
// is what keeps Cloud out of the credential path entirely (the Store decision's D2).
type Client struct {
	endpoint string
	http     *http.Client
}

// NewClient validates the configuration and returns a ready dispatch client.
func NewClient(cfg ClientConfig) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || parsed.Host == "" {
		return nil, errors.New("clone executor endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("clone executor endpoint must use https")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("clone executor endpoint must not carry a query or fragment")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.RootCAs == nil {
		return nil, errors.New("clone executor requires a client certificate and a trusted certificate authority")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultDispatchTimeout
	}
	return &Client{
		endpoint: strings.TrimSuffix(parsed.String(), "/"),
		http:     &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg.TLS}},
	}, nil
}

// Dispatch performs one credentialed clone and returns its result.
//
// There is deliberately no retry. The authorization in the job is spent the moment the executor
// redeems it, so a dispatch that times out or is refused has an unknown outcome: the clone may have
// succeeded, may have failed, or may never have started. Re-sending the same authorization would fail
// as already consumed, and minting a new one without knowing what happened to the first would be
// exactly the "hide an ambiguous result behind a retry" the execution contract forbids. Recovery is
// the caller's, and it begins by reconciling the execution's state.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, and the copy this method encodes is the copy it sends.
func (c *Client) Dispatch(ctx context.Context, job Job) (Result, error) {
	encoded, err := json.Marshal(job)
	if err != nil {
		return Result{}, fmt.Errorf("encode clone job: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/clone-executions", bytes.NewReader(encoded))
	if err != nil {
		return Result{}, fmt.Errorf("build clone dispatch request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("clone executor is unreachable: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxJobBody))
	if err != nil {
		return Result{}, fmt.Errorf("read clone dispatch response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code == "" {
			refusal.Code = ReasonCodeUnavailable
		}
		return Result{}, &DispatcherRefusal{Code: refusal.Code, Status: response.StatusCode}
	}
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, errors.New("clone executor returned an unreadable response")
	}
	if strings.TrimSpace(result.Directory) == "" || strings.TrimSpace(result.Commit) == "" {
		return Result{}, errors.New("clone executor returned no checkout")
	}
	return result, nil
}
