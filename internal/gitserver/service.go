// Package gitserver serves Project repositories to sandboxes over smart HTTP. A git-access client
// certificate only names the runtime target; every request is authorized from current Cloud state
// for the execution it declares, and pushes are checked again in pre-receive before any ref is
// written. The server keeps no authorization state of its own.
package gitserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: Httpoxy affects Go < 1.6.3 only; cgi drops the Proxy header.
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
)

// ExecutionHeader names the execution a sandbox repository request serves. It selects which
// current permission applies; it can never choose the authenticated runtime target.
const ExecutionHeader = "Ora-Execution-Id"

// OperationHeader names the repository Operation a Repo Executor request serves.
const OperationHeader = "Ora-Git-Operation-Id"

// Store is the authoritative policy the repository boundary consumes.
type Store interface {
	AuthorizeGitAccess(ctx context.Context, scope core.GitRuntimeScope, executionID, projectID string) (core.GitGrant, error)
	AuthorizeGitOperation(ctx context.Context, operationID, tenantID, projectID string) (core.GitGrant, error)
}

// Options injects policy, storage and process wiring.
type Options struct {
	Store Store
	// RepositoryRoot holds the bare repositories; it must be absolute.
	RepositoryRoot string
	// GitProgram runs http-backend and repository administration.
	GitProgram string
	// CloudClient is the DER of the only certificate allowed to query refs.
	CloudClient []byte
	// Hook is the command Git runs as pre-receive; it must call RunPreReceive.
	Hook   []string
	Health func(context.Context) error
	Logger *zap.Logger
}

// Service owns the repository tree and the private hook channel.
type Service struct {
	options  Options
	repos    *repositories
	pushes   *pushes
	runtime  string
	hooks    string
	listener net.Listener
	cancel   context.CancelFunc
	done     sync.WaitGroup
}

// New validates dependencies, installs the pre-receive hook in a private runtime directory and
// starts the hook channel. Close releases both.
func New(input *Options) (*Service, error) {
	if input == nil {
		return nil, errors.New("git server options are required")
	}
	o := *input
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.Store == nil || o.Health == nil || o.GitProgram == "" || len(o.Hook) == 0 || len(o.CloudClient) == 0 || !filepath.IsAbs(o.RepositoryRoot) {
		return nil, errors.New("git server store, health, git program, hook, Cloud client and absolute repository root are required")
	}
	// cgi runs Path without a PATH lookup, and a fixed absolute program also keeps every git
	// process of this server on the same binary.
	git, err := exec.LookPath(o.GitProgram)
	if err != nil {
		return nil, errors.New("git server git program unavailable")
	}
	if o.GitProgram, err = filepath.Abs(git); err != nil {
		return nil, errors.New("git server git program unavailable")
	}
	// Some distributions package http-backend separately from git (Alpine: git-daemon). Without it
	// every authorized request would fail at the CGI step, so refuse to start instead.
	if err := requireHTTPBackend(o.GitProgram); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.RepositoryRoot, 0o750); err != nil {
		return nil, errors.New("git server repository root unavailable")
	}
	runtime, err := os.MkdirTemp("", "ora-git-server-")
	if err != nil {
		return nil, errors.New("git server runtime directory unavailable")
	}
	hooks := filepath.Join(runtime, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		_ = os.RemoveAll(runtime)
		return nil, errors.New("git server hook directory unavailable")
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(hookScript(o.Hook)), 0o700); err != nil { //nolint:gosec // G306: git must execute the hook; the directory is private (0700).
		_ = os.RemoveAll(runtime)
		return nil, errors.New("git server hook unavailable")
	}
	listener, err := net.Listen("unix", filepath.Join(runtime, "hook.sock"))
	if err != nil {
		_ = os.RemoveAll(runtime)
		return nil, errors.New("git server hook channel unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{options: o, repos: &repositories{root: o.RepositoryRoot, git: o.GitProgram}, pushes: &pushes{active: map[string]push{}}, runtime: runtime, hooks: hooks, listener: listener, cancel: cancel}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		s.serveHooks(ctx, listener)
	}()
	return s, nil
}

func requireHTTPBackend(git string) error {
	cmd := exec.Command(git, "--exec-path") //nolint:gosec // G204: the deployment's git program, resolved above.
	cmd.Env = gitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return errors.New("git server git program unavailable")
	}
	info, err := os.Stat(filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"))
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("git server requires git http-backend")
	}
	return nil
}

