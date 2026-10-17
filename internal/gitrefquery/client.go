// Package gitrefquery is Cloud's client for the Git Server's read-only ref query. Cloud uses it to
// verify, outside any database transaction, that a ref it is about to register exists and names the
// commit it was told about. The query listener accepts only Cloud's pinned client certificate.
package gitrefquery

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

	"github.com/wanglongan587/cloud/internal/core"
)

// maxResponse bounds what one answer may be; a commit answer is well under 1 KiB.
const maxResponse = 64 << 10

// Client queries one Git Server ref listener.
type Client struct {
	base    *url.URL
	http    *http.Client
	timeout time.Duration
}

// New returns a client for the ref listener at base (https, no path, query or fragment). The HTTP
// client carries the TLS configuration that pins the Git Server and presents Cloud's identity;
// timeout bounds every query.
func New(base string, client *http.Client, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("git ref query URL must be https://host[:port]")
	}
	if client == nil || timeout <= 0 {
		return nil, errors.New("git ref query needs an HTTP client and a positive timeout")
	}
	u.Path = ""
	return &Client{base: u, http: client, timeout: timeout}, nil
}

// ResolveGitRef returns the commit ref names in the Project repository. A 404 ref_not_found is the
// definitive core.ErrGitRefNotFound; every other answer, a transport failure or a timeout is an
// error that proves nothing, so callers retry rather than conclude.
func (c *Client) ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := *c.base
	target.Path = "/internal/v1/refs"
	target.RawQuery = url.Values{"tenantId": {tenantID}, "projectId": {projectID}, "ref": {ref}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build git ref query: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("git ref query: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var body struct {
		Commit string `json:"commit"`
		Code   string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponse)).Decode(&body); err != nil {
		return "", fmt.Errorf("git ref query answered %d without a readable body", response.StatusCode)
	}
	switch {
	case response.StatusCode == http.StatusOK && core.GitObjectID(strings.TrimSpace(body.Commit)):
		return body.Commit, nil
	case response.StatusCode == http.StatusNotFound && body.Code == "ref_not_found":
		return "", core.ErrGitRefNotFound
	default:
		return "", fmt.Errorf("git ref query answered %d %q", response.StatusCode, body.Code)
	}
}
