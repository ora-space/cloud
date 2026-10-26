package workflowasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

type deadlineTransport struct {
	want time.Duration
	ctx  context.Context
	t    *testing.T
}

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	d.ctx = r.Context()
	deadline, ok := d.ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining < d.want/2 || remaining > d.want {
		d.t.Fatalf("download deadline = %s (present %v), want within %s", remaining, ok, d.want)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("package bytes"))}, nil
}

func TestFetchDeadlineConfiguration(t *testing.T) {
	for _, c := range []struct {
		name          string
		timeout, want time.Duration
	}{
		{"production default", FetchTimeout, 60 * time.Second},
		{"zero retains bound", 0, 60 * time.Second},
		{"negative retains bound", -time.Second, 60 * time.Second},
		{"injected limit", time.Second, time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := &deadlineTransport{want: c.want, t: t}
			fetcher := NewFetcher(nil, transport)
			if fetcher.Timeout != FetchTimeout {
				t.Fatalf("constructor timeout = %s, want %s", fetcher.Timeout, FetchTimeout)
			}
			fetcher.Timeout = c.timeout
			data, err := fetcher.Fetch(t.Context(), "https://example.invalid/p.orax", sha256Hex([]byte("package bytes")))
			if err != nil || string(data) != "package bytes" {
				t.Fatalf("fetch before deadline = %q, %v", data, err)
			}
			if !errors.Is(transport.ctx.Err(), context.Canceled) {
				t.Fatal("successful fetch did not release its deadline context")
			}
		})
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestFetchDownloadsAndVerifiesHTTPS(t *testing.T) {
	pkg := []byte("package bytes")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("fetch must GET the stored representation without content coding")
		}
		_, _ = w.Write(pkg)
	}))
	defer server.Close()
	fetcher := NewFetcher(nil, server.Client().Transport)
	got, err := fetcher.Fetch(t.Context(), server.URL+"/packages/demo.orax", sha256Hex(pkg))
	if err != nil || string(got) != string(pkg) {
		t.Fatalf("Fetch = %q, %v", got, err)
	}
}

func TestFetchNormalizesTheCatalogDigest(t *testing.T) {
	pkg := []byte("package bytes")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pkg)
	}))
	defer server.Close()
	_, err := NewFetcher(nil, server.Client().Transport).Fetch(t.Context(), server.URL+"/p.orax", strings.ToUpper(sha256Hex(pkg)))
	if err != nil {
		t.Fatalf("Fetch rejected an upper-case digest: %v", err)
	}
}

func TestFetchRejectsHTTPSFailures(t *testing.T) {
	pkg := []byte("package bytes")
	for _, test := range []struct {
		name    string
		locator func(server *httptest.Server) string
		digest  string
		handler http.HandlerFunc
		code    string
	}{
		{
			name:    "digest mismatch",
			locator: func(server *httptest.Server) string { return server.URL + "/p.orax" },
			digest:  sha256Hex([]byte("other")),
			code:    FailureDigestMismatch,
		},
		{
			name:    "unexpected status",
			locator: func(server *httptest.Server) string { return server.URL + "/p.orax" },
			digest:  sha256Hex(pkg),
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			code:    FailureUnavailable,
		},
		{
			name:    "declared size exceeds the bound",
			locator: func(server *httptest.Server) string { return server.URL + "/p.orax" },
			digest:  sha256Hex(pkg),
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "16777217")
				w.WriteHeader(http.StatusOK)
			},
			code: FailureTooLarge,
		},
		{
			name: "plain http",
			locator: func(server *httptest.Server) string {
				return "http" + strings.TrimPrefix(server.URL, "https") + "/p.orax"
			},
			digest: sha256Hex(pkg),
			code:   FailureInvalid,
		},
		{
			name:    "fragment",
			locator: func(server *httptest.Server) string { return server.URL + "/p.orax#fragment" },
			digest:  sha256Hex(pkg),
			code:    FailureInvalid,
		},
		{
			name: "user info",
			locator: func(server *httptest.Server) string {
				return "https://user:pass@" + strings.TrimPrefix(server.URL, "https://") + "/p.orax"
			},
			digest: sha256Hex(pkg),
			code:   FailureInvalid,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := test.handler
			if handler == nil {
				handler = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(pkg) }
			}
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			_, err := NewFetcher(nil, server.Client().Transport).Fetch(t.Context(), test.locator(server), test.digest)
			requireFailure(t, err, test.code)
		})
	}
}

func TestFetchRejectsInvalidDigests(t *testing.T) {
	for _, digest := range []string{"", "not-a-digest", strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		_, err := NewFetcher(nil, nil).Fetch(t.Context(), "packages/p.orax", digest)
		requireFailure(t, err, FailureInvalid)
	}
}

