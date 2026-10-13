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
// reader, rather than being inherited from a join.
//
// # Why this is a union of two halves
//
// A mirror is kept while something still names it (D6), and in this channel two different somethings
// can: a read that could be authorized right now, and a clone that has been authorized but has not
// consumed the mirror yet. Each half takes the three facts of the key from the same columns its own
// consumer reads them from, so "live" here means "one of the two would derive this key":
//
//   - the read half reproduces `gitReadState.mirrorKey` — the Workspace's Project, that Project's
//     canonical upstream, and the pinned baseline. It is why a mirror a live sandbox is entitled to
//     read can never be reclaimed out from under it;
//   - the clone half reproduces `Store.cloneInputFor` — the preparation's own repository (which must
//     still agree with the Project's, since a Project may be rebound) and the commit the preparation
//     published. Between publication and the `node` -> `clone` gate that pins the baseline, the first
//     half names nothing: the mirror is ready and the Workspace it was prepared for has no baseline
//     yet. Without this half a sweep in that window would reclaim the mirror of an Operation that is
//     in flight, and the gate would then refuse a Workspace whose mirror had just been published.
//
// The clone half is limited to Operations that have not settled. Its preparation row is durable and
// outlives the Operation, so without that limit every mirror a credentialed Project ever prepared
// would stay referenced forever by the first row that named it and the sweep would never reclaim
// anything. Once the Operation settles, the Workspace either carries the pinned baseline — which is
// the read half's business, and it keeps the mirror for as long as that Workspace lives — or it does
// not, and the mirror is genuinely unreferenced.
//
// # The latitude this takes, and which way it points
//
// `cloneInputFor` compares three more things before it accepts a preparation: the pinned ref against
// the Workspace's requested ref, the preparation's runtime generation against the Workspace's, and
// the preparation's credential against the Project's. None of them is here. Each is a comparison that
// can only *narrow* the set of keys a consumer would derive — the key is the Project, the repository
// and the commit, and none of the three changes any of those — so this list is a superset of the keys
// the two consumers would name, never a subset.
//
// The direction is deliberate. A key that is missing from this list is a mirror the sweep is free to
// destroy, so an under-approximation would remove content a live consumer is about to read, while an
// over-approximation only keeps a mirror until its Operation settles. Reproduction here is therefore
// exact about the key and generous about the binding, and a case where the two disagree resolves
// towards keeping the content.
//
// # What this replaced, and why
//
// The two halves above replaced a single query whose fourth conjunct was
// `EXISTS (SELECT 1 FROM clone_executions c WHERE c.workspace_id = w.id AND c.result->>'outcome' =
// 'clone_ready')`. That conjunct reads "this Workspace has already cloned", and it is the *reverse*
// of this channel's order: D7 publishes the mirror before the clone, and the clone cannot succeed
// until the mirror exists. A reference set defined by a completed clone therefore named no mirror
// until the clone that needed one had already run, so a reconciling pass over it could neither
// publish nor keep the mirror the clone was waiting for. It was also narrower than the truth in a
// second way: `clone_executions` is not where the reader's authorization comes from — the Git Server
// derives the key from the Workspace's baseline and asks only the store — so a Workspace whose clone
// had completed while its baseline named a different commit would have kept a mirror nothing read.
//
// # The channel predicate
//
// Both halves are limited to Projects that name a credential, because that is what makes a mirror
// this deployment's content channel at all: `cloneInputFor` sends a Project with no credential to its
// upstream and never to a mirror, so a key derived for one would be content no clone input and no
// reader is entitled to, published at the cost of copying a public repository into the volume.
//
// `pinned_ref` and `requested_ref` are carried rather than derived. The writer fetches that branch
// and refuses to publish unless its tip is the pinned commit, so the branch Cloud recorded is what
// makes the mirror the commit Cloud bound, rather than a mirror of wherever the branch has since
// moved to. The two halves store the branch in different spellings — the gate's is qualified, the
// Workspace's is not — and the fetch takes the short name, so the clone half strips the one prefix
// the schema guarantees is there (0034's `pinned_ref` CHECK).
const liveReferences = `
SELECT w.project_id, p.tenant_id, p.repository_url, w.base_commit_id, w.requested_ref
FROM workspaces w
JOIN projects p ON p.id = w.project_id
WHERE w.deleted_at IS NULL
  AND w.base_commit_id IS NOT NULL
  AND p.deleted_at IS NULL
  AND p.lifecycle IN ('provisioning', 'active')
  AND p.credential_ref_id IS NOT NULL
UNION ALL
SELECT m.project_id, m.tenant_id, m.repository_url, m.commit_id,
       substring(m.pinned_ref FROM length('refs/heads/') + 1)
FROM mirror_preparations m
JOIN projects p ON p.id = m.project_id
JOIN operations o ON o.id = m.operation_id
WHERE m.state = 'ready'
  AND o.state NOT IN ('succeeded', 'failed')
  AND p.deleted_at IS NULL
  AND p.lifecycle IN ('provisioning', 'active')
  AND p.credential_ref_id IS NOT NULL
  AND m.repository_url = p.repository_url
ORDER BY 1, 4`

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
// the same mirror twice and report it twice. The union produces the same overlap for a third reason:
// a Workspace whose baseline the gate has already pinned satisfies both halves at once.
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
