// Package gitrefs is Cloud's client of the Git Server's read-only ref query (Git Server contract
// D6). Cloud never writes the Git Server: it reads refs outside transactions and records what it
// read inside them, so this client only answers "which commit does this ref name" and "which refs
// lie under this prefix", authenticated with Cloud's pinned client certificate.
package gitrefs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

// maxResponse bounds a ref query answer. A prefix listing of one namespace instance is small in
// phase one; a larger answer is treated as a failure rather than buffered without limit.
const maxResponse = 4 << 20

// Client queries refs of Project repositories. It implements core.GitRefReader.
type Client struct {
	base string
	http *http.Client
}

// Config names the ref listener and Cloud's client identity for it.
type Config struct {
	// BaseURL is the ref listener, e.g. https://git-server:8446.
	BaseURL string
	// CAFile verifies the Git Server's certificate.
	CAFile string
	// CertFile and KeyFile are Cloud's ref-query client certificate, the one leaf the Git Server pins.
	CertFile, KeyFile string
	// Timeout bounds one query.
	Timeout time.Duration
}

// New builds a client with TLS 1.3 mutual authentication.
func New(c Config) (*Client, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.Path != "" && base.Path != "/" {
		return nil, errors.New("git ref query base URL must be an https origin")
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read git ref query CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid git ref query CA")
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load git ref query client identity: %w", err)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{pair}}, ForceAttemptHTTP2: true}
	return NewWithHTTP(strings.TrimRight(c.BaseURL, "/"), &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
}

// NewWithHTTP builds a client over a caller-provided HTTP client, for tests and custom transports.
func NewWithHTTP(base string, client *http.Client) (*Client, error) {
	if base == "" || client == nil {
		return nil, errors.New("git ref query base URL and HTTP client are required")
	}
	return &Client{base: strings.TrimRight(base, "/"), http: client}, nil
}

// ResolveGitRef returns the commit ref names, or found=false when the ref (or the repository)
// does not exist.
func (c *Client) ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (string, bool, error) {
	var out struct {
		Commit string `json:"commit"`
	}
	status, err := c.get(ctx, url.Values{"tenantId": {tenantID}, "projectId": {projectID}, "ref": {ref}}, &out)
	switch {
	case err != nil:
		return "", false, err
	case status == http.StatusNotFound:
		return "", false, nil
	case !core.GitObjectID(out.Commit):
		return "", false, errors.New("git ref query returned an invalid commit")
	}
	return out.Commit, true, nil
}

// ListGitRefs lists the refs under one namespace prefix.
func (c *Client) ListGitRefs(ctx context.Context, tenantID, projectID, prefix string) ([]core.GitRefCommit, error) {
	var out struct {
		Refs []struct {
			Ref    string `json:"ref"`
			Commit string `json:"commit"`
		} `json:"refs"`
	}
	status, err := c.get(ctx, url.Values{"tenantId": {tenantID}, "projectId": {projectID}, "prefix": {prefix}}, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("git ref query answered %d", status)
	}
	refs := make([]core.GitRefCommit, 0, len(out.Refs))
	for _, r := range out.Refs {
		if !strings.HasPrefix(r.Ref, prefix) || !core.GitObjectID(r.Commit) {
			return nil, errors.New("git ref query returned a ref outside the prefix")
		}
		refs = append(refs, core.GitRefCommit{Ref: r.Ref, Commit: r.Commit})
	}
	return refs, nil
}

// get performs one query. 200 decodes into out; 404 ref_not_found is reported as a status; every
// other answer is an error, so a caller never mistakes an unavailable server for a missing ref.
func (c *Client) get(ctx context.Context, query url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/internal/v1/refs?"+query.Encode(), http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("build git ref query: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("git ref query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return 0, fmt.Errorf("read git ref query: %w", err)
	}
	if len(body) > maxResponse {
		return 0, errors.New("git ref query answer too large")
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(body, out); err != nil {
			return 0, fmt.Errorf("decode git ref query: %w", err)
		}
		return http.StatusOK, nil
	case http.StatusNotFound:
		var fault struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &fault) == nil && fault.Code == "ref_not_found" {
			return http.StatusNotFound, nil
		}
	}
	return 0, fmt.Errorf("git ref query answered %d", resp.StatusCode)
}