// Close stops the hook channel after the HTTP servers have drained, then removes runtime files.
func (s *Service) Close() error {
	s.cancel()
	err := s.listener.Close()
	s.done.Wait()
	return errors.Join(err, os.RemoveAll(s.runtime))
}

// listener is the network boundary a repository request arrived on. Each accepts one identity and
// one context header, and runs Git with its own repository policy.
type listener int

const (
	// sandboxListener serves sandbox executions holding git-access certificates.
	sandboxListener listener = iota + 1
	// serviceListener serves the Repo Executor holding its service certificate.
	serviceListener
)

// header is the one context header requests on this listener must carry.
func (l listener) header() string {
	if l == serviceListener {
		return OperationHeader
	}
	return ExecutionHeader
}

// RepositoryHandler serves the smart HTTP surface to sandboxes on the repository listener.
func (s *Service) RepositoryHandler(w http.ResponseWriter, r *http.Request) {
	s.serveRepository(w, r, sandboxListener)
}

// ServiceRepositoryHandler serves the smart HTTP surface to the Repo Executor on the service
// repository listener.
func (s *Service) ServiceRepositoryHandler(w http.ResponseWriter, r *http.Request) {
	s.serveRepository(w, r, serviceListener)
}

func (s *Service) serveRepository(w http.ResponseWriter, r *http.Request, on listener) {
	request, ok := parseRepositoryRequest(r.Method, r.URL.Path, r.URL.RawQuery)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	p := push{listener: on, tenant: request.tenantID, project: request.projectID}
	var err error
	if on == serviceListener {
		err = RequireExecutor(r)
	} else {
		p.scope, err = RuntimeScope(r)
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "git_certificate_required")
		return
	}
	// Exactly one context header, and the one this listener serves: a request that names both,
	// or another listener's context, is malformed whatever its certificate.
	contexts, foreign := r.Header.Values(on.header()), r.Header.Values(ExecutionHeader)
	if on == sandboxListener {
		foreign = r.Header.Values(OperationHeader)
	}
	if len(contexts) != 1 || len(foreign) != 0 || on == sandboxListener && !validExecutionID(contexts[0]) || on == serviceListener && !canonicalID(contexts[0]) {
		code := "git_execution_required"
		if on == serviceListener {
			code = "git_operation_required"
		}
		writeError(w, http.StatusBadRequest, code)
		return
	}
	p.context = contexts[0]
	// A path naming another tenant is answered exactly like an unauthorized repository.
	if on == sandboxListener && request.tenantID != p.scope.TenantID {
		writeError(w, http.StatusForbidden, "git_access_denied")
		return
	}
	grant, err := s.authorize(r.Context(), &p)
	if err != nil {
		var fault *core.Fault
		if errors.As(err, &fault) {
			writeError(w, http.StatusForbidden, "git_access_denied")
			return
		}
		s.options.Logger.Warn("git authorization unavailable", zap.String("projectId", request.projectID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	}
	if request.endpoint.writes() {
		if !grant.Writes() {
			writeError(w, http.StatusForbidden, "git_access_denied")
			return
		}
		if err := s.repos.ensure(r.Context(), request.tenantID, request.projectID); err != nil {
			s.options.Logger.Warn("git repository unavailable", zap.String("projectId", request.projectID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
			return
		}
	} else if exists, err := s.repos.exists(request.tenantID, request.projectID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	} else if !exists {
		// An absent repository is indistinguishable from one the caller may not read.
		writeError(w, http.StatusForbidden, "git_access_denied")
		return
	}
	env := append(gitEnvironment(), "GIT_PROJECT_ROOT="+s.options.RepositoryRoot, "GIT_HTTP_EXPORT_ALL=1")
	env = append(env, policyEnvironment(s.hooks, on)...)
	if request.endpoint == receivePack {
		token, release := s.pushes.register(&p)
		defer release()
		env = append(env, hookSocketEnv+"="+s.listener.Addr().String(), pushTokenEnv+"="+token)
	}
	backend := &cgi.Handler{Path: s.options.GitProgram, Args: []string{"http-backend"}, Root: "/", Env: env, Stderr: io.Discard, Logger: log.New(io.Discard, "", 0)}
	forwarded := r.Clone(r.Context())
	forwarded.URL.Path, forwarded.URL.RawPath = request.relativePath()+suffix(request.endpoint), ""
	backend.ServeHTTP(w, forwarded)
}

// authorize resolves the request's current grant from Cloud state by the listener's identity.
// The grant must be for exactly the repository the request addresses.
func (s *Service) authorize(ctx context.Context, p *push) (core.GitGrant, error) {
	var grant core.GitGrant
	var err error
	if p.listener == serviceListener {
		grant, err = s.options.Store.AuthorizeGitOperation(ctx, p.context, p.tenant, p.project)
	} else {
		grant, err = s.options.Store.AuthorizeGitAccess(ctx, p.scope, p.context, p.project)
	}
	if err == nil && (grant.TenantID != p.tenant || grant.ProjectID != p.project) {
		return core.GitGrant{}, &core.Fault{Code: "git_access_denied", Status: http.StatusForbidden}
	}
	return grant, err
}

func suffix(e endpoint) string {
	switch e {
	case advertiseUploadPack, advertiseReceivePack:
		return "/info/refs"
	case uploadPack:
		return "/git-upload-pack"
	case receivePack:
		return "/git-receive-pack"
	}
	return ""
}

// policyEnvironment applies the server's repository policy and this process's hook to every git
// process of a request. The hook path lives in a per-process runtime directory and is never stored
// in a repository, so a restart cannot leave a repository pointing at a missing hook; and a
// repository created by an older release cannot weaken the policy. These variables come from the
// server, never from the client.
//
// The sandbox listener keeps Git's own non-fast-forward refusal as a second line behind the hook.
// The service listener turns it off so a tracking ref can follow an external force-push; the hook
// still refuses non-fast-forwards in every other namespace.
func policyEnvironment(hooks string, on listener) []string {
	config := append(repositoryConfig(), [2]string{"core.hooksPath", hooks})
	if on == serviceListener {
		config = append(config, [2]string{"receive.denyNonFastForwards", "false"})
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(config))}
	for i, kv := range config {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	return env
}

// validExecutionID matches node_executions.execution_id: 1..200 visible ASCII characters.
func validExecutionID(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for i := range len(s) {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// RefHandler answers Cloud's read-only ref query and the unauthenticated health route on the ref
// listener. Only Cloud's pinned leaf may query. A query names either one ref of a contract
// namespace (?ref=) or a prefix inside one namespace instance (?prefix=), whose refs it lists. The
// answer is the repository's state at query time; the server records nothing.
func (s *Service) RefHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.health(w, r)
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/refs" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || !bytes.Equal(r.TLS.PeerCertificates[0].Raw, s.options.CloudClient) {
		writeError(w, http.StatusUnauthorized, "cloud_certificate_required")
		return
	}
	query := r.URL.Query()
	single := func(key string) string {
		if v := query[key]; len(v) == 1 {
			return v[0]
		}
		return ""
	}
	tenant, project, ref, prefix := single("tenantId"), single("projectId"), single("ref"), single("prefix")
	if len(query) != 3 || !canonicalID(tenant) || !canonicalID(project) {
		writeError(w, http.StatusBadRequest, "invalid_ref_query")
		return
	}
	if prefix != "" {
		if !core.ValidGitRefPrefix(prefix) {
			writeError(w, http.StatusBadRequest, "invalid_ref_query")
			return
		}
		refs, err := s.repos.list(r.Context(), tenant, project, prefix)
		if err != nil {
			s.options.Logger.Warn("git ref query unavailable", zap.String("projectId", project), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"refs": refs})
		return
	}
	if _, ok := core.ParseGitRef(ref); !ok {
		writeError(w, http.StatusBadRequest, "invalid_ref_query")
		return
	}
	commit, err := s.repos.resolve(r.Context(), tenant, project, ref)
	switch {
	case err != nil:
		s.options.Logger.Warn("git ref query unavailable", zap.String("projectId", project), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
	case commit == "":
		writeError(w, http.StatusNotFound, "ref_not_found")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"commit": commit})
	}
}

// health reports database and repository-volume readiness without disclosing configuration.
func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if r.Method != http.MethodGet || s.options.Health(ctx) != nil {
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	}
	if _, err := os.Stat(s.options.RepositoryRoot); err != nil {
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"code": code, "params": map[string]any{}, "requestId": uuid.NewString()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
