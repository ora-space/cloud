package repoexec

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// maxResultBody bounds a task response; a probe listing many refs is the largest.
const maxResultBody = 1 << 20

// clientGrace is how much longer than the task's own budget the client waits: room for the
// executor's cleanup and the response to arrive. The client never gives up before the task could.
const clientGrace = 2 * time.Minute

// NotDeliveredError: the request never reached the executor (dial, TLS or write failure before the
// request was complete). The task certainly did not start; the authorization is unspent.
type NotDeliveredError struct{ Err error }

func (e *NotDeliveredError) Error() string {
	return "repo executor task not delivered: " + e.Err.Error()
}

// Unwrap returns the transport error.
func (e *NotDeliveredError) Unwrap() error { return e.Err }

// LostError: the request was sent but no readable response arrived. The task may have run, fully or
// partly; Cloud must reconcile (ref query for sync, probe for publish) and never resend blindly.
type LostError struct{ Err error }

func (e *LostError) Error() string { return "repo executor task result lost: " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *LostError) Unwrap() error { return e.Err }

// RefusedError: the executor answered without running the task (429 executor_busy, 403 identity,
// 400 malformed). The task did not start.
type RefusedError struct {
	Status int
	Code   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("repo executor refused the task: %s (status %d)", e.Code, e.Status)
}

// ClientConfig wires Cloud's dispatcher.
type ClientConfig struct {
	// Endpoint is the executor's https base URL.
	Endpoint string
	// TLS presents the dispatcher certificate (spiffe://ora.local/cloud/repo-dispatcher) and trusts
	// the executor's CA.
	TLS *tls.Config
}

// Client is Cloud's side of the task interface.
type Client struct {
	endpoint string
	http     *http.Client
}

// NewClient validates the configuration and returns a ready client.
func NewClient(cfg *ClientConfig) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("repo executor endpoint must be an https URL")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.RootCAs == nil {
		return nil, errors.New("repo executor client needs the dispatcher certificate and the executor CA")
	}
	tlsConfig := cfg.TLS.Clone()
	tlsConfig.MinVersion = tls.VersionTLS13
	return &Client{
		endpoint: strings.TrimSuffix(u.String(), "/"),
		// No client timeout: a sync may legitimately run for its whole budget. Run bounds each call by
		// the task's own timeout plus clientGrace instead.
		http: &http.Client{
			Transport:     &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, TLSHandshakeTimeout: 10 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Run sends one task and waits for its result. It never retries: a lost result must be reconciled,
// not resent.
func (c *Client) Run(ctx context.Context, task *Task) (Result, error) {
	body, err := json.Marshal(task)
	if err != nil {
		return Result{}, &NotDeliveredError{Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, task.Timeout()+clientGrace)
	defer cancel()
	var written atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err == nil {
			written.Store(true)
		}
	}}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, c.endpoint+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return Result{}, &NotDeliveredError{Err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if !written.Load() {
			return Result{}, &NotDeliveredError{Err: err}
		}
		return Result{}, &LostError{Err: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResultBody))
	if err != nil {
		return Result{}, &LostError{Err: err}
	}
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &refusal) != nil || refusal.Code == "" {
			refusal.Code = "unknown"
		}
		if response.StatusCode >= http.StatusInternalServerError {
			// A 5xx from a proxy or a crashed handler says nothing about whether the task ran.
			return Result{}, &LostError{Err: &RefusedError{Status: response.StatusCode, Code: refusal.Code}}
		}
		return Result{}, &RefusedError{Status: response.StatusCode, Code: refusal.Code}
	}
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil || result.Classification == "" {
		return Result{}, &LostError{Err: errors.New("unreadable task result")}
	}
	return result, nil
}

// ClientTLS loads the dispatcher certificate and the executor CA.
func ClientTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("dispatcher TLS identity unavailable")
	}
	pool, err := loadPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool}, nil
}
