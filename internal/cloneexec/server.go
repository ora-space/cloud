package cloneexec

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// maxJobBody bounds a clone job. The only variable-length fields are the authorization and the
// repository reference, both already bounded by their issuers, so anything larger is a malformed
// caller rather than a large job.
const maxJobBody = 16 << 10

// Server exposes the executor over mutually authenticated HTTP.
//
// The transport is plain HTTP underneath TLS for the same reason the Credential Service's is: the
// platform's service-identity model is "TLS with a pinned client CA and a SPIFFE URI in the client
// certificate", which net/http expresses completely. A generated protocol would have to be shared
// with Cloud's public contract, and this endpoint is not part of it — it is one internal component
// calling another, and the decisions deliberately keep the authorization off the business protocol.
type Server struct {
	executor *Executor
	caller   string
	log      *zap.Logger
}

// ServerConfig wires the executor's listener.
type ServerConfig struct {
	// TLS must require and verify a client certificate. A configuration that would also serve
	// anonymous callers is refused rather than downgraded: this endpoint spends authorizations and
	// runs clones.
	TLS *tls.Config
	// Caller is the single service identity allowed to dispatch jobs. There is exactly one, because
	// the ability to ask for a credentialed clone is the ability to spend an authorization, and a
	// second dispatcher would be a second party whose jobs this executor cannot distinguish.
	Caller string
	// Log receives one line per job. It never receives a credential or an authorization.
	Log *zap.Logger
}

// NewServer validates the transport and caller configuration and returns a ready server.
func NewServer(executor *Executor, cfg ServerConfig) (*Server, error) {
	if executor == nil {
		return nil, errors.New("clone executor is required")
	}
	if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 || cfg.TLS.ClientCAs == nil || cfg.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		return nil, errors.New("clone executor requires mutually authenticated TLS with a client CA")
	}
	if err := validServiceIdentity(cfg.Caller); err != nil {
		return nil, fmt.Errorf("clone dispatcher identity: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{executor: executor, caller: cfg.Caller, log: log}, nil
}

// Handler returns the executor's HTTP surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/clone-executions", s.dispatch)
	return mux
}

// dispatch performs one job for the one caller allowed to ask for it.
//
// The caller's identity comes from the verified client certificate and never from the request: a
// header, a path, or a body field cannot make a caller into the dispatcher. The job's own fields are
// decoded strictly, and the authorization they carry is a bearer secret that is cleared before any
// log line or error path can see it.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	identity, err := presentedIdentity(r.TLS)
	if err != nil {
		s.refuse(w, "", http.StatusUnauthorized, "service_identity_required", err)
		return
	}
	if identity != s.caller {
		s.refuse(w, identity, http.StatusForbidden, "service_identity_required", errors.New("identity is not the clone dispatcher"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJobBody)
	var job Job
	if !decode(w, r, &job) {
		return
	}
	result, err := s.executor.Execute(r.Context(), job)
	// The authorization is a bearer secret. It must not survive into a log line, an audit record, or
	// an error path.
	job.Authorization = ""
	if err != nil {
		s.failure(w, identity, job.ExecutionID, err)
		return
	}
	s.log.Info("clone executor completed a job",
		zap.String("serviceIdentity", identity),
		zap.String("executionId", job.ExecutionID),
		zap.Int64("credentialVersion", result.CredentialVersion),
	)
	respond(w, http.StatusOK, map[string]any{
		"directory":         result.Directory,
		"commit":            result.Commit,
		"credentialVersion": result.CredentialVersion,
	})
}

// failure maps a coded refusal to its wire code.
//
// The cause is written to the executor's own log and never to the response: it can carry the Store's
// reason code, a Git error, or a filesystem path, and the caller receives the executor's own stable
// code instead. A caller that went away is not a job failure and is not logged as one.
func (s *Server) failure(w http.ResponseWriter, identity, executionID string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		s.refuse(w, identity, http.StatusInternalServerError, ReasonCodeUnavailable, err)
		return
	}
	status := http.StatusConflict
	switch failure.ReasonCode() {
	case ReasonCodeInvalidJob, ReasonCodeBindingMismatch:
		status = http.StatusBadRequest
	case ReasonCodeAuthorizationRejected:
		status = http.StatusForbidden
	case ReasonCodeUnavailable, ReasonCodeCleanupFailed:
		status = http.StatusInternalServerError
	}
	s.log.Error("clone executor refused a job",
		zap.String("serviceIdentity", identity),
		zap.String("executionId", executionID),
		zap.String("reason", failure.ReasonCode()),
		zap.Error(failure.Unwrap()),
	)
	respond(w, status, map[string]string{"code": failure.ReasonCode(), "message": "clone executor refused the job"})
}

// refuse reports a request refused before any job ran.
func (s *Server) refuse(w http.ResponseWriter, identity string, status int, code string, cause error) {
	s.log.Warn("clone executor request refused",
		zap.String("serviceIdentity", identity),
		zap.String("reason", code),
		zap.Error(cause),
	)
	respond(w, status, map[string]string{"code": code, "message": "clone executor refused the request"})
}

// decode reads a strict JSON body. Unknown fields, a malformed document, a second JSON value, and an
// over-long body are all refused: a silently ignored field here would mean Cloud believing it set
// something it did not.
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

// respond writes a JSON response. Responses carry no credential material, and `no-store` keeps an
// intermediary from retaining the checkout path.
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The body is small and already in memory; a write failure here means the client is gone and there
	// is nothing left to report to it.
	_ = json.NewEncoder(w).Encode(body)
}

// presentedIdentity returns the SPIFFE identity a mutually authenticated peer proved.
//
// Verification is manual rather than delegated to a name check: the chain must already have been
// validated by the TLS layer against the client CA, and the identity is the URI in the leaf's subject
// alternative names. The URI is returned as presented and the caller compares it against the one
// dispatcher it accepts, so a certificate that verified but names anything else is refused by that
// comparison rather than treated as anonymous. A certificate carrying no URI at all is refused here,
// because there would be nothing to compare.
func presentedIdentity(state *tls.ConnectionState) (string, error) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return "", errors.New("client certificate is required")
	}
	for _, presented := range state.PeerCertificates[0].URIs {
		return presented.String(), nil
	}
	return "", errors.New("client certificate names no service identity")
}

// validServiceIdentity accepts only a SPIFFE URI naming exactly one workload.
//
// The shape is the one Cloud's control listener and the Credential Service already use, so a reviewer
// reads one rule across the platform, and a URI with extra path segments or a query cannot smuggle a
// second, unchecked name into a certificate.
func validServiceIdentity(identity string) error {
	if strings.TrimSpace(identity) == "" {
		return errors.New("is not configured")
	}
	uri, err := url.Parse(identity)
	if err != nil {
		return errors.New("is not a valid URI")
	}
	if uri.Scheme != "spiffe" || uri.Host == "" {
		return errors.New("is not a spiffe:// URI with a trust domain")
	}
	if uri.RawQuery != "" || uri.Fragment != "" || uri.User != nil {
		return errors.New("must not carry a query, fragment, or user info")
	}
	segments := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return errors.New("must name exactly one workload as /<kind>/<name>")
	}
	return nil
}
