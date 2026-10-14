package modelgateway

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// RuntimeScope accepts only a dedicated model-access identity from a verified TLS client chain.
// Runtime identifiers in request bodies or headers cannot substitute for this certificate.
func RuntimeScope(r *http.Request) (core.ModelRuntimeScope, error) {
	bad := func() (core.ModelRuntimeScope, error) {
		return core.ModelRuntimeScope{}, &core.Fault{Status: 401, Code: "model_certificate_required"}
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return bad()
	}
	cert := r.TLS.PeerCertificates[0]
	if len(cert.URIs) != 1 {
		return bad()
	}
	u := cert.URIs[0]
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "spiffe" || u.Host != "ora.local" || u.RawQuery != "" || u.Fragment != "" || len(p) != 7 || p[0] != "model-access" || p[1] != "tenant" || p[3] != "runtime" || p[5] != "generation" || uuid.Validate(p[2]) != nil || uuid.Validate(p[4]) != nil {
		return bad()
	}
	g, err := strconv.ParseInt(p[6], 10, 64)
	if err != nil || g <= 0 {
		return bad()
	}
	return core.ModelRuntimeScope{TenantID: p[2], WorkspaceID: p[4], Generation: g}, nil
}

func (s *Service) grantResponse(g *core.ModelGrant, token string) map[string]any {
	path := "/runtime/openai/v1"
	if g.Protocol == "anthropic-messages" {
		path = "/runtime/anthropic/v1"
	}
	return map[string]any{"grantId": g.ID, "token": token, "expiresAt": g.ExpiresAt, "protocol": g.Protocol, "proxyBaseUrl": s.options.PublicOrigin + path, "model": g.Model}
}

// GrantHandler mints and renews digest-only leases after certificate and durable-scope validation.
func (s *Service) GrantHandler(w http.ResponseWriter, r *http.Request) {
	scope, err := RuntimeScope(r)
	if err != nil {
		fail(w, err)
		return
	}
	const root = "/internal/v1/model-grants"
	if r.URL.Path == root && r.Method == http.MethodPost {
		var body struct {
			BindingID   string `json:"bindingId"`
			ExecutionID string `json:"executionId"`
		}
		if !decode(w, r, &body) {
			return
		}
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			reject(w, 503, "model_service_unavailable")
			return
		}
		token := base64.RawURLEncoding.EncodeToString(secret)
		clear(secret)
		var grant core.ModelGrant
		grant, err = s.options.Store.CreateModelGrant(r.Context(), scope, body.BindingID, body.ExecutionID, tokenDigest(token))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, s.grantResponse(&grant, token))
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, root+"/"), "/")
	if !strings.HasPrefix(r.URL.Path, root+"/") || len(p) < 1 || uuid.Validate(p[0]) != nil {
		reject(w, 404, "not_found")
		return
	}
	var body struct {
		ExecutionID string `json:"executionId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(p) == 2 && p[1] == "renew" && r.Method == http.MethodPost {
		grant, renewErr := s.options.Store.RenewModelGrant(r.Context(), scope, p[0], body.ExecutionID)
		if renewErr != nil {
			fail(w, renewErr)
			return
		}
		writeJSON(w, 200, map[string]any{"expiresAt": grant.ExpiresAt})
		return
	}
	if len(p) == 1 && r.Method == http.MethodDelete {
		if err = s.options.Store.RevokeModelGrant(r.Context(), scope, p[0], body.ExecutionID); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reject(w, 404, "not_found")
}
