// Package projectrepos prepares fixed project mirrors before recording projects.
// It never fetches again, writes Agent changes back, or selects conversation directories.
package projectrepos

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// ErrRepositoryUnavailable covers inaccessible repositories and branches without exposing Git output.
var ErrRepositoryUnavailable = errors.New("project repository unavailable")

// Store owns project records and membership checks; preparation completes before CreateProject.
type Store interface {
	CheckExecutionScope(context.Context, execution.Scope) error
	CreateProject(context.Context, execution.Scope, store.Project) (store.Project, error)
	GetProject(context.Context, execution.Scope, string) (store.Project, error)
	ListProjects(context.Context, execution.Scope) ([]store.Project, error)
}

// Options configures a trusted repository folder and executable, never browser input.
type Options struct {
	Root string
	// GitBinary defaults to git resolved from PATH at construction.
	GitBinary string
	// AllowLoopbackHTTP permits numeric loopback HTTP fixture servers in tests only.
	// Production uses credential-free HTTPS repositories.
	AllowLoopbackHTTP bool
}

// Input is the supported synchronous project creation request.
type Input struct {
	SpaceID       string `json:"spaceId,omitempty"`
	Name          string `json:"name"`
	RepositoryURL string `json:"repositoryUrl"`
	DefaultBranch string `json:"defaultBranch"`
}

// Service combines repository preparation with the existing project store.
type Service struct {
	store             Store
	root, git         string
	allowLoopbackHTTP bool
	seedMu            sync.Mutex
}

