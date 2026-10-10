package credentialstore

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"
)

// maxRequestBody bounds a credential request. The only variable-length field is a token, already
// bounded by maxSecretBytes, so anything larger is a malformed caller rather than a large secret.
const maxRequestBody = 16 << 10

// Server exposes the Store over mutually authenticated HTTP.
//
// The transport is deliberately plain HTTP underneath TLS rather than a new generated gRPC service:
// the platform's service-identity model is "TLS with a pinned client CA and a SPIFFE URI in the
// client certificate", which net/http expresses completely, and D1 makes the backend replaceable
// behind these service endpoints — so the endpoint shape belongs to this Store and must not leak
// into Cloud's or the Node's generated protocol.
type Server struct {
	store      *Store
	identities *Identities
	log        *zap.Logger
}

// ServerConfig wires the Store and the deployment's identity set.
type ServerConfig struct {
	// TLS must require and verify a client certificate. A configuration that would also serve
	// anonymous callers is refused rather than downgraded (D3, D6).
	TLS *tls.Config
	// Identities names the three fixed service identities. All three are required.
	Identities IdentityConfig
	// Log receives the audit line for each operation. D7 fixes what may appear there: the operation,
	// the credential id, the caller identity, the result, and a failure reason code — never the
	// token, the ciphertext, a wrapped data key, a key-encryption key, or a broker authorization.
	Log *zap.Logger
}

// NewServer validates the transport and identity configuration and returns a ready server.
func NewServer(store *Store, cfg ServerConfig) (*Server, error) {
	if store == nil {
		return nil, errors.New("credential store is required")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.ClientCAs == nil || cfg.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		return nil, errors.New("credential store requires mutually authenticated TLS with a client CA")
	}
	identities, err := NewIdentities(cfg.Identities)
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{store: store, identities: identities, log: log}, nil
}

// Handler returns the Store's HTTP surface. It is exported so the command can mount it on its own
// listener and so tests can drive it through a real TLS connection.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/credentials", s.route(CapabilityWrite, s.write))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/rotate", s.route(CapabilityWrite, s.rotate))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/revoke", s.route(CapabilityWrite, s.revoke))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/resolve", s.route(CapabilityResolve, s.resolve))
	return mux
}

// operation is one capability-gated handler. It receives the credential id from the path.
type operation func(w http.ResponseWriter, r *http.Request, credentialID string)

// route builds the authentication and capability check every operation shares.
//
// Both factors come from outside the request: the service identity from the verified client
// certificate, and the capability from the deployment's fixed table. Nothing the caller can put in
// the URL, headers, or body influences either.
func (s *Server) route(required Capability, next operation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity := ""
		if r.TLS != nil {
			presented, err := s.identities.PresentedIdentity(r.TLS)
			if err != nil {
				s.fail(w, r, "", http.StatusUnauthorized, "service_identity_required", err)
				return
			}
			identity = presented
		} else {
			s.fail(w, r, "", http.StatusUnauthorized, "service_identity_required", errors.New("no client certificate"))
			return
		}
		held, known := s.identities.Capability(identity)
		if !known {
			s.fail(w, r, identity, http.StatusUnauthorized, "service_identity_required", errors.New("identity holds no capability"))
			return
		}
		if held != required {
			s.fail(w, r, identity, http.StatusForbidden, "capability_denied", fmt.Errorf("identity holds %s, not %s", held, required))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next(w, r, r.PathValue("credentialID"))
	}
}

// writeRequest is the ingress-writer's create payload. `token` is the only secret the Store ever
// accepts, and it arrives in the body of this one endpoint (cloud/AGENTS.md conditions 2 and 3).
type writeRequest struct {
	// CredentialID is Cloud's `credential_refs.id`. D4 names it as the cross-store idempotency key,
	// which is why Cloud generates it before calling here.
	CredentialID string `json:"credentialId"`
	Token        string `json:"token"`
}

// credentialRequest is the rotate payload. Revoke carries no body at all.
type credentialRequest struct {
	Token string `json:"token"`
}

// write stores a new token and returns the reference the Store generated for it.
func (s *Server) write(w http.ResponseWriter, r *http.Request, _ string) {
	var body writeRequest
	if !decode(w, r, &body) {
		return
	}
	secretRef, version, err := s.store.Write(r.Context(), body.CredentialID, body.Token)
	body.Token = ""
	if err != nil {
		s.failure(w, r, "write", body.CredentialID, err)
		return
	}
	s.audit(r, "write", body.CredentialID, "succeeded")
	respond(w, http.StatusOK, map[string]any{"secretRef": secretRef, "version": version})
}

