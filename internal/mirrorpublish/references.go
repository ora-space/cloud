package mirrorpublish

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// liveReferences is the query that decides which mirrors may exist.
//
// It is written out rather than composed from helpers because this projection *is* the
// authorization: an edit that adds or drops a predicate should have to be made here, in front of the
// reader, rather than being inherited from a join. Four facts have to hold together, and each one is
// a way a key could otherwise be published without the binding D2 requires:
//
//   - the Workspace is not deleted, so a mirror is not kept alive by a Workspace that is gone;
//   - it carries a pinned commit, because a Workspace whose clone never completed names no immutable
//     content and a mirror of "whatever it last had" is exactly what D6's key exists to prevent;
//   - its Project is active and not deleted, so a Project being torn down does not authorize
//     publication while its mirrors are on their way to being reclaimed;
//   - `clone_executions` has a completed clone for that Workspace, which is the execution binding:
//     without it, a row that only claims a commit would be enough to put content in the store.
//
// `requested_ref` is carried rather than derived. The writer fetches that branch and refuses to
// publish unless its tip is the pinned commit, so the branch Cloud recorded is what makes the mirror
// the commit Cloud bound, rather than a mirror of wherever the branch has since moved to.
const liveReferences = `
SELECT w.project_id, p.tenant_id, p.repository_url, w.base_commit_id, w.requested_ref
FROM workspaces w
JOIN projects p ON p.id = w.project_id
WHERE w.deleted_at IS NULL
  AND w.base_commit_id IS NOT NULL
  AND p.deleted_at IS NULL
  AND p.lifecycle = 'active'
  AND EXISTS (SELECT 1 FROM clone_executions c
              WHERE c.workspace_id = w.id AND c.result->>'outcome' = 'clone_ready')
ORDER BY w.project_id, w.base_commit_id`

// liveProjects is every Project that may still be compared against a stored mirror.
//
// It is separate from the reference query because withdrawal is about the Project, not about a
// Workspace: a Project that changed its upstream must withdraw the mirrors of the repository it left
// even when no Workspace pins one any more. Only the identity is selected — the writer never needs a
// Project's name, owner or branch to decide what may exist.
const liveProjects = `
SELECT id, tenant_id, repository_url
FROM projects
WHERE deleted_at IS NULL AND lifecycle = 'active'
ORDER BY id`

// References answers Cloud's mirror references from PostgreSQL.
//
// It reads outside any control transaction, for the same reason the read authorization does: this is
// a point-in-time question about state a mutation would change, and answering it must not queue
// behind the cluster-wide lock or hold one across a fetch.
type References struct {
	// Pool is Cloud's database handle. It is used read-only.
	Pool *sql.DB
}

// Binding returns every live Project and every mirror key Cloud's rows reference right now.
//
// A row that cannot be turned into a mirror key is skipped rather than failing the answer: a Project
// whose stored URL the canonicalizer refuses is a Project no mirror can be addressed for, and one
// such project must not stop every other tenant's mirrors from being published. The same applies to
// a stored commit that is not a full object name.
func (r *References) Binding(ctx context.Context) (Binding, error) {
	if r.Pool == nil {
		return Binding{}, errors.New("the reference reader needs Cloud's database pool")
	}
	binding := Binding{}
	projects, err := r.Pool.QueryContext(ctx, liveProjects)
	if err != nil {
		return Binding{}, fmt.Errorf("read the live projects: %w", err)
	}
	defer func() { _ = projects.Close() }()
	for projects.Next() {
		var project Project
		var repositoryURL string
		if err := projects.Scan(&project.ID, &project.TenantID, &repositoryURL); err != nil {
			return Binding{}, fmt.Errorf("read a live project: %w", err)
		}
		// A repository that cannot be canonicalized leaves the field empty, which the caller reads as
		// "not comparable" and never as "points somewhere else" — that difference is what keeps an
		// unusable URL from withdrawing a mirror.
		if canonical, err := cloneauth.CanonicalRepository(repositoryURL); err == nil {
			project.Repository = canonical
		}
		binding.Projects = append(binding.Projects, project)
	}
	if err := projects.Err(); err != nil {
		return Binding{}, fmt.Errorf("read the live projects: %w", err)
	}
	references, err := r.Pool.QueryContext(ctx, liveReferences)
	if err != nil {
		return Binding{}, fmt.Errorf("read the live mirror references: %w", err)
	}
	defer func() { _ = references.Close() }()
	for references.Next() {
		var projectID, tenantID, repositoryURL, commit, upstream string
		if err := references.Scan(&projectID, &tenantID, &repositoryURL, &commit, &upstream); err != nil {
			return Binding{}, fmt.Errorf("read a live mirror reference: %w", err)
		}
		canonical, err := cloneauth.CanonicalRepository(repositoryURL)
		if err != nil {
			continue
		}
		key := mirror.Key{ProjectID: projectID, Repository: canonical, Commit: commit}
		if err := key.Validate(); err != nil {
			continue
		}
		binding.Live = append(binding.Live, Reference{Key: key, TenantID: tenantID, Upstream: upstream})
	}
	if err := references.Err(); err != nil {
		return Binding{}, fmt.Errorf("read the live mirror references: %w", err)
	}
	binding.Live = dedupe(binding.Live)
	return binding, nil
}

// dedupe removes repeated keys. Two Workspaces of one Project resolve to the same key by D6's
// design — that is what makes key reuse work — and a repeat would otherwise make the writer look at
// the same mirror twice and report it twice.
func dedupe(references []Reference) []Reference {
	seen := make(map[string]struct{}, len(references))
	unique := references[:0]
	for _, reference := range references {
		digest := reference.Key.Digest()
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		unique = append(unique, reference)
	}
	return unique
}
