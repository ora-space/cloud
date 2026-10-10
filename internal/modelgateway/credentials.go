package modelgateway

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// IsCredentialPath is the fixed Gateway routing allowlist; callers cannot select an upstream.
func IsCredentialPath(path string) bool {
	// Reserve malformed variants too: they must never send a plaintext credential to Cloud.
	return strings.HasPrefix(path, "/api/v1/me/model-connections/") && strings.HasSuffix(strings.TrimRight(path, "/"), "/credential")
}

func (s *Service) userIdentity(r *http.Request) (*core.Claims, error) {
	service, err := s.options.Auth.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "service")
	if err != nil || service.Role != "gateway" {
		return nil, &core.Fault{Status: 401, Code: "invalid_service_credential"}
	}
	user, err := s.options.Auth.Verify(r.Header.Get("X-Ora-User-Token"), "user")
	if err != nil || user.Caller != service.Subject {
		return nil, &core.Fault{Status: 401, Code: "invalid_user_credential"}
	}
	return user, nil
}

// CredentialHandler accepts write-only keys exclusively behind the authenticated browser Gateway.
// Original keys are never forwarded to Cloud or included in a replay, error or response.
func (s *Service) CredentialHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.HealthHandler(w, r)
		return
	}
	if !IsCredentialPath(r.URL.Path) {
		reject(w, 404, "not_found")
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 7 || uuid.Validate(parts[5]) != nil {
		reject(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		reject(w, 405, "method_not_allowed")
		return
	}
	identity, err := s.userIdentity(r)
	if err != nil {
		fail(w, err)
		return
	}
	connectionID := parts[5]
	if _, err = s.options.Store.ModelCredential(r.Context(), identity, connectionID); err != nil {
		fail(w, err)
		return
	}
	operation := r.Header.Get("Idempotency-Key")
	if operation == "" || len(operation) > 200 {
		reject(w, 400, "idempotency_key_required")
		return
	}
	var resource core.Object
	if r.Method == http.MethodDelete {
		var body struct {
			Version *int64 `json:"version"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Version == nil {
			reject(w, 428, "version_required")
			return
		}
		resource, err = s.options.Store.ClearModelCredential(r.Context(), identity, connectionID, *body.Version, operation)
	} else {
		var body struct {
			Version *int64 `json:"version"`
			APIKey  string `json:"apiKey"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Version == nil {
			reject(w, 428, "version_required")
			return
		}
		if strings.TrimSpace(body.APIKey) == "" || len(body.APIKey) > 8192 || strings.ContainsAny(body.APIKey, "\r\n\x00") {
			reject(w, 400, "invalid_model_credential")
			return
		}
		id := s.options.Cipher.CredentialID(identity, connectionID, *body.Version, operation, body.APIKey)
		plaintext := []byte(body.APIKey)
		body.APIKey = ""
		var sealed []byte
		sealed, err = s.options.Cipher.Seal(id, plaintext)
		clear(plaintext)
		if err == nil {
			resource, err = s.options.Store.PutModelCredential(r.Context(), identity, connectionID, *body.Version, operation, id, sealed, s.options.Cipher.keyID)
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"resource": resource})
}
