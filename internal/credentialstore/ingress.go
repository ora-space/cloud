package credentialstore

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// maxTokenLength bounds a submitted token. Real HTTPS Git tokens are far shorter; the bound keeps
// the volume from becoming an unbounded write target.
const maxTokenLength = 8192

// maxUsernameLength bounds a submitted username.
const maxUsernameLength = 256

// DefaultUsername is the HTTPS username stored when a submission omits one; GitHub and most
// forges accept any non-empty username with a token as the password.
const DefaultUsername = "x-access-token"

// submissionScheme is the Authorization scheme of a submission grant.
const submissionScheme = "Ora-Submission "

// IngressHandler serves the public browser ingress. It has exactly one route,
// PUT /v1/credentials/{credentialId}/versions (plus its CORS preflight), and no route that reads
// a secret back. It authenticates only by the submission grant: no cookie is read and
// Access-Control-Allow-Credentials is never set, so there is no ambient authority to forge.
func (s *Server) IngressHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/credentials/{credentialId}/versions", s.withCORS(s.submit))
	mux.HandleFunc("OPTIONS /v1/credentials/{credentialId}/versions", s.withCORS(preflight))
	mux.HandleFunc("/", notFound)
	return mux
}

// withCORS admits requests without an Origin (non-browser callers still need a valid grant) and
// browser requests only from a configured origin, which it echoes exactly.
func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.origins[origin] {
				s.log.Info("credential store request refused",
					zap.String("listener", listenerIngress), zap.String("outcome", CodeOriginForbidden))
				respondCode(w, http.StatusForbidden, CodeOriginForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		next(w, r)
	}
}

// preflight answers the CORS preflight for the single PUT route.
func preflight(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Access-Control-Request-Method") != http.MethodPut {
		respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", http.MethodPut)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// submissionRequest is the ingress body. Token is the secret; it is never logged or echoed.
type submissionRequest struct {
	Token    string `json:"token"`
	Username string `json:"username,omitempty"`
}

// submit verifies the grant with the Store's own clock and writes the version the grant names.
func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	fields := []zap.Field{zap.String("operation", "submit"), zap.String("listener", listenerIngress)}
	if !s.limiter.allow(remoteIP(r)) {
		s.log.Info("credential store request refused", append(fields, zap.String("outcome", CodeRateLimited))...)
		respondCode(w, http.StatusTooManyRequests, CodeRateLimited)
		return
	}
	id, ok := validCredentialPath(w, r)
	if !ok {
		return
	}
	fields = append(fields, zap.String("credentialId", id))
	raw, hasScheme := strings.CutPrefix(r.Header.Get("Authorization"), submissionScheme)
	if !hasScheme || raw == "" {
		s.log.Info("credential store request refused", append(fields, zap.String("outcome", CodeInvalidSubmission))...)
		respondCode(w, http.StatusUnauthorized, CodeInvalidSubmission)
		return
	}
	grant, err := cloneauth.VerifySubmission(raw, s.submissionKeys, s.now())
	if err != nil {
		code := CodeInvalidSubmission
		if errors.Is(err, cloneauth.ErrExpired) {
			code = CodeSubmissionExpired
		}
		s.log.Info("credential store request refused", append(fields, zap.String("reason", verifyReason(err)), zap.String("outcome", code))...)
		respondCode(w, http.StatusUnauthorized, code)
		return
	}
	fields = append(fields, zap.String("submissionId", grant.SubmissionID), zap.Int64("version", grant.ExpectedStoreVersion))
	if grant.CredentialID != id {
		s.log.Warn("credential store request refused", append(fields, zap.String("outcome", CodeSubmissionMismatch))...)
		respondCode(w, http.StatusForbidden, CodeSubmissionMismatch)
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var body submissionRequest
	if !readStrict(w, r, &body) {
		return
	}
	secret := Secret{Username: body.Username, Token: body.Token}
	body = submissionRequest{}
	if secret.Username == "" {
		secret.Username = DefaultUsername
	}
	if !validSecretField(secret.Token, maxTokenLength) || !validSecretField(secret.Username, maxUsernameLength) ||
		strings.ContainsRune(secret.Username, ':') {
		s.log.Info("credential store request refused", append(fields, zap.String("outcome", CodeInvalidRequest))...)
		respondCode(w, http.StatusBadRequest, CodeInvalidRequest)
		return
	}
	stored, err := s.store.submit(r.Context(), &grant, secret)
	if err != nil {
		s.failOperation(w, err, fields...)
		return
	}
	s.log.Info("credential store operation", append(fields, zap.String("outcome", "stored"))...)
	respond(w, http.StatusOK, map[string]int64{"storeVersion": stored})
}

// validSecretField accepts 1..limit bytes of UTF-8 without CR, LF or NUL. Those characters would
// let a value inject lines into the git credential protocol the executor's helper speaks.
func validSecretField(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

// remoteIP is the rate-limit key: the connection's remote address without the port.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseOrigins validates the configured browser origins: each must be exactly scheme://host[:port]
// with https, or http only for a loopback development origin.
func parseOrigins(origins []string) (map[string]bool, error) {
	if len(origins) == 0 {
		return nil, errors.New("credential store ingress requires at least one allowed origin")
	}
	set := make(map[string]bool, len(origins))
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" ||
			parsed.Fragment != "" || parsed.String() != origin || origin == "*" {
			return nil, fmt.Errorf("allowed origin %q is not scheme://host[:port]", origin)
		}
		loopback := parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()).IsLoopback()
		if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
			return nil, fmt.Errorf("allowed origin %q must use https", origin)
		}
		set[origin] = true
	}
	return set, nil
}
