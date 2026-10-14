// Package gitserver serves Project repositories to sandboxes over smart HTTP, and serves published
// mirrors to Workspaces on a second listener that is authenticated a different way.
//
// The two are one program, one set of dependencies and one lifecycle, and two surfaces that cannot
// reach each other's routes:
//
//   - the *repository* surface (this file, with `repository.go`, `scope.go` and `hook.go`) serves
//     `/{tenantId}/{projectId}.git` to a caller that presents a git-access mTLS client certificate.
//     The certificate only names the runtime target; every request is authorized from current Cloud
//     state for the execution it declares, and pushes are checked again in pre-receive before any ref
//     is written. It is the only surface that can write.
//   - the *mirror* surface (`mirror.go`) serves a published mirror at an opaque digest path to a
//     caller that presents nothing at the transport layer and is identified from the address its
//     connection arrived from. It is read-only, and `mirror.go` records why it cannot share the
//     repository listener's TLS configuration.
//
// The server keeps no authorization state of its own: both surfaces decide every request from Cloud
// state read for that request.
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

// ExecutionHeader names the execution a repository request serves. It selects which current
// permission applies; it can never choose the authenticated runtime target.
const ExecutionHeader = "Ora-Execution-Id"

// Store is the authoritative policy the repository boundary consumes.
type Store interface {
	AuthorizeGitAccess(ctx context.Context, scope core.GitRuntimeScope, executionID, projectID string) (core.GitAccess, error)
}

// Options injects policy, storage and process wiring. Each surface is wired separately and either
// may be absent, so a process can serve the repository surface, the mirror surface, or both; a
// process given neither is refused rather than started as a server with no routes.
type Options struct {
	// GitProgram runs every git process of this server, of both surfaces. It has no default: a
	// deployment states which binary it fixed rather than leaving the choice to PATH.
	GitProgram string
	// Repository wires the repository surface. Nil means this process serves no Project
	// repositories: no listener is opened for it, no pre-receive channel exists, and no request can
	// reach it however it is addressed.
	Repository *RepositoryOptions
	// Mirror wires the read-only mirror surface. Nil means this process serves no mirrors.
	Mirror *MirrorOptions
	// Health reports whether the process can reach the Cloud state both surfaces authorize from.
	// Required: readiness that is not answerable is readiness an operator cannot act on.
	Health func(context.Context) error
	// Logger records decisions. Nil discards them.
	Logger *zap.Logger
}

// RepositoryOptions is the repository surface's wiring. Nothing here is optional when the surface is
// present: every field is a trust anchor or a directory it owns.
type RepositoryOptions struct {
	Store Store
	// RepositoryRoot holds the bare repositories; it must be absolute.
	RepositoryRoot string
	// CloudClient is the DER of the only certificate allowed to query refs.
	CloudClient []byte
	// Hook is the command Git runs as pre-receive; it must call RunPreReceive.
	Hook []string
}

// Service owns the surfaces this process was given, the repository tree, and the private hook
// channel.
//
// The git program is resolved once, here, and handed to both surfaces. That is deliberate rather
// than incidental reuse: the two surfaces run the same binary, and a deployment that could point
// them at different ones could make a mount visible to a read that its policy never authorized.
type Service struct {
	repository *RepositoryOptions
	mirror     *mirrorRead
	git        string
	log        *zap.Logger
	health     func(context.Context) error
	repos      *repositories
	pushes     *pushes
	runtime    string
	hooks      string
	listener   net.Listener
	cancel     context.CancelFunc
	done       sync.WaitGroup
}

