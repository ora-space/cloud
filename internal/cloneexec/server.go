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
	mirror   *MirrorExecutor
	push     *PushExecutor
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
	// Mirror is the mirror-preparation executor, or nil when the deployment declared no mirror
	// capability. It is optional in a way the clone executor is not: filling the internal Git Server's
	// mirrors is a separate capability from cloning into a Workspace, and a deployment that runs only
	// one of them must be able to say so. A nil value leaves the mirror route answering
	// `credential_executor_unavailable` rather than absent, so a dispatcher that reaches it learns the
	// deployment did not establish the capability instead of guessing at a routing mistake.
	Mirror *MirrorExecutor
	// Push is the push executor, or nil when the deployment declared no push capability. It is optional
	// for the mirror's reason and one more: writing to a user's upstream is the only capability in this
	// component that changes something outside the platform, and a deployment has to be able to run C
	// for clones and preparations while refusing pushes outright. A nil value leaves the push route
	// answering `credential_executor_unavailable` rather than absent, for the same reason.
	Push *PushExecutor
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
	return &Server{executor: executor, mirror: cfg.Mirror, push: cfg.Push, caller: cfg.Caller, log: log}, nil
}

// Handler returns the executor's HTTP surface.
//
// The three capabilities are three routes rather than one route with a discriminator, because they
// carry different job shapes and different identities: a clone job names an execution and a Node, a
// mirror job names an Operation and an attempt, a push job names an approval and the write it
// authorizes. A single route would have to decode all three and decide which one it was looking at,
// and that decision would be C's rather than the signature's (the 2026-10-13 ruling).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/clone-executions", s.dispatch)
	mux.HandleFunc("POST /v1/mirror-preparations", s.prepare)
	mux.HandleFunc("POST /v1/push-executions", s.pushExecution)
	return mux
}

