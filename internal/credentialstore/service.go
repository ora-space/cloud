package credentialstore

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// Listener names used in audit lines.
const (
	listenerRedemption = "redemption"
	listenerManagement = "management"
	listenerKey        = "key"
	listenerIngress    = "ingress"
)

// RedemptionHandler serves the redemption listener: POST /v1/redemptions, only for the redeemer
// identity. Serve it with RequireAndVerifyClientCert against the service client CA.
func (s *Server) RedemptionHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/redemptions", s.requirePeer(listenerRedemption, s.identities.Redeemer, s.redeem))
	mux.HandleFunc("/", notFound)
	return mux
}

// ManagementHandler serves the management listener, only for the manager identity:
// GET /v1/credentials/{credentialId} and POST /v1/credentials/{credentialId}/revoke. Neither route
// accepts or returns plaintext.
func (s *Server) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/credentials/{credentialId}", s.requirePeer(listenerManagement, s.identities.Manager, s.getMetadata))
	mux.HandleFunc("POST /v1/credentials/{credentialId}/revoke", s.requirePeer(listenerManagement, s.identities.Manager, s.revoke))
	mux.HandleFunc("/", notFound)
	return mux
}

// KeyHandler serves the key listener: POST /v1/keys/rewrap, only for the KEK operator identity.
func (s *Server) KeyHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/keys/rewrap", s.requirePeer(listenerKey, s.identities.KeyOperator, s.rewrap))
	mux.HandleFunc("/", notFound)
	return mux
}

// redemptionRequest is the redemption body. The authorization is a bearer secret and is never
// logged.
type redemptionRequest struct {
	Authorization string `json:"authorization"`
}

// redemptionResponse is the only response in the Store that carries plaintext.
type redemptionResponse struct {
	Username string `json:"username"`
	Token    string `json:"token"`
	Version  int64  `json:"version"`
}

// redeem verifies an execution authorization with the Store's own clock, binds it to the peer
// identity, and delivers the secret at most once.
func (s *Server) redeem(w http.ResponseWriter, r *http.Request, peer string) {
	var body redemptionRequest
	if !readStrict(w, r, &body) {
		return
	}
	authorization, err := cloneauth.Verify(body.Authorization, s.executionKeys, s.now())
	body.Authorization = ""
	if err != nil {
		s.log.Info("credential store operation refused",
			zap.String("operation", "redeem"), zap.String("peer", peer),
			zap.String("reason", verifyReason(err)), zap.String("outcome", CodeInvalidAuthorization))
		respondCode(w, http.StatusBadRequest, CodeInvalidAuthorization)
		return
	}
	fields := []zap.Field{
		zap.String("operation", "redeem"), zap.String("peer", peer),
		zap.String("authorizationId", authorization.AuthorizationID),
		zap.String("purpose", string(authorization.Purpose)),
		zap.String("credentialId", authorization.CredentialID),
		zap.Int64("version", authorization.CredentialVersion),
	}
	switch authorization.Purpose {
	case cloneauth.PurposeSync, cloneauth.PurposePublish, cloneauth.PurposeProbe:
	default:
		s.log.Info("credential store operation refused", append(fields, zap.String("outcome", CodeInvalidAuthorization))...)
		respondCode(w, http.StatusBadRequest, CodeInvalidAuthorization)
		return
	}
	if authorization.ExecutorIdentity != peer {
		s.log.Warn("credential store operation refused", append(fields, zap.String("outcome", CodeExecutorMismatch))...)
		respondCode(w, http.StatusForbidden, CodeExecutorMismatch)
		return
	}
	if authorization.CredentialID == "" {
		// An anonymous sync authorization is valid for the executor but has nothing to redeem.
		s.log.Info("credential store operation refused", append(fields, zap.String("outcome", CodeInvalidAuthorization))...)
		respondCode(w, http.StatusBadRequest, CodeInvalidAuthorization)
		return
	}
	secret, err := s.store.redeem(r.Context(), &authorization)
	if err != nil {
		s.failOperation(w, err, fields...)
		return
	}
	s.log.Info("credential store operation", append(fields, zap.String("outcome", "redeemed"))...)
	respond(w, http.StatusOK, redemptionResponse{Username: secret.Username, Token: secret.Token, Version: authorization.CredentialVersion})
}

func (s *Server) getMetadata(w http.ResponseWriter, r *http.Request, peer string) {
	id, ok := validCredentialPath(w, r)
	if !ok {
		return
	}
	metadata, err := s.store.metadata(r.Context(), id)
	if err != nil {
		s.failOperation(w, err, zap.String("operation", "get"), zap.String("peer", peer), zap.String("credentialId", id))
		return
	}
	respond(w, http.StatusOK, metadata)
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request, peer string) {
	id, ok := validCredentialPath(w, r)
	if !ok || !readEmpty(w, r) {
		return
	}
	fields := []zap.Field{zap.String("operation", "revoke"), zap.String("peer", peer), zap.String("credentialId", id)}
	if err := s.store.revoke(r.Context(), id); err != nil {
		s.failOperation(w, err, fields...)
		return
	}
	s.log.Info("credential store operation", append(fields, zap.String("outcome", "revoked"))...)
	respond(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) rewrap(w http.ResponseWriter, r *http.Request, peer string) {
	if !readEmpty(w, r) {
		return
	}
	fields := []zap.Field{zap.String("operation", "rewrap"), zap.String("peer", peer), zap.String("kekId", s.store.KeyID())}
	count, err := s.store.rewrap(r.Context())
	if err != nil {
		s.failOperation(w, err, append(fields, zap.Int("rewrapped", count))...)
		return
	}
	s.log.Info("credential store operation", append(fields, zap.Int("rewrapped", count), zap.String("outcome", "rewrapped"))...)
	respond(w, http.StatusOK, map[string]int{"rewrapped": count})
}

// verifyReason classifies a verification failure for the log without echoing the token.
func verifyReason(err error) string {
	switch {
	case errors.Is(err, cloneauth.ErrExpired):
		return "expired"
	case errors.Is(err, cloneauth.ErrSignature):
		return "signature"
	case errors.Is(err, cloneauth.ErrMalformed):
		return "malformed"
	default:
		return "invalid"
	}
}
