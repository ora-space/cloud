package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func readDigest(data string) string {
	digest := sha256.Sum256([]byte(data))
	return hex.EncodeToString(digest[:])
}

func readConfig(endpoint string) *Config {
	return &Config{Endpoint: endpoint, PublicEndpoint: "http://public.invalid", Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret"}
}

func TestReadVerifiedPrivateEndpointAndByteVerification(t *testing.T) {
	data := "{\"seq\":1}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/revisions/tenant/run/session.jsonl" || r.URL.Query().Get("X-Amz-Signature") == "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("read did not bind the private key, GET signature and stored representation")
		}
		_, _ = w.Write([]byte(data))
	}))
	defer server.Close()
	got, err := ReadVerified(t.Context(), readConfig(server.URL), "tenant/run/session.jsonl", int64(len(data)), readDigest(data), int64(len(data)))
	if err != nil || string(got) != data {
		t.Fatalf("verified read = %q, %v", got, err)
	}
}

func TestReadVerifiedRejectsUnavailableInvalidAndOversizedObjects(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		size       int64
		digest     string
		limit      int64
		chunked    bool
		want       error
		wantCalled bool
	}{
		{name: "wrong checksum", status: 200, body: "changed", size: 7, digest: readDigest("original"), limit: 20, want: ErrReadInvalid, wantCalled: true},
		{name: "wrong actual size", status: 200, body: "data", size: 3, digest: readDigest("data"), limit: 20, want: ErrReadInvalid, wantCalled: true},
		{name: "missing", status: 404, size: 4, digest: readDigest("data"), limit: 20, want: ErrReadUnavailable, wantCalled: true},
		{name: "unavailable", status: 503, size: 4, digest: readDigest("data"), limit: 20, want: ErrReadUnavailable, wantCalled: true},
		{name: "declared oversized", size: 21, digest: readDigest("data"), limit: 20, want: ErrReadTooLarge},
		{name: "content length oversized", status: 200, body: strings.Repeat("x", 21), size: 4, digest: readDigest("data"), limit: 20, want: ErrReadTooLarge, wantCalled: true},
		{name: "streamed oversized", status: 200, body: strings.Repeat("x", 21), size: 4, digest: readDigest("data"), limit: 20, chunked: true, want: ErrReadTooLarge, wantCalled: true},
		{name: "invalid checksum", size: 4, digest: "not-a-sha256", limit: 20, want: ErrReadInvalid},
		{name: "negative size", size: -1, digest: readDigest("data"), limit: 20, want: ErrReadInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			var called atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called.Store(true)
				if !test.chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)))
				}
				w.WriteHeader(test.status)
				if test.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			got, err := ReadVerified(t.Context(), readConfig(server.URL), "session.jsonl", test.size, test.digest, test.limit)
			if !errors.Is(err, test.want) || got != nil || called.Load() != test.wantCalled {
				t.Fatalf("got bytes=%d error=%v called=%v; want error=%v called=%v", len(got), err, called.Load(), test.want, test.wantCalled)
			}
		})
	}
}

func TestReadVerifiedDoesNotFollowRedirectOrLeakTransportDetails(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { redirected.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/must-not-receive-signed-request", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	_, err := ReadVerified(t.Context(), readConfig(server.URL), "session.jsonl", 1, readDigest("x"), 10)
	if !errors.Is(err, ErrReadUnavailable) || redirected.Load() {
		t.Fatalf("redirect was followed or not classified safely: %v", err)
	}
	server.Close()
	_, err = ReadVerified(t.Context(), readConfig(server.URL), "session.jsonl", 1, readDigest("x"), 10)
	if !errors.Is(err, ErrReadUnavailable) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "X-Amz") {
		t.Fatalf("network failure exposed transport details: %v", err)
	}
}

func TestReadVerifiedCancellationStopsBodyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	got, err := ReadVerified(ctx, readConfig(server.URL), "session.jsonl", 1, readDigest("x"), 10)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = bytes %d, %v", len(got), err)
	}
}