// rotate replaces the material of an existing credential.
func (s *Server) rotate(w http.ResponseWriter, r *http.Request, credentialID string) {
	var body credentialRequest
	if !decode(w, r, &body) {
		return
	}
	version, err := s.store.Rotate(r.Context(), credentialID, body.Token)
	body.Token = ""
	if err != nil {
		s.failure(w, r, "rotate", credentialID, err)
		return
	}
	s.audit(r, "rotate", credentialID, "succeeded")
	respond(w, http.StatusOK, map[string]any{"version": version})
}

// revoke destroys the material of an existing credential.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request, credentialID string) {
	if err := s.store.Revoke(r.Context(), credentialID); err != nil {
		s.failure(w, r, "revoke", credentialID, err)
		return
	}
	s.audit(r, "revoke", credentialID, "succeeded")
	respond(w, http.StatusOK, map[string]bool{"revoked": true})
}

// resolve is refused unconditionally.
//
// Resolution needs two factors, and only one exists today: the `broker-resolver` identity is
// enforced above, but the second factor — a Cloud-issued single-use authorization bound to a
// tenant, owner, project, workspace, clone execution, credential id and version, repository
// identity, Node instance and runtime generation — belongs to the Broker step of the approved
// rollout order, which depends on the Node-side execution identity that is not settled. Until that
// lands, D6 requires an explicit failure rather than a fallback, so this endpoint never delivers a
// secret. It exists, and is authenticated, so the refusal is visible and auditable instead of
// looking like a missing route.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, credentialID string) {
	s.audit(r, "resolve", credentialID, "authorization_verification_unavailable")
	respond(w, http.StatusConflict, map[string]string{
		"code":    "authorization_verification_unavailable",
		"message": "single-use broker authorization verification is not implemented",
	})
}

// decode reads a strict JSON body. Unknown fields, a malformed document, a second JSON value, and an
// over-long body are all refused: cloud/AGENTS.md requires strict decoding at every boundary, and a
// silently ignored field here would mean a caller believing it set something it did not.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		respond(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "request body is not a single valid JSON object"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respond(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "request body carries trailing content"})
		return false
	}
	return true
}

// failure maps a Store error to a stable reason code.
//
// D7 requires the codes to distinguish unauthorized, revoked, version mismatch, and unavailable
// without revealing anything about the secret, so the error text never reaches the caller and the
// message is fixed per outcome rather than derived from the error. A caller that went away is not a
// Store failure and is not audited as one.
func (s *Server) failure(w http.ResponseWriter, r *http.Request, operation, credentialID string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	status, code := http.StatusInternalServerError, "store_unavailable"
	switch {
	case errors.Is(err, ErrInvalidRequest):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, ErrRevoked):
		status, code = http.StatusConflict, "credential_revoked"
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, "credential_conflict"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "credential_not_found"
	}
	s.audit(r, operation, credentialID, code)
	// The detail stays in the Store's own log; the caller receives the reason code only.
	s.log.Error("credential store operation failed",
		zap.String("operation", operation),
		zap.String("credentialId", credentialID),
		zap.String("reason", code),
		zap.Error(err),
	)
	respond(w, status, map[string]string{"code": code, "message": "credential store refused the request"})
}

// audit records one operation for D7. The credential id and the caller's service identity are the
// only identifiers that appear; no secret, ciphertext, wrapped data key, or authorization does.
func (s *Server) audit(r *http.Request, operation, credentialID, result string) {
	identity := ""
	if r.TLS != nil {
		if presented, err := s.identities.PresentedIdentity(r.TLS); err == nil {
			identity = presented
		}
	}
	s.log.Info("credential store operation",
		zap.String("operation", operation),
		zap.String("credentialId", credentialID),
		zap.String("serviceIdentity", identity),
		zap.String("result", result),
	)
}

// fail reports a request refused before any operation ran.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, identity string, status int, code string, cause error) {
	s.log.Warn("credential store request refused",
		zap.String("serviceIdentity", identity),
		zap.String("reason", code),
		zap.String("path", r.URL.Path),
		zap.Error(cause),
	)
	respond(w, status, map[string]string{"code": code, "message": "credential store refused the request"})
}

// respond writes a JSON response. Responses carry no secret material beyond the opaque reference,
// and `no-store` keeps an intermediary from retaining even that.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The body is small and already in memory; a write failure here means the client is gone and
	// there is nothing left to report to it.
	_ = json.NewEncoder(w).Encode(body)
}