// dispatch performs one job for the one caller allowed to ask for it.
//
// The caller's identity comes from the verified client certificate and never from the request: a
// header, a path, or a body field cannot make a caller into the dispatcher. The job's own fields are
// decoded strictly, and the authorization they carry is a bearer secret that is cleared before any
// log line or error path can see it.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.callerIdentity(w, r)
	if !ok {
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
		s.failure(w, r, identity, job.ExecutionID, err)
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

// prepare performs one mirror preparation for the one caller allowed to ask for it.
//
// It is the mirror route's half of `dispatch`, and it differs in one thing that matters: an
// unconfigured deployment answers `credential_executor_unavailable` rather than 404. Filling the
// internal Git Server is a capability a deployment either established or did not, and telling a
// dispatcher which of the two it is by the shape of a routing error would make "this deployment cannot
// prepare mirrors" indistinguishable from "you sent the request to the wrong path".
//
// The authorization is cleared before any log line or error path can see it, exactly as on the clone
// route.
func (s *Server) prepare(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.callerIdentity(w, r)
	if !ok {
		return
	}
	if s.mirror == nil {
		s.refuse(w, identity, http.StatusInternalServerError, ReasonCodeUnavailable,
			errors.New("this deployment declared no mirror preparation capability"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJobBody)
	var job MirrorJob
	if !decode(w, r, &job) {
		return
	}
	result, err := s.mirror.Prepare(r.Context(), job)
	job.Authorization = ""
	if err != nil {
		s.failure(w, r, identity, job.OperationID, err)
		return
	}
	s.log.Info("mirror executor completed a preparation",
		zap.String("serviceIdentity", identity),
		zap.String("operationId", job.OperationID),
		zap.String("commit", result.Commit),
		zap.Int64("generation", result.Generation),
	)
	// The result is written as the type the caller will decode it into, so the wire shape and the
	// structure cannot drift into two spellings of one answer.
	respond(w, http.StatusOK, result)
}

// pushExecution performs one controlled push for the one caller allowed to ask for it.
//
// It is the push route's half of `dispatch`, and it keeps the rule the mirror route established: an
// unconfigured deployment answers `credential_executor_unavailable` rather than 404. Writing to a
// person's upstream is a capability a deployment either established or did not, and a dispatcher has to
// be able to tell "this deployment cannot push" from "you sent the request to the wrong path".
//
// Its one deliberate difference from the other two routes is that a job which *ran* always answers 200,
// including the attempts that failed and the attempts whose outcome is unknown: the reply carries the
// state, and the state is the answer. An error means the executor never reached the write — a refusal it
// decided, reported with its stable code, or a failure of the credential path, reported as the executor
// being unavailable. That split is what lets the dispatcher settle a request as failed from a coded
// refusal and as uncertain from a lost or unreadable reply, rather than reading every non-200 as an
// unknown outcome.
//
// The authorization is cleared before any log line or error path can see it, exactly as on the other two
// routes.
func (s *Server) pushExecution(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.callerIdentity(w, r)
	if !ok {
		return
	}
	if s.push == nil {
		s.refuse(w, identity, http.StatusInternalServerError, ReasonCodeUnavailable,
			errors.New("this deployment declared no push execution capability"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJobBody)
	var job PushJob
	if !decode(w, r, &job) {
		return
	}
	result, err := s.push.Push(r.Context(), job)
	job.Authorization = ""
	if err != nil {
		s.failure(w, r, identity, job.OperationID, err)
		return
	}
	s.log.Info("push executor completed a job",
		zap.String("serviceIdentity", identity),
		zap.String("operationId", job.OperationID),
		zap.String("repository", job.Repository),
		zap.String("ref", job.Ref),
		zap.String("state", string(result.State)),
		zap.Int64("credentialVersion", result.CredentialVersion),
	)
	// The result is written as the type the caller will decode it into, so the wire shape and the
	// structure cannot drift into two spellings of one answer.
	respond(w, http.StatusOK, result)
}

// callerIdentity resolves and checks the dispatching service identity, answering the request itself
// when the peer is not the one dispatcher this deployment accepts.
//
// It is shared by all three routes so they cannot drift into three identity stories: there is one
// dispatcher, and whether it is asking for a clone, a mirror preparation, or a push is a question about
// the body, not about who it is.
func (s *Server) callerIdentity(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, err := presentedIdentity(r.TLS)
	if err != nil {
		s.refuse(w, "", http.StatusUnauthorized, "service_identity_required", err)
		return "", false
	}
	if identity != s.caller {
		s.refuse(w, identity, http.StatusForbidden, "service_identity_required", errors.New("identity is not the executor's dispatcher"))
		return "", false
	}
	return identity, true
}

// failure maps a coded refusal to its wire code.
//
// The cause is written to the executor's own log and never to the response: it can carry the Store's
// reason code, a Git error, or a filesystem path, and the caller receives the executor's own stable
// code instead. A caller that went away is not a job failure and is not logged as one.
//
// The subject is whichever identifier the route's job carried — an execution for a clone, an operation
// for a preparation — and it is logged so a refusal can be matched to the request that caused it.
// Neither identifier is a secret.
//
// A refusal the job itself decided is answered whatever else its error carries, and that ordering is
// the point rather than a detail. The job's own deadline expiring is the ordinary way a redeem, a
// fetch, or a write runs out of time — and the refusal it produced was written to stand on its own
// with the deadline as its *cause*, which is exactly the shape a test for "did this reach the
// network" looks for. Reading the cause first dropped those refusals: the handler returned without
// writing anything, and net/http sent `200` with an empty body, which is not an answer any caller can
// settle a request with. A push settled from it is recorded as an outcome nobody stated, when the
// executor had in fact concluded that the write never happened.
func (s *Server) failure(w http.ResponseWriter, r *http.Request, identity, subject string, err error) {
	var failure *Failure
	if !errors.As(err, &failure) {
		// Nothing decided the failure, so the only thing left to read is who ended the job. A
		// cancellation or deadline that belongs to the *caller* — a dispatch Cloud abandoned, a
		// connection that dropped, a listener shutting down — has nobody left to answer it and is not a
		// job failure, so this route writes nothing and logs nothing.
		if r.Context().Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return
		}
		// Anything else is an executor that could not carry the job to an answer. The caller is still
		// waiting for one, so it is answered rather than left with a `200` whose body says nothing.
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
	case ReasonCodePushFetchFailed, ReasonCodePushNotApplicable:
		// Both are outcomes of a job that was well formed: the executor could not read the repository it
		// was told to write to, or the authorized write is not one that repository's content supports.
		// Neither is a protocol error by the caller, so both take the conflict status every refusal with
		// no more specific meaning takes, and the dispatcher settles the request as a confirmed failure
		// carrying this code. Naming them here rather than letting them fall through keeps that decision
		// deliberate: a later push code must be given a status on purpose.
		status = http.StatusConflict
	}
	s.log.Error("credential executor refused a job",
		zap.String("serviceIdentity", identity),
		zap.String("subject", subject),
		zap.String("reason", failure.ReasonCode()),
		zap.Error(failure.Unwrap()),
	)
	respond(w, status, map[string]string{"code": failure.ReasonCode(), "message": "credential executor refused the job"})
}

// refuse reports a request refused before any job ran.
func (s *Server) refuse(w http.ResponseWriter, identity string, status int, code string, cause error) {
	s.log.Warn("credential executor request refused",
		zap.String("serviceIdentity", identity),
		zap.String("reason", code),
		zap.Error(cause),
	)
	respond(w, status, map[string]string{"code": code, "message": "credential executor refused the request"})
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