func TestFetchRefusesRedirects(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
	}))
	defer target.Close()
	pkg := []byte("package bytes")
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/p.orax", http.StatusFound)
	}))
	defer source.Close()
	_, err := NewFetcher(nil, source.Client().Transport).Fetch(t.Context(), source.URL+"/p.orax", sha256Hex(pkg))
	requireFailure(t, err, FailureUnavailable)
	if reached.Load() != 0 {
		t.Fatal("fetch followed the redirect, bypassing the address check")
	}
}

func TestFetchUsesThePrivateObjectStoreForObjectKeys(t *testing.T) {
	pkg := []byte("package bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/plugins/packages/demo.orax" || r.URL.Query().Get("X-Amz-Signature") == "" {
			t.Error("object fetch must GET the signed private key")
		}
		_, _ = w.Write(pkg)
	}))
	defer server.Close()
	fetcher := NewFetcher(&objectstore.Config{
		Endpoint: server.URL, PublicEndpoint: "http://public.invalid", Region: "us-east-1",
		Bucket: "plugins", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret",
	}, nil)
	got, err := fetcher.Fetch(t.Context(), "packages/demo.orax", sha256Hex(pkg))
	if err != nil || string(got) != string(pkg) {
		t.Fatalf("Fetch = %q, %v", got, err)
	}
}

func TestFetchMapsObjectStoreFailures(t *testing.T) {
	pkg := []byte("package bytes")
	for _, test := range []struct {
		name   string
		status int
		body   []byte
		digest string
		code   string
	}{
		{name: "missing object", status: http.StatusNotFound, body: pkg, digest: sha256Hex(pkg), code: FailureUnavailable},
		{name: "digest mismatch", status: http.StatusOK, body: pkg, digest: sha256Hex([]byte("other")), code: FailureInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			config := &objectstore.Config{
				Endpoint: server.URL, Region: "us-east-1", Bucket: "plugins", PathStyle: true,
				AccessKeyID: "test-access", SecretAccessKey: "test-secret",
			}
			_, err := NewFetcher(config, nil).Fetch(t.Context(), "packages/demo.orax", test.digest)
			requireFailure(t, err, test.code)
		})
	}
}

func TestFetchWithoutAnObjectStoreIsUnavailable(t *testing.T) {
	_, err := NewFetcher(nil, nil).Fetch(t.Context(), "packages/demo.orax", sha256Hex([]byte("package bytes")))
	requireFailure(t, err, FailureUnavailable)
}

func TestFetchWithoutATransportFailsClosed(t *testing.T) {
	// A nil transport must not fall back to http.DefaultTransport, which honors environment
	// proxies and would bypass the checked outbound policy.
	_, err := NewFetcher(nil, nil).Fetch(t.Context(), "https://example.invalid/p.orax", sha256Hex(nil))
	requireFailure(t, err, FailureUnavailable)
}

// checkedDialError stands in for the model gateway's checked-dial failure: it exposes a bounded
// address-and-reason detail and deliberately has no URL of its own.
type checkedDialError string

func (e checkedDialError) Error() string             { return "checked dial failed: " + string(e) }
func (e checkedDialError) DialFailureDetail() string { return string(e) }

type dialFailureTransport struct{ detail string }

func (t dialFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	// http.Client would wrap a transport error in *url.Error, whose text carries the full request
	// URL including the signed query that must never reach the failure detail or a log.
	return nil, &url.Error{Op: "Get", URL: "https://assets.example.invalid/package.orax?X-Amz-Signature=secret", Err: checkedDialError(t.detail)}
}

func TestFetchKeepsCheckedAddressAttributionWithoutTheSignedURL(t *testing.T) {
	transport := dialFailureTransport{detail: "198.51.100.1:443 timeout; 198.51.100.2:443 connection refused"}
	_, err := NewFetcher(nil, transport).Fetch(t.Context(), "https://assets.example.invalid/package.orax?X-Amz-Signature=secret", sha256Hex(nil))
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != FailureUnavailable {
		t.Fatalf("error = %v, want the bounded unavailable failure", err)
	}
	if want := "download failed: " + transport.detail; failure.Detail != want {
		t.Fatalf("detail = %q, want %q", failure.Detail, want)
	}
	for _, private := range []string{"X-Amz-Signature", "https://"} {
		if strings.Contains(failure.Detail, private) {
			t.Fatalf("failure detail leaked the request URL: %q", failure.Detail)
		}
	}
}

func TestFetchHonoursCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewFetcher(nil, server.Client().Transport).Fetch(ctx, server.URL+"/p.orax", sha256Hex(nil))
	var packageFailure *Failure
	if !errors.As(err, &packageFailure) {
		t.Fatalf("error = %v, want a bounded package failure", err)
	}
}
