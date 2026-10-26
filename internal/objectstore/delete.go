package objectstore

import (
	"context"
	"net/http"
	"time"
)

// DeleteError is a bounded classification that never carries a signed URL or transport details.
type DeleteError string

// Error returns the safe object-delete classification.
func (e DeleteError) Error() string { return string(e) }

const (
	// ErrDeleteUnavailable means the store did not confirm the deletion; a later pass may retry it.
	ErrDeleteUnavailable DeleteError = "object delete unavailable"
)

// Delete removes one object Cloud owns, through the private S3 endpoint, with the same local
// presigning the uploads use: the signed URL is a server-side capability for exactly this key and
// method, and it is fetched from the private endpoint rather than the browser-facing one so a
// deployment that publishes a CDN can never route a deletion through it.
//
// An object that is already gone is success: the callers are cleanup passes that must converge on
// "not there" rather than distinguish "we deleted it" from "nothing was left to delete". Redirects
// are refused, and the caller must not hold a database transaction here.
func Delete(ctx context.Context, cfg *Config, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return ErrDeleteUnavailable
	}
	private := *cfg
	private.PublicEndpoint = ""
	grant, err := presign(&private, key, http.MethodDelete, map[string]string{}, time.Now())
	if err != nil {
		return ErrDeleteUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, grant.Method, grant.URL, http.NoBody)
	if err != nil {
		return ErrDeleteUnavailable
	}
	for name, value := range grant.Headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrDeleteUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return ErrDeleteUnavailable
	}
}
