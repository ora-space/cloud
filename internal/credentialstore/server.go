package credentialstore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// maxRequestBody bounds every request body the Store reads. The largest legitimate body is a
// submission (a token of at most maxTokenLength bytes) or a redemption (one signed authorization).
const maxRequestBody = 16 << 10

// Wire error codes. Every refusal is a JSON body {"code":"<code>"}; the codes carry no secret and
// no internal detail, which stays in the Store's structured log.
const (
	CodeInvalidRequest        = "invalid_request"            // 400: malformed, non-strict or invalid body/path
	CodeRequestTooLarge       = "request_too_large"          // 413: body over 16 KiB
	CodeNotFound              = "not_found"                  // 404: no such route on this listener
	CodeIdentityRequired      = "client_identity_required"   // 401: no verified leaf with exactly one URI SAN
	CodeIdentityForbidden     = "client_identity_forbidden"  // 403: identity not trusted by this listener
	CodeExecutorMismatch      = "executor_identity_mismatch" // 403: authorization names another executor
	CodeInvalidAuthorization  = "invalid_authorization"      // 400: signature, shape, purpose, expiry or no credential
	CodeCredentialNotFound    = "credential_not_found"       // 404: no version was ever stored
	CodeCredentialUnavailable = "credential_unavailable"     // 410: revoked, or the version is unknown or past retention
	CodeAlreadyRedeemed       = "already_redeemed"           // 409: the authorization was redeemed before
	CodeInvalidSubmission     = "invalid_submission"         // 401: missing, malformed or forged submission grant
	CodeSubmissionExpired     = "submission_expired"         // 401: the submission grant expired
	CodeSubmissionMismatch    = "submission_mismatch"        // 403: grant names another credential or tenant
	CodeSubmissionReplayed    = "submission_replayed"        // 409: the submission grant was used before
	CodeVersionConflict       = "version_conflict"           // 409: expected version is not current+1
	CodeCredentialRevoked     = "credential_revoked"         // 410: submission to a revoked credential
	CodeOriginForbidden       = "origin_forbidden"           // 403: browser origin not allowed
	CodeRateLimited           = "rate_limited"               // 429: per-IP ingress budget exhausted
	CodeStoreUnavailable      = "store_unavailable"          // 500: storage or decryption failure
)

// ServerOptions configures NewServer. Every field except Logger is required.
type ServerOptions struct {
	Store *Store
	// ExecutionKeys verify Cloud's execution authorizations on the redemption listener.
	ExecutionKeys []ed25519.PublicKey
	// SubmissionKeys verify Cloud's submission grants on the browser ingress. They must not
	// overlap ExecutionKeys: the two grant kinds use distinct key pairs (ADR invariant 3).
	SubmissionKeys []ed25519.PublicKey
	Identities     Identities
	// AllowedOrigins are the exact browser origins (scheme://host[:port]) CORS admits.
	AllowedOrigins []string
	// RatePerMinute and RateBurst bound submissions per remote IP.
	RatePerMinute int
	RateBurst     int
	// Logger receives the audit lines. Nil disables logging.
	Logger *zap.Logger
}

// Server serves the four Store interfaces, each on its own listener.
type Server struct {
	store          *Store
	executionKeys  []ed25519.PublicKey
	submissionKeys []ed25519.PublicKey
	identities     Identities
	origins        map[string]bool
	limiter        *rateLimiter
	log            *zap.Logger
}

// NewServer validates the options and returns a ready server.
func NewServer(opts *ServerOptions) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("credential store server requires a store")
	}
	if len(opts.ExecutionKeys) == 0 || len(opts.SubmissionKeys) == 0 {
		return nil, errors.New("credential store server requires execution and submission public keys")
	}
	for _, a := range opts.ExecutionKeys {
		for _, b := range opts.SubmissionKeys {
			if bytes.Equal(a, b) {
				return nil, errors.New("execution and submission grants must use distinct signing keys")
			}
		}
	}
	if err := opts.Identities.Validate(); err != nil {
		return nil, err
	}
	origins, err := parseOrigins(opts.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	limiter, err := newRateLimiter(opts.RatePerMinute, opts.RateBurst, opts.Store.now)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{
		store: opts.Store, executionKeys: opts.ExecutionKeys, submissionKeys: opts.SubmissionKeys,
		identities: opts.Identities, origins: origins, limiter: limiter, log: log,
	}, nil
}

