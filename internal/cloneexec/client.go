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

// Push asks the executor to perform one controlled push and returns what it established.
//
// It is a third method rather than a mode on either of the others, for the reason PrepareMirror is a
// second one: the three jobs are different subjects on different routes, and a client that took all
// three would be a client whose caller decides which identity fields matter.
//
// The distinction this method has to preserve, and the reason it is written out rather than folded into
// the two above, is that *the result is not the same kind of thing as a clone's*. A clone either
// produced a checkout or did not. A push can have happened without the caller learning that it did, so:
//
//   - a non-200 carrying a code is the executor saying this job never reached the write. Cloud settles
//     that as a confirmed failure with that code, which is faithful only because the executor's own
//     contract is that a job which reached the write answers 200 with a state.
//   - a transport failure, a timeout, a killed connection, a 200 whose body cannot be read, or a non-200
//     carrying no code at all is *not* an outcome. Each is returned as a plain error with no refusal
//     attached, and the caller must settle the request as uncertain. Returning a refusal here would
//     record a possible write as a failure.
//   - a 200 whose state is not one of the five is treated the same way. An unrecognized state is not a
//     state, and reading it as "not applied" would be inferring an outcome the executor did not state.
//
// There is no retry, and for a push the reason is stronger than for the other two: the write may have
// landed, so a second attempt under a new authorization would be a second write to the same ref, which
// is exactly what the compare-and-swap exists to prevent.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, and the copy this method encodes is the copy it sends.
func (c *Client) Push(ctx context.Context, job PushJob) (PushResult, error) {
	encoded, err := json.Marshal(job)
	if err != nil {
		return PushResult{}, fmt.Errorf("encode push job: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/push-executions", bytes.NewReader(encoded))
	if err != nil {
		return PushResult{}, fmt.Errorf("build push dispatch request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return PushResult{}, fmt.Errorf("clone executor is unreachable: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxJobBody))
	if err != nil {
		return PushResult{}, fmt.Errorf("read push dispatch response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		// Only a code the executor itself wrote is the executor's refusal, and only that is returned as
		// one. A non-200 whose body cannot be read as a code — a gateway's error page, a truncated reply,
		// an empty body — is not evidence about the job at all: the request may have reached the executor
		// and the write may have happened, and an intermediary's answer cannot rule that out. It is
		// therefore returned as a plain error with no refusal attached, so the caller settles the request
		// as uncertain. Substituting a code here would do the opposite of what this method exists for: it
		// would let a reply no component vouched for settle a possible write as a concluded failure. The
		// other two routes keep the substitution, because neither of them can have written anything.
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code == "" {
			return PushResult{}, fmt.Errorf("the push route answered %d with no reason code", response.StatusCode)
		}
		return PushResult{}, &DispatcherRefusal{Code: refusal.Code, Status: response.StatusCode}
	}
	var result PushResult
	if err := json.Unmarshal(raw, &result); err != nil {
		// The executor answered, so this request *ran*, and the answer is unreadable. Whatever the write
		// did, this client cannot say, and it must not say "nothing happened".
		return PushResult{}, errors.New("clone executor returned an unreadable push result")
	}
	if !result.State.known() {
		return PushResult{}, errors.New("clone executor returned a push state this Cloud does not know")
	}
	// The observed commit travels with the result for the audit, so its shape is checked here rather
	// than trusted: it is written to a row a person will read, and a value that is not an object name is
	// not evidence of anything. An absent one is permitted — it means the executor made no observation.
	if observed := strings.TrimSpace(result.RemoteCommit); observed != "" {
		if len(observed) != 40 || strings.Trim(observed, "0123456789abcdef") != "" {
			return PushResult{}, errors.New("clone executor returned an unreadable push observation")
		}
		result.RemoteCommit = observed
	}
	return result, nil
}

// PrepareMirror asks the executor to prepare one internal mirror and returns what it published.
//
// It is a second method rather than a mode on Dispatch because the two jobs are different subjects on
// different routes, and the reason is the same one the executor's two handlers have: a clone names an
// execution and a Node, a preparation names an Operation and an attempt, and a client that took both
// would be a client whose caller decides which identity fields matter. Cloud sends one or the other,
// and which one it is sending is a fact about the call site rather than about a field.
//
// There is no retry here either, and for the same reason: the authorization is spent when the executor
// redeems it, so an ambiguous result is reconciled by the caller against the operation's state, not
// hidden behind a second request.
//
//nolint:gocritic // By value on purpose: the job carries the bearer authorization, and the copy this method encodes is the copy it sends.
func (c *Client) PrepareMirror(ctx context.Context, job MirrorJob) (MirrorResult, error) {
	encoded, err := json.Marshal(job)
	if err != nil {
		return MirrorResult{}, fmt.Errorf("encode mirror job: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/mirror-preparations", bytes.NewReader(encoded))
	if err != nil {
		return MirrorResult{}, fmt.Errorf("build mirror dispatch request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return MirrorResult{}, fmt.Errorf("clone executor is unreachable: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxJobBody))
	if err != nil {
		return MirrorResult{}, fmt.Errorf("read mirror dispatch response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code == "" {
			refusal.Code = ReasonCodeUnavailable
		}
		return MirrorResult{}, &DispatcherRefusal{Code: refusal.Code, Status: response.StatusCode}
	}
	var result MirrorResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return MirrorResult{}, errors.New("clone executor returned an unreadable response")
	}
	// A preparation that reported no commit published nothing this caller can act on, and the commit is
	// the value Cloud compares against its own rows before it believes the mirror exists.
	if strings.TrimSpace(result.Commit) == "" || strings.TrimSpace(result.Repository) == "" {
		return MirrorResult{}, errors.New("clone executor returned no mirror")
	}
	return result, nil
}
