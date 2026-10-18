package workflowasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

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
