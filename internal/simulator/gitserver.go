package simulator

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitpreset"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

// GitServerBaseURL is the repository base the simulated deployment writes into inputs. Nothing
// listens there: the simulated Node and Cloud's ref resolver map it onto Root on disk.
const GitServerBaseURL = "https://git-server.simulator.invalid:8445"

// GitServer stands in for the Git Server: the Project repositories it would serve, laid out on disk
// exactly as the Git Server lays out its volume, so the git-server-preset fixtures write snapshots
// into it unchanged. It enforces no ref rules; the real Git Server's authorization has its own tests.
type GitServer struct {
	Root string
}

// NewGitServer creates the repository root.
func NewGitServer(root string) (*GitServer, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	return &GitServer{Root: absolute}, nil
}

// Config is the Git Server as a Store uses it: inputs name GitServerBaseURL and ref queries read
// the repositories on disk.
func (g *GitServer) Config() *core.GitServer {
	return &core.GitServer{RepositoryBaseURL: GitServerBaseURL, Refs: g}
}

// Repository maps a repository URL from a fixed input to its bare repository on disk.
func (g *GitServer) Repository(repositoryURL string) (string, bool) {
	rest, ok := strings.CutPrefix(repositoryURL, GitServerBaseURL+"/")
	if !ok {
		return "", false
	}
	tenant, project, ok := strings.Cut(strings.TrimSuffix(rest, ".git"), "/")
	if !ok || !strings.HasSuffix(rest, ".git") || uuid.Validate(tenant) != nil || uuid.Validate(project) != nil {
		return "", false
	}
	return gitserver.RepositoryPath(g.Root, tenant, project), true
}

// URL is the repository URL of one Project.
func (g *GitServer) URL(tenantID, projectID string) string {
	return GitServerBaseURL + "/" + url.PathEscape(tenantID) + "/" + url.PathEscape(projectID) + ".git"
}

// ResolveGitRef answers Cloud's ref query from the repository on disk. An absent repository or ref
// is the definitive not-found answer, as the Git Server gives it.
func (g *GitServer) ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (string, error) {
	if _, ok := core.ParseGitRef(ref); !ok {
		return "", fmt.Errorf("ref outside the contract namespaces")
	}
	repo := gitserver.RepositoryPath(g.Root, tenantID, projectID)
	if _, err := os.Stat(repo); os.IsNotExist(err) {
		return "", core.ErrGitRefNotFound
	}
	out, err := git(ctx, "--git-dir", repo, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == ref {
			return fields[1], nil
		}
	}
	return "", core.ErrGitRefNotFound
}

// SeedSnapshot stands in for the external line's sync of a Project: it copies the HEAD commit of a
// local source repository into the Project repository and records it as the Binding's current
// snapshot through the git-server-preset fixtures, advancing the Project's pending sync. It returns
// the snapshot commit.
func (g *GitServer) SeedSnapshot(ctx context.Context, db *sql.DB, projectID, source string) (string, error) {
	var tenant string
	if err := db.QueryRowContext(ctx, "SELECT tenant_id FROM projects WHERE id=$1", projectID).Scan(&tenant); err != nil {
		return "", fmt.Errorf("seed snapshot: %w", err)
	}
	repo, err := gitserver.EnsureRepository(ctx, g.Root, "git", tenant, projectID)
	if err != nil {
		return "", err
	}
	commit, err := git(ctx, "-C", source, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	// FETCH_HEAD keeps the objects without writing a ref outside the contract's namespaces; the
	// preset then names them with the snapshot and tracking refs.
	if _, err := git(ctx, "--git-dir", repo, "fetch", "--quiet", "--no-tags", "--", source, "HEAD"); err != nil {
		return "", err
	}
	preset := &gitpreset.Preset{DB: db, Root: g.Root, Git: "git"}
	if _, err := preset.Snapshot(ctx, projectID, commit, 0); err != nil {
		return "", err
	}
	return commit, nil
}
