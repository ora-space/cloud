package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// endpoint is one of the three smart HTTP operations; nothing else is ever handed to git.
type endpoint int

const (
	advertiseUploadPack endpoint = iota + 1
	advertiseReceivePack
	uploadPack
	receivePack
)

func (e endpoint) writes() bool { return e == advertiseReceivePack || e == receivePack }

// repositoryRequest is a parsed and validated smart HTTP request path.
type repositoryRequest struct {
	tenantID, projectID string
	endpoint            endpoint
}

// relativePath is the http-backend PATH_INFO: built from validated IDs, never from the request.
func (r repositoryRequest) relativePath() string {
	return "/" + r.tenantID + "/" + r.projectID + ".git"
}

// parseRepositoryRequest accepts exactly the smart HTTP surface. Dumb HTTP files, other methods
// and any path that is not /{tenantId}/{projectId}.git/... with canonical UUIDs are rejected.
func parseRepositoryRequest(method, path, query string) (repositoryRequest, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		return repositoryRequest{}, false
	}
	tenant, project := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !canonicalID(tenant) || !canonicalID(project) {
		return repositoryRequest{}, false
	}
	r := repositoryRequest{tenantID: tenant, projectID: project}
	switch rest := strings.Join(parts[2:], "/"); {
	case method == "GET" && rest == "info/refs" && query == "service=git-upload-pack":
		r.endpoint = advertiseUploadPack
	case method == "GET" && rest == "info/refs" && query == "service=git-receive-pack":
		r.endpoint = advertiseReceivePack
	case method == "POST" && rest == "git-upload-pack" && query == "":
		r.endpoint = uploadPack
	case method == "POST" && rest == "git-receive-pack" && query == "":
		r.endpoint = receivePack
	default:
		return repositoryRequest{}, false
	}
	return r, true
}

// canonicalID admits only the lowercase hyphenated UUID form, so one Project has one path.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// deliveryRef is the only ref shape the ref query answers; it matches what core authorizes.
var deliveryRef = regexp.MustCompile("^" + regexp.QuoteMeta(core.GitDeliveryRefPrefix) + `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// repositoryConfig is fixed by the server; clients cannot change it. Deletions and
// non-fast-forwards are refused by Git itself as a second line behind the hook, objects are
// checked on receipt, and push options are never advertised.
func repositoryConfig() [][2]string {
	return [][2]string{
		{"http.receivepack", "true"},
		{"receive.denyDeletes", "true"},
		{"receive.denyNonFastForwards", "true"},
		{"receive.fsckObjects", "true"},
		{"receive.advertisePushOptions", "false"},
		{"uploadpack.allowAnySHA1InWant", "false"},
	}
}

// repositories owns the bare repository tree under one root.
type repositories struct {
	root, git string
}

func (r *repositories) path(tenantID, projectID string) string {
	return filepath.Join(r.root, tenantID, projectID+".git")
}

// exists reports whether a Project repository has been created; read requests never create one.
func (r *repositories) exists(tenantID, projectID string) (bool, error) {
	info, err := os.Stat(r.path(tenantID, projectID))
	switch {
	case err == nil:
		return info.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// ensure creates the Project repository on its first authorized write. It is initialized and
// configured in a private sibling directory and published with one rename, so a concurrent first
// push either wins the rename or finds the complete repository; nobody sees a half-configured one.
func (r *repositories) ensure(ctx context.Context, tenantID, projectID string) error {
	final := r.path(tenantID, projectID)
	if ok, err := r.exists(tenantID, projectID); err != nil || ok {
		return err
	}
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return fmt.Errorf("create tenant directory: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".init-")
	if err != nil {
		return fmt.Errorf("stage repository: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := r.run(ctx, "", "init", "--quiet", "--bare", staging); err != nil {
		return err
	}
	for _, kv := range repositoryConfig() {
		if err := r.run(ctx, staging, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, final); err != nil {
		if ok, existsErr := r.exists(tenantID, projectID); existsErr == nil && ok {
			return nil
		}
		return fmt.Errorf("publish repository: %w", err)
	}
	return nil
}

// resolve returns the commit a delivery ref names, or "" when the repository or ref is absent.
func (r *repositories) resolve(ctx context.Context, tenantID, projectID, ref string) (string, error) {
	ok, err := r.exists(tenantID, projectID)
	if err != nil || !ok {
		return "", err
	}
	cmd := exec.CommandContext(ctx, r.git, "--git-dir", r.path(tenantID, projectID), "for-each-ref", "--format=%(refname) %(objectname) %(objecttype)", ref) //nolint:gosec // G204: IDs are canonical UUIDs and ref matched deliveryRef.
	cmd.Env = gitEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read ref: %w", err)
	}
	// for-each-ref matches path prefixes, so only an exact refname counts.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == ref {
			if fields[2] != "commit" {
				return "", errors.New("delivery ref does not name a commit")
			}
			return fields[1], nil
		}
	}
	return "", nil
}

// run executes git with a minimal environment; repository administration never reads user or
// system Git configuration.
func (r *repositories) run(ctx context.Context, gitDir string, args ...string) error {
	if gitDir != "" {
		args = append([]string{"--git-dir", gitDir}, args...)
	}
	cmd := exec.CommandContext(ctx, r.git, args...) //nolint:gosec // G204: arguments are server-built; IDs are canonical UUIDs.
	cmd.Env = gitEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitEnvironment isolates every git process from global and system configuration.
func gitEnvironment() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "LC_ALL=C"}
}
