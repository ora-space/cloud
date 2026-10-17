package repoexec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"go.uber.org/zap"
)

// TaskRunner runs one admitted task. *Executor implements it.
type TaskRunner interface {
	Run(ctx context.Context, task *Task) Result
}

// Server is the executor's task listener: mTLS, one client identity, a fixed number of slots.
type Server struct {
	runner TaskRunner
	slots  chan struct{}
	log    *zap.Logger
}

// NewServer returns a task server with slots concurrent tasks.
func NewServer(runner TaskRunner, slots int, log *zap.Logger) (*Server, error) {
	if runner == nil || slots <= 0 {
		return nil, errors.New("task server needs a runner and at least one slot")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{runner: runner, slots: make(chan struct{}, slots), log: log}, nil
}

// ServerTLS loads the executor's server certificate and requires a client certificate chained to the
// dispatcher CA. The handler then requires the dispatcher identity itself.
func ServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("executor TLS identity unavailable")
	}
	pool, err := loadPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}, nil
}

func loadPool(file string) (*x509.CertPool, error) {
	data, err := os.ReadFile(file) //nolint:gosec // G304: deployment configuration.
	if err != nil {
		return nil, errors.New("CA file unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("CA file holds no certificate")
	}
	return pool, nil
}

// Handler serves POST /v1/tasks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", s.task)
	return mux
}

// peerIdentity is the single URI SAN of the verified client leaf; a leaf with none or several is
// no identity at all.
func peerIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	leaf := r.TLS.PeerCertificates[0]
	if len(leaf.URIs) != 1 {
		return ""
	}
	return leaf.URIs[0].String()
}

// task runs one task synchronously: the response is written when the task ends, however long that
// takes within its own budget. A full executor refuses with 429 before anything starts.
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	if peerIdentity(r) != DispatcherIdentity {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "identity_forbidden"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskBody)
	var task Task
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": CodeInvalidTask})
		return
	}
	select {
	case s.slots <- struct{}{}:
	default:
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"code": CodeExecutorBusy})
		return
	}
	defer func() { <-s.slots }()
	result := s.runner.Run(r.Context(), &task)
	// The authorization is a bearer secret; it must not outlive the task in this process.
	task.Authorization = ""
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