// New validates dependencies, resolves the one git program, installs the pre-receive hook in a
// private runtime directory and starts the hook channel when there is a repository surface to serve.
// Close releases all of it.
func New(input *Options) (*Service, error) {
	if input == nil {
		return nil, errors.New("git server options are required")
	}
	o := *input
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.Repository == nil && o.Mirror == nil {
		return nil, errors.New("git server requires at least one surface to serve")
	}
	if o.Health == nil {
		return nil, errors.New("git server health is required")
	}
	if o.GitProgram == "" {
		return nil, errors.New("git server git program is required")
	}
	if o.Repository != nil {
		if o.Repository.Store == nil || len(o.Repository.Hook) == 0 || len(o.Repository.CloudClient) == 0 || !filepath.IsAbs(o.Repository.RepositoryRoot) {
			return nil, errors.New("git server store, hook, Cloud client and absolute repository root are required")
		}
	}
	// cgi runs Path without a PATH lookup, and a fixed absolute program also keeps every git
	// process of this server on the same binary.
	git, err := exec.LookPath(o.GitProgram)
	if err != nil {
		return nil, errors.New("git server git program unavailable")
	}
	if git, err = filepath.Abs(git); err != nil {
		return nil, errors.New("git server git program unavailable")
	}
	runtime, err := os.MkdirTemp("", "ora-git-server-")
	if err != nil {
		return nil, errors.New("git server runtime directory unavailable")
	}
	s := &Service{repository: o.Repository, git: git, log: o.Logger, health: o.Health, runtime: runtime}
	// The mirror surface's HOME is this directory rather than one of its own: it is created and
	// removed with every other runtime file, so a surface cannot outlive the process's own cleanup.
	if o.Mirror != nil {
		home := filepath.Join(runtime, "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			return s.fail(errors.New("git server mirror home unavailable"))
		}
		if s.mirror, err = newMirrorRead(*o.Mirror, git, home, o.Logger); err != nil {
			return s.fail(err)
		}
	}
	if o.Repository == nil {
		// Nothing below belongs to a surface this process does not serve, and a repository root or a
		// hook channel created for one would be state no request can reach.
		_, s.cancel = context.WithCancel(context.Background())
		return s, nil
	}
	// Some distributions package http-backend separately from git (Alpine: git-daemon). Without it
	// every authorized request would fail at the CGI step, so refuse to start instead.
	if err := requireHTTPBackend(git); err != nil {
		return s.fail(err)
	}
	if err := os.MkdirAll(o.Repository.RepositoryRoot, 0o750); err != nil {
		return s.fail(errors.New("git server repository root unavailable"))
	}
	hooks := filepath.Join(runtime, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return s.fail(errors.New("git server hook directory unavailable"))
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(hookScript(o.Repository.Hook)), 0o700); err != nil { //nolint:gosec // G306: git must execute the hook; the directory is private (0700).
		return s.fail(errors.New("git server hook unavailable"))
	}
	listener, err := net.Listen("unix", filepath.Join(runtime, "hook.sock"))
	if err != nil {
		return s.fail(errors.New("git server hook channel unavailable"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.hooks, s.listener = cancel, hooks, listener
	s.repos = &repositories{root: o.Repository.RepositoryRoot, git: git}
	s.pushes = &pushes{active: map[string]push{}}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		s.serveHooks(ctx, listener)
	}()
	return s, nil
}

// fail discards everything New created so a refused startup leaves no runtime directory behind. It
// returns the error unchanged so the caller can `return s.fail(err)`.
func (s *Service) fail(err error) (*Service, error) {
	_ = os.RemoveAll(s.runtime)
	return nil, err
}

// requireHTTPBackend refuses to start a repository surface whose git cannot serve smart HTTP.
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
	if s.cancel != nil {
		s.cancel()
	}
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.done.Wait()
	return errors.Join(err, os.RemoveAll(s.runtime))
}

// RepositoryHandler serves the smart HTTP surface to sandboxes on the repository listener. A service
// built without that surface answers not-found here, which is what "no such route exists in this
// process" means: a caller cannot turn a disabled surface into an enabled one by addressing it.
func (s *Service) RepositoryHandler(w http.ResponseWriter, r *http.Request) {
	if s.repository == nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	request, ok := parseRepositoryRequest(r.Method, r.URL.Path, r.URL.RawQuery)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	scope, err := RuntimeScope(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "git_certificate_required")
		return
	}
	executions := r.Header.Values(ExecutionHeader)
	if len(executions) != 1 || !validExecutionID(executions[0]) {
		writeError(w, http.StatusBadRequest, "git_execution_required")
		return
	}
	// A path naming another tenant is answered exactly like an unauthorized repository.
	if request.tenantID != scope.TenantID {
		writeError(w, http.StatusForbidden, "git_access_denied")
		return
	}
	access, err := s.repository.Store.AuthorizeGitAccess(r.Context(), scope, executions[0], request.projectID)
	if err != nil {
		var fault *core.Fault
		if errors.As(err, &fault) {
			writeError(w, http.StatusForbidden, "git_access_denied")
			return
		}
		s.log.Warn("git authorization unavailable", zap.String("workspaceId", scope.WorkspaceID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	}
	if request.endpoint.writes() {
		if access.Mode != core.GitWrite {
			writeError(w, http.StatusForbidden, "git_access_denied")
			return
		}
		if err := s.repos.ensure(r.Context(), request.tenantID, request.projectID); err != nil {
			s.log.Warn("git repository unavailable", zap.String("projectId", request.projectID), zap.Error(err))
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
	env := append(gitEnvironment(), "GIT_PROJECT_ROOT="+s.repository.RepositoryRoot, "GIT_HTTP_EXPORT_ALL=1")
	env = append(env, policyEnvironment(s.hooks)...)
	if request.endpoint == receivePack {
		token, release := s.pushes.register(push{scope: scope, execution: executions[0], project: request.projectID})
		defer release()
		env = append(env, hookSocketEnv+"="+s.listener.Addr().String(), pushTokenEnv+"="+token)
	}
	backend := &cgi.Handler{Path: s.git, Args: []string{"http-backend"}, Root: "/", Env: env, Stderr: io.Discard, Logger: log.New(io.Discard, "", 0)}
	forwarded := r.Clone(r.Context())
	forwarded.URL.Path, forwarded.URL.RawPath = request.relativePath()+suffix(request.endpoint), ""
	backend.ServeHTTP(w, forwarded)
}

// MirrorHandler serves the read-only mirror surface on its own listener. Like RepositoryHandler, a
// service built without that surface answers not-found rather than serving anything.
func (s *Service) MirrorHandler(w http.ResponseWriter, r *http.Request) {
	if s.mirror == nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	s.mirror.ServeHTTP(w, r)
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
func policyEnvironment(hooks string) []string {
	config := append(repositoryConfig(), [2]string{"core.hooksPath", hooks})
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

// RefHandler answers Cloud's read-only delivery-ref query and the unauthenticated health route on
// the ref listener. The query is the only operation and only Cloud's pinned leaf may use it.
func (s *Service) RefHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		s.healthRoute(w, r)
		return
	}
	if s.repository == nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/refs" {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || !bytes.Equal(r.TLS.PeerCertificates[0].Raw, s.repository.CloudClient) {
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
	tenant, project, ref := single("tenantId"), single("projectId"), single("ref")
	if len(query) != 3 || !canonicalID(tenant) || !canonicalID(project) || !deliveryRef.MatchString(ref) {
		writeError(w, http.StatusBadRequest, "invalid_ref_query")
		return
	}
	commit, err := s.repos.resolve(r.Context(), tenant, project, ref)
	switch {
	case err != nil:
		s.log.Warn("git ref query unavailable", zap.String("projectId", project), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
	case commit == "":
		writeError(w, http.StatusNotFound, "ref_not_found")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"commit": commit})
	}
}

// healthRoute reports readiness for the surfaces this process was given, without disclosing
// configuration: whether the state both surfaces decide from is reachable, and whether each present
// surface's own durable root is still there.
func (s *Service) healthRoute(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if r.Method != http.MethodGet || s.health(ctx) != nil {
		writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
		return
	}
	roots := []string{}
	if s.repository != nil {
		roots = append(roots, s.repository.RepositoryRoot)
	}
	if s.mirror != nil {
		roots = append(roots, s.mirror.cfg.Mirrors.Root())
	}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			writeError(w, http.StatusServiceUnavailable, "git_service_unavailable")
			return
		}
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
