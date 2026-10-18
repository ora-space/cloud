package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"time"
)

// ReadError is a bounded classification that never carries a signed URL or transport details.
type ReadError string

// Error returns the safe object-read classification.
func (e ReadError) Error() string { return string(e) }

const (
	// ErrReadUnavailable means the private store could not supply the registered object.
	ErrReadUnavailable ReadError = "object read unavailable"
	// ErrReadInvalid means the bytes do not match the registered size or SHA-256.
	ErrReadInvalid ReadError = "object read invalid"
	// ErrReadTooLarge means the declared or streamed bytes exceed the caller's bound.
	ErrReadTooLarge ReadError = "object read too large"
)

// ReadVerified fetches one server-selected object through the private S3 endpoint. It signs GET
// locally, refuses redirects, bounds both declared and streamed size, and hashes the actual bytes.
// The caller must authorize the key before calling and must not hold a database transaction here.
func ReadVerified(ctx context.Context, cfg *Config, key string, size int64, digest string, maxBytes int64) ([]byte, error) {
	if size < 0 {
		return nil, ErrReadInvalid
	}
	return readBounded(ctx, cfg, key, size, digest, maxBytes)
}

// ReadHashed is ReadVerified for callers that know the expected digest but not the object size,
// such as a marketplace locator that records only a SHA-256. The declared size is still bounded by
// the streamed read cap, and the digest is verified over the stored bytes.
func ReadHashed(ctx context.Context, cfg *Config, key, digest string, maxBytes int64) ([]byte, error) {
	return readBounded(ctx, cfg, key, -1, digest, maxBytes)
}

// readBounded implements both read contracts; a negative size means the caller does not know it.
func readBounded(ctx context.Context, cfg *Config, key string, size int64, digest string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, ErrReadInvalid
	}
	if size >= 0 && size > maxBytes {
		return nil, ErrReadTooLarge
	}
	sum, err := hex.DecodeString(digest)
	if err != nil || len(sum) != sha256.Size || hex.EncodeToString(sum) != digest {
		return nil, ErrReadInvalid
	}
	if cfg == nil {
		return nil, ErrReadUnavailable
	}
	private := *cfg
	private.PublicEndpoint = ""
	grant, err := presign(&private, key, http.MethodGet, map[string]string{}, time.Now())
	if err != nil {
		return nil, ErrReadUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, http.NoBody)
	if err != nil {
		return nil, ErrReadUnavailable
	}
	for name, value := range grant.Headers {
		req.Header.Set(name, value)
	}
	// Hash the stored representation, never an automatically decompressed response.
	req.Header.Set("Accept-Encoding", "identity")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrReadUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrReadUnavailable
	}
	if resp.ContentLength > maxBytes {
		return nil, ErrReadTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrReadTooLarge
	}
	if err != nil {
		return nil, ErrReadUnavailable
	}
	actual := sha256.Sum256(data)
	if (size >= 0 && int64(len(data)) != size) || hex.EncodeToString(actual[:]) != digest {
		return nil, ErrReadInvalid
	}
	return data, nil
}
