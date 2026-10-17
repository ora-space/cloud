package core

import (
	"context"
	"strings"
)

// ModelEgress checks deployment-controlled model admission, including network I/O.
// It is installed once by composition and never called while holding a transaction.
type ModelEgress interface {
	Enterprise() bool
	CheckModelURL(context.Context, string) error
}

// admitModelWrite separates authority/replay checks from DNS. The final write transaction
// repeats identity, ownership, version and replay checks after network I/O completes.
func (s *Store) admitModelWrite(ctx context.Context, r *PublicRequest) error {
	if s.ModelEgress == nil || !s.ModelEgress.Enterprise() ||
		!strings.HasPrefix(r.Path, "/api/v1/me/model-connections") || strings.HasSuffix(r.Path, "/credential") ||
		(r.Method != "POST" && r.Method != "PUT") {
		return nil
	}
	var base string
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		if r.Method == "POST" {
			require(r.Key != "", 400, "idempotency_key_required")
			if personalModelReplay(t, uid, r.Key, requestHash(r.Method, r.Path, r.Body)) != nil {
				return nil
			}
		} else {
			c := modelConnection(t, uid, r.ModelConnectionID)
			version(c, r.Body.N("version"))
			if pureModelDisable(c, r.Body) {
				return nil
			}
		}
		base = validateModelConnectionForPolicy(r.Body, true).S("baseUrl")
		return nil
	})
	if err != nil || base == "" {
		return err
	}
	if err := s.ModelEgress.CheckModelURL(ctx, base); err != nil {
		return &Fault{Status: 400, Code: "invalid_model_url", Params: Object{}}
	}
	return nil
}

func pureModelDisable(current, body Object) bool {
	enabled, ok := body["enabled"].(bool)
	if !ok || enabled {
		return false
	}
	for _, key := range []string{"name", "protocol", "baseUrl", "authMode", "models"} {
		if jsonText(current[key]) != jsonText(body[key]) {
			return false
		}
	}
	return true
}

func modelWriteData(t *transaction, body Object) Object {
	return validateModelConnectionForPolicy(body, t.store.ModelEgress != nil && t.store.ModelEgress.Enterprise())
}
