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
	resolver   *Resolver
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
	// The resolve capability is taken once, here, so the handlers below cannot disagree about whether
	// this deployment can verify authorizations. A deployment with no verification key yields no
	// resolver, and the resolve endpoint then refuses every request with the code it has always used.
	resolver, _ := store.Resolver()
	return &Server{store: store, resolver: resolver, identities: identities, log: log}, nil
}

// Handler returns the Store's HTTP surface. It is exported so the command can mount it on its own
// listener and so tests can drive it through a real TLS connection.
//
// Each purpose has exactly one redemption endpoint. A `mirror_prepare` authorization presented to the
// clone endpoint is refused by the operation check the resolver performs, and a clone authorization
// presented to the mirror endpoint likewise — the separation is enforced on the signed payload rather
// than by the routes being distinct.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/credentials", s.route(CapabilityWrite, s.write))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/rotate", s.route(CapabilityWrite, s.rotate))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/revoke", s.route(CapabilityWrite, s.revoke))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/resolve", s.route(CapabilityResolve, s.resolve))
	mux.HandleFunc("POST /v1/credentials/{credentialID}/resolve-mirror", s.route(CapabilityResolve, s.resolveMirror))
	return mux
}

// operation is one capability-gated handler. It receives the credential id from the path and the
// service identity the route already proved, so a handler that needs to know who is calling never
// reads the request for it: the identity is resolved from the verified client certificate once, at
// the boundary, and handed inward as a trusted value.
type operation func(w http.ResponseWriter, r *http.Request, credentialID, identity string)

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
		next(w, r, r.PathValue("credentialID"), identity)
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
func (s *Server) write(w http.ResponseWriter, r *http.Request, _, _ string) {
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
func (s *Server) rotate(w http.ResponseWriter, r *http.Request, credentialID, _ string) {
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
func (s *Server) revoke(w http.ResponseWriter, r *http.Request, credentialID, _ string) {
	if err := s.store.Revoke(r.Context(), credentialID); err != nil {
		s.failure(w, r, "revoke", credentialID, err)
		return
	}
	s.audit(r, "revoke", credentialID, "succeeded")
	respond(w, http.StatusOK, map[string]bool{"revoked": true})
}

// resolve delivers the token one single-use authorization names, for either operation redeemed here.
//
// Two factors must both hold, and neither is taken from the caller's assertion: the `broker-resolver`
// service identity is enforced by the route above, and the authorization is verified against Cloud's
// public key by the Store itself (D3). The Store additionally checks the authorization's binding, its
// expiry against the Store's own clock, and whether it has already been spent, and it records the
// consumption before the secret leaves the process (D10).
//
// One route serves both a clone's redemption and a push's, which is the push decision's D5: the purpose
// is inside the signed payload, so the signature is what decides which set of checks applies, and a
// second URL would be a second surface to authenticate without adding a distinction the first one cannot
// make. The handler therefore does not branch on the purpose — it could only do so by reading an
// unverified payload — and hands the resolver the identity the route proved. The `identity` is what the
// push branch compares against the authorization's `executorIdentity`; a clone's branch ignores it,
// because a clone's caller is identified by the runtime it declares and by the certificate both Nodes
// share.
//
// When the deployment configured no verification key there is no resolver at all. The refusal is then
// issued before the body is read, because a Store that cannot decide whether an authorization is
// genuine must not act on one, and parsing input it can never accept would only invite a caller to
// keep trying.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, credentialID, identity string) {
	if s.resolver == nil {
		s.audit(r, "resolve", credentialID, "authorization_verification_unavailable")
		respond(w, http.StatusConflict, map[string]string{
			"code":    "authorization_verification_unavailable",
			"message": "this deployment configured no authorization verification key",
		})
		return
	}
	var body ResolveRequest
	if !decode(w, r, &body) {
		return
	}
	resolved, err := s.resolver.Resolve(r.Context(), credentialID, identity, body)
	// The authorization is a bearer secret; it must not survive into the audit line or the failure log.
	body.Authorization = ""
	if err != nil {
		s.failure(w, r, "resolve", credentialID, err)
		return
	}
	s.audit(r, "resolve", credentialID, "succeeded")
	respond(w, http.StatusOK, map[string]any{"token": resolved.Token, "version": resolved.Version})
}

// resolveMirror delivers the token one `mirror_prepare` authorization names.
//
// It reads no identity from the body, because there is none to read: `MirrorResolveRequest` carries
// the authorization and nothing else. The identity the resolver compares against the binding is the
// one the route above proved from the verified client certificate, which is why the handler takes it
// as an argument rather than looking it up again — a second lookup could only disagree with the first
// if the state changed underneath, and there is no reason to leave that possibility open.
//
// This endpoint spends a mirror authorization exactly once, durably, before any secret leaves the
// process, and it cannot decide Cloud's facts (see Resolver.ResolveMirror). Cloud re-checks those
// where the content is used.
func (s *Server) resolveMirror(w http.ResponseWriter, r *http.Request, credentialID, identity string) {
	if s.resolver == nil {
		s.audit(r, "resolve-mirror", credentialID, "authorization_verification_unavailable")
		respond(w, http.StatusConflict, map[string]string{
			"code":    "authorization_verification_unavailable",
			"message": "this deployment configured no authorization verification key",
		})
		return
	}
	var body MirrorResolveRequest
	if !decode(w, r, &body) {
		return
	}
	resolved, err := s.resolver.ResolveMirror(r.Context(), credentialID, identity, body)
	// The authorization is a bearer secret; it must not survive into the audit line or the failure log.
	body.Authorization = ""
	if err != nil {
		s.failure(w, r, "resolve-mirror", credentialID, err)
		return
	}
	s.audit(r, "resolve-mirror", credentialID, "succeeded")
	respond(w, http.StatusOK, map[string]any{"token": resolved.Token, "version": resolved.Version})
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
//
// The resolve outcomes are separate codes rather than one "resolve failed": a caller that was refused
// because its authorization expired, because it already spent it, or because it is presenting the
// wrong runtime has to be able to tell those apart to decide between asking for a new authorization
// and investigating. None of them reveals anything about the secret, which is what D7 protects.
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
	case errors.Is(err, ErrAuthorizationUnverifiable):
		status, code = http.StatusConflict, "authorization_verification_unavailable"
	case errors.Is(err, ErrAuthorizationInvalid):
		status, code = http.StatusForbidden, "authorization_invalid"
	case errors.Is(err, ErrAuthorizationExpired):
		status, code = http.StatusForbidden, "authorization_expired"
	case errors.Is(err, ErrAuthorizationBinding):
		status, code = http.StatusForbidden, "authorization_binding_mismatch"
	case errors.Is(err, ErrAuthorizationPurpose):
		status, code = http.StatusForbidden, "authorization_purpose_mismatch"
	case errors.Is(err, ErrAuthorizationConsumed):
		status, code = http.StatusConflict, "authorization_consumed"
	case errors.Is(err, ErrVersionUnavailable):
		status, code = http.StatusConflict, "credential_version_unavailable"
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