// New validates and creates the repository root. It does not clone or contact any remote.
func New(s Store, options Options) (*Service, error) {
	if s == nil || !filepath.IsAbs(options.Root) {
		return nil, execution.ErrInvalidRequest
	}
	if err := os.MkdirAll(options.Root, 0700); err != nil {
		return nil, fmt.Errorf("repository root: %w", err)
	}
	root, err := filepath.EvalSymlinks(options.Root)
	if err != nil {
		return nil, err
	}
	git := options.GitBinary
	if git == "" {
		git = "git"
	}
	git, err = exec.LookPath(git)
	if err != nil {
		return nil, fmt.Errorf("git executable: %w", err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		return nil, err
	}
	return &Service{store: s, root: root, git: git, allowLoopbackHTTP: options.AllowLoopbackHTTP}, nil
}

// SnapshotRef names the immutable snapshot expected by the fixed Node clone protocol.
func SnapshotRef(projectID, commit string) string {
	return "refs/ora/upstreams/" + projectID + "/snapshots/" + commit
}

// Create checks membership, clones once and fixes the requested branch before saving a project.
// A failed clone or missing branch creates no project. There is no background sync or retry.
func (s *Service) Create(ctx context.Context, scope execution.Scope, in Input) (store.Project, error) {
	if err := s.store.CheckExecutionScope(ctx, scope); err != nil {
		return store.Project{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 || !utf8.ValidString(in.Name) || !s.validURL(in.RepositoryURL) ||
		in.DefaultBranch == "" || in.DefaultBranch == "HEAD" || len(in.DefaultBranch) > 255 {
		return store.Project{}, execution.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := s.run(ctx, s.root, "check-ref-format", "refs/heads/"+in.DefaultBranch); err != nil {
		if ctx.Err() != nil {
			return store.Project{}, ctx.Err()
		}
		return store.Project{}, execution.ErrInvalidRequest
	}
	stage, err := os.MkdirTemp(s.root, ".prepare-")
	if err != nil {
		return store.Project{}, err
	}
	defer os.RemoveAll(stage) // stage is freshly generated below the configured root.
	mirror := filepath.Join(stage, "repository.git")
	if _, err := s.run(ctx, s.root, "clone", "--mirror", "--template=", "--", in.RepositoryURL, mirror); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	return s.recordMirror(ctx, scope, in, mirror)
}

func (s *Service) recordMirror(ctx context.Context, scope execution.Scope, in Input, mirror string) (store.Project, error) {
	commit, err := s.run(ctx, mirror, "rev-parse", "--verify", "refs/heads/"+in.DefaultBranch+"^{commit}")
	if err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	commit = strings.TrimSpace(commit)
	if len(commit) != 40 && len(commit) != 64 {
		return store.Project{}, ErrRepositoryUnavailable
	}
	id, err := projectID()
	if err != nil {
		return store.Project{}, err
	}
	snapshot := SnapshotRef(id, commit)
	if _, err := s.run(ctx, mirror, "update-ref", snapshot, commit); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	// Remove the upstream configuration after selecting the fixed snapshot, so later code
	// cannot accidentally fetch updates or use credentials against the original remote.
	if _, err := s.run(ctx, mirror, "config", "--remove-section", "remote.origin"); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	final := filepath.Join(s.root, id+".git")
	if err := os.Rename(mirror, final); err != nil {
		return store.Project{}, err
	}
	project := store.Project{SpaceID: in.SpaceID, ID: id, Name: in.Name, RepositoryURL: in.RepositoryURL, Branch: in.DefaultBranch,
		Commit: commit, SnapshotRef: snapshot, MirrorPath: final}
	// Keep the mirror if the database returns an error: a failed connection does not prove
	// the INSERT rolled back. Retain unreferenced mirrors for explicit administrative inspection.
	return s.store.CreateProject(ctx, scope, project)
}

// SeedDemo prepares or reuses the tenant's trusted local fixture repository. It accepts no
// browser URL/path and is never called by the project HTTP handler. This keeps the one-command
// fixture check independent of external networks while still exercising real Git and Node.
func (s *Service) SeedDemo(ctx context.Context, scope execution.Scope) (store.Project, error) {
	s.seedMu.Lock()
	defer s.seedMu.Unlock()
	projects, err := s.store.ListProjects(ctx, scope)
	if err != nil {
		return store.Project{}, err
	}
	for _, project := range projects {
		if project.RepositoryURL == "fixture://demo" {
			return project, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stage, err := os.MkdirTemp(s.root, ".seed-")
	if err != nil {
		return store.Project{}, err
	}
	defer os.RemoveAll(stage)
	source, mirror := filepath.Join(stage, "source"), filepath.Join(stage, "repository.git")
	if err := os.Mkdir(source, 0700); err != nil {
		return store.Project{}, err
	}
	if _, err := s.run(ctx, source, "init", "--template=", "-b", "main"); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("# Ora demo\n\nA local repository for the real Node and fixture-model demonstration.\n"), 0600); err != nil {
		return store.Project{}, err
	}
	if _, err := s.run(ctx, source, "add", "--", "README.md"); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	if _, err := s.run(ctx, source, "-c", "user.name=Ora Demo", "-c", "user.email=demo@localhost", "-c", "commit.gpgsign=false", "commit", "-m", "Prepare demo repository"); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	// Only this internally generated source may use Git's file transport. The public Create
	// path remains HTTPS-only; no option can turn browser-supplied paths into fixture sources.
	if _, err := s.run(ctx, stage, "-c", "protocol.file.allow=always", "clone", "--mirror", "--no-hardlinks", "--template=", "--", source, mirror); err != nil {
		return store.Project{}, repositoryError(ctx)
	}
	return s.recordMirror(ctx, scope, Input{Name: "Demo", RepositoryURL: "fixture://demo", DefaultBranch: "main"}, mirror)
}

// Get returns a tenant-visible project; private mirror paths are stripped by the HTTP layer.
func (s *Service) Get(ctx context.Context, scope execution.Scope, id string) (store.Project, error) {
	return s.store.GetProject(ctx, scope, id)
}

// List returns only projects visible to the current tenant member.
func (s *Service) List(ctx context.Context, scope execution.Scope) ([]store.Project, error) {
	return s.store.ListProjects(ctx, scope)
}

func (s *Service) validURL(raw string) bool {
	if len(raw) > 4096 || strings.TrimSpace(raw) != raw {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return s.allowLoopbackHTTP && u.Scheme == "http" && err == nil && ip.IsLoopback()
}

func (s *Service) run(ctx context.Context, dir string, args ...string) (string, error) {
	config := []string{"-c", "credential.helper=", "-c", "credential.interactive=false", "-c", "core.askPass=",
		"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "http.followRedirects=false"}
	if s.allowLoopbackHTTP {
		config = append(config, "-c", "protocol.http.allow=always")
	}
	cmd := exec.CommandContext(ctx, s.git, append(config, args...)...)
	cmd.Dir = dir
	// Do not inherit HOME, config, SSH, askpass, credential helpers, proxies, netrc or Git env.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_SSH_COMMAND=/bin/false"}
	cmd.WaitDelay = time.Second
	output := &limitedOutput{}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	err := cmd.Run()
	return output.String(), err
}

type limitedOutput struct{ strings.Builder }

func (o *limitedOutput) Write(p []byte) (int, error) {
	if o.Len()+len(p) > 4096 {
		return 0, errors.New("git output exceeds limit")
	}
	return o.Builder.Write(p)
}

func repositoryError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrRepositoryUnavailable
}

func projectID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
