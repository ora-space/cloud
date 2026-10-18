package workflowasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// FetchTimeout bounds one package from connect to the last byte. The transport separately bounds
// connect and response-header waits; this deadline is the approved 60-second whole-download limit.
const FetchTimeout = 60 * time.Second

// Fetcher reads one pinned plugin release. HTTPS locators use an injected client whose transport
// enforces the deployment's public-address policy; S3 object keys use Cloud's private object store
// endpoint and never leave the cluster. Redirects are always refused: following one would bypass
// the address check already performed for the original host.
type Fetcher struct {
	objects *objectstore.Config
	client  *http.Client
}

// NewFetcher returns a fetcher over the given object store configuration and HTTPS transport.
// A nil transport fails closed: http.Client would otherwise fall back to the environment's
// proxy-aware default and bypass the checked public-address policy.
func NewFetcher(objects *objectstore.Config, transport http.RoundTripper) *Fetcher {
	if transport == nil {
		transport = refusingTransport{}
	}
	return &Fetcher{objects: objects, client: &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// refusingTransport is the fail-closed transport of an unwired fetcher. Deployment wiring always
// injects the checked transport; this only guarantees a misconfiguration cannot reach the network.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("workflow asset fetch has no outbound transport")
}

// Fetch downloads and verifies one package. The locator is either a credential-free HTTPS URL or an
// S3 object key, exactly the two forms the catalog's validateLocator accepts.
func (f *Fetcher) Fetch(ctx context.Context, locator, digest string) ([]byte, error) {
	digest = strings.ToLower(digest)
	sum, err := hex.DecodeString(digest)
	if err != nil || len(sum) != sha256.Size || hex.EncodeToString(sum) != digest {
		return nil, failure(FailureInvalid, "invalid sha256")
	}
	if !strings.Contains(locator, "://") {
		data, e := objectstore.ReadHashed(ctx, f.objects, locator, digest, MaxPackageBytes)
		return data, mapObjectError(e)
	}
	parsed, e := url.Parse(locator)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, failure(FailureInvalid, "invalid https locator")
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), http.NoBody)
	if e != nil {
		return nil, failure(FailureInvalid, "invalid https locator")
	}
	// Verification hashes the stored representation; a decompressed body would not match the
	// digest the catalog recorded.
	req.Header.Set("Accept-Encoding", "identity")
	resp, e := f.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, failure(FailureUnavailable, "download timed out")
		}
		return nil, failure(FailureUnavailable, "download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, failure(FailureUnavailable, "unexpected status")
	}
	if resp.ContentLength > MaxPackageBytes {
		return nil, failure(FailureTooLarge, "declared package size exceeds the bound")
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, MaxPackageBytes+1))
	if e != nil {
		if ctx.Err() != nil {
			return nil, failure(FailureUnavailable, "download timed out")
		}
		return nil, failure(FailureUnavailable, "download failed")
	}
	if int64(len(data)) > MaxPackageBytes {
		return nil, failure(FailureTooLarge, "streamed package size exceeds the bound")
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != digest {
		return nil, failure(FailureDigestMismatch, "package sha256 does not match the catalog")
	}
	return data, nil
}

// mapObjectError translates object-store classifications into the package-level codes; the
// object-store errors deliberately carry no URL or credential detail.
func mapObjectError(err error) error {
	if err == nil {
		return nil
	}
	switch err {
	case objectstore.ErrReadTooLarge:
		return failure(FailureTooLarge, "package exceeds the bound")
	case objectstore.ErrReadInvalid:
		return failure(FailureInvalid, "package failed object verification")
	}
	return failure(FailureUnavailable, "package is not available")
}