// now is the Store's clock; every expiry decision on every listener uses it.
func (s *Server) now() time.Time { return s.store.now() }

// peerHandler is a handler that runs after the listener's identity check.
type peerHandler func(w http.ResponseWriter, r *http.Request, peer string)

// requirePeer admits only the one identity a listener trusts. The identity comes from the verified
// client certificate, never from the request, and the body is capped before any handler reads it.
func (s *Server) requirePeer(listener, want string, next peerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, err := PeerIdentity(r.TLS)
		if err != nil {
			s.refuse(w, listener, "", http.StatusUnauthorized, CodeIdentityRequired)
			return
		}
		if peer != want {
			s.refuse(w, listener, peer, http.StatusForbidden, CodeIdentityForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		next(w, r, peer)
	}
}

// refuse logs and answers a request refused before it reached an operation.
func (s *Server) refuse(w http.ResponseWriter, listener, peer string, status int, code string) {
	s.log.Warn("credential store request refused",
		zap.String("listener", listener), zap.String("peer", peer), zap.String("outcome", code))
	respondCode(w, status, code)
}

// storeFailure maps a store error to its wire code. Context cancellation means the caller left;
// nothing is written for it.
func storeFailure(err error) (status int, code string) {
	switch {
	case errors.Is(err, errNotFound):
		return http.StatusNotFound, CodeCredentialNotFound
	case errors.Is(err, errUnavailable):
		return http.StatusGone, CodeCredentialUnavailable
	case errors.Is(err, errAlreadyRedeemed):
		return http.StatusConflict, CodeAlreadyRedeemed
	case errors.Is(err, errRevoked):
		return http.StatusGone, CodeCredentialRevoked
	case errors.Is(err, errVersionConflict):
		return http.StatusConflict, CodeVersionConflict
	case errors.Is(err, errReplayed):
		return http.StatusConflict, CodeSubmissionReplayed
	case errors.Is(err, errTenantMismatch):
		return http.StatusForbidden, CodeSubmissionMismatch
	default:
		return http.StatusInternalServerError, CodeStoreUnavailable
	}
}

// failOperation answers a failed store operation; internal detail goes to the log only. The error
// values the store returns never contain plaintext.
func (s *Server) failOperation(w http.ResponseWriter, err error, fields ...zap.Field) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	status, code := storeFailure(err)
	fields = append(fields, zap.String("outcome", code))
	if status == http.StatusInternalServerError {
		s.log.Error("credential store operation failed", append(fields, zap.Error(err))...)
	} else {
		s.log.Info("credential store operation refused", fields...)
	}
	respondCode(w, status, code)
}

// readStrict decodes one JSON object with no unknown fields and no trailing value, translating a
// body over the cap into request_too_large.
func readStrict(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := decodeStrict(r.Body, target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondCode(w, http.StatusRequestEntityTooLarge, CodeRequestTooLarge)
		} else {
			respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		}
		return false
	}
	return true
}

// decodeStrict decodes exactly one JSON value into target, refusing unknown fields.
func decodeStrict(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return errors.New("trailing content after the JSON value")
	}
	return nil
}

// readEmpty accepts an absent body or an empty JSON object, for routes that take no input.
func readEmpty(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondCode(w, http.StatusRequestEntityTooLarge, CodeRequestTooLarge)
		} else {
			respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		}
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	if decodeStrict(bytes.NewReader(raw), &struct{}{}) != nil {
		respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		return false
	}
	return true
}

func respondCode(w http.ResponseWriter, status int, code string) {
	respond(w, status, map[string]string{"code": code})
}

// respond writes a JSON response that intermediaries must not cache: a redemption response holds
// plaintext, and every other response is kept uniform with it.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The client may be gone; there is nobody left to report a write failure to.
	_ = json.NewEncoder(w).Encode(body)
}

// notFound answers every route a listener does not serve.
func notFound(w http.ResponseWriter, _ *http.Request) {
	respondCode(w, http.StatusNotFound, CodeNotFound)
}

// validCredentialPath extracts and validates the {credentialId} path segment.
func validCredentialPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("credentialId")
	if !validUUID(id) {
		respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		return "", false
	}
	return id, true
}
