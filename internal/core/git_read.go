package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// gitReadBinding is the one query every read authorization is decided from. It is written out
// rather than built from the generic helpers for two reasons: `workspaces` and `projects` share
// column names, so every column has to be named explicitly; and this projection *is* the
// authorization input, so a later edit that adds or drops a column should have to be made here, in
// front of the reader, rather than being inherited from `SELECT *`.
//
// The sandbox join is LEFT and carries the predicates that define "live" — unterminated, running,
// and the generation the workspace currently runs. A workspace with no live sandbox, or with one
// from an earlier generation, produces no sandbox columns and is therefore not a live container.
const gitReadBinding = `
SELECT s.id AS sandbox_id, s.generation AS sandbox_generation, s.observed_state AS sandbox_state,
       w.id AS workspace_id, w.tenant_id AS workspace_tenant_id, w.project_id AS workspace_project_id,
       w.runtime_generation AS runtime_generation, w.base_commit_id AS base_commit_id,
       w.requested_ref AS requested_ref, w.deleted_at AS workspace_deleted_at,
       p.tenant_id AS project_tenant_id, p.repository_url AS repository_url,
       p.lifecycle AS project_lifecycle, p.deleted_at AS project_deleted_at
FROM workspaces w
JOIN projects p ON p.id = w.project_id
LEFT JOIN sandbox_instances s
       ON s.workspace_id = w.id AND s.terminated_at IS NULL AND s.observed_state = 'running'
      AND s.generation = w.runtime_generation
WHERE w.id = $1`

// ReadGitMirror answers Cloud's authoritative state for one read of one mirror by one caller.
//
// It answers the binding and only the binding: which workspace and container the caller's identity
// resolved to, whether that container is still the live one, which project and tenant that
// workspace belongs to, and which mirror key this workspace's content is addressed by. It does not
// answer whether the mirror is ready or what generation it has — that is the mirror store's own
// state, and the caller composes the two.
//
// The mirror key is *derived*, never taken from the request. The request names a digest; this
// method recomputes the digest this workspace is currently entitled to — its project's canonical
// upstream repository and the commit the workspace is pinned to — and answers with it. A caller
// holding a digest for a mirror that is not its own therefore produces a key whose digest does not
// equal what it asked for, and gitread refuses the mismatch. There is no table mapping digests to
// keys, and no lookup that could be made to return someone else's row.
//
// A workspace that has not completed its clone has no pinned commit yet, and the derived key is
// therefore not usable; gitread refuses it rather than serving whatever the workspace last had.
func (s *Store) ReadGitMirror(ctx context.Context, origin gitread.Origin, digest string) (gitread.Snapshot, error) {
	if err := origin.Validate(); err != nil {
		// An origin that is not an identity cannot be matched against anything. It is refused by
		// evaluation, so answering with an empty snapshot keeps this method from being the place
		// where a malformed identity accidentally becomes a lookup.
		return gitread.Snapshot{}, nil
	}
	row, err := s.gitReadRow(ctx, origin.WorkspaceID)
	if err != nil {
		return gitread.Snapshot{}, err
	}
	if row == nil {
		// The workspace the resolver named does not exist. Nothing about this request can be
		// authorized, and the caller learns only that the read was refused.
		return gitread.Snapshot{}, nil
	}
	snapshot := gitread.Snapshot{
		ContainerLive:      row.live(origin.ContainerID),
		WorkspaceID:        row.workspaceID,
		ContainerID:        row.sandboxID,
		RuntimeGeneration:  row.runtimeGeneration,
		WorkspaceProjectID: row.workspaceProjectID,
		WorkspaceTenantID:  row.workspaceTenantID,
		ProjectTenantID:    row.projectTenantID,
	}
	key, ok := row.mirrorKey()
	if !ok {
		// The workspace exists and its container may well be live, but there is no mirror it can be
		// authorized for. Found stays false, which is a refusal.
		return snapshot, nil
	}
	snapshot.Found = true
	snapshot.Key = key
	return snapshot, nil
}

// gitReadState is one row of the binding query, with the nullable columns already resolved.
type gitReadState struct {
	workspaceID        string
	workspaceTenantID  string
	workspaceProjectID string
	runtimeGeneration  int64
	baseCommit         string
	workspaceDeleted   bool
	projectTenantID    string
	repositoryURL      string
	projectLifecycle   string
	projectDeleted     bool
	sandboxID          string
	sandboxGeneration  int64
	sandboxState       string
}

// live reports whether the named container is the live sandbox instance of this workspace.
//
// Three facts have to hold at once, and each of them is a way a stale caller could otherwise keep
// reading: the workspace must still be usable, a running unterminated sandbox must exist, and it
// must be the container the resolver named at the generation the workspace currently runs. The
// database enforces at most one unterminated sandbox per workspace, so "the id matches" and "it is
// the live one" are the same statement — and an older generation has a different instance id.
func (r *gitReadState) live(containerID string) bool {
	if r.workspaceDeleted || r.projectDeleted || r.projectLifecycle != "active" {
		return false
	}
	if r.sandboxID == "" || r.sandboxState != "running" {
		return false
	}
	if r.sandboxGeneration != r.runtimeGeneration || r.runtimeGeneration <= 0 {
		return false
	}
	return r.sandboxID == containerID
}

// mirrorKey derives the mirror key this workspace is addressed by. It reports false when the
// workspace cannot be addressed at all: a repository that is not a usable https reference, or no
// pinned commit yet.
func (r *gitReadState) mirrorKey() (mirror.Key, bool) {
	repository, err := cloneauth.CanonicalRepository(r.repositoryURL)
	if err != nil {
		return mirror.Key{}, false
	}
	key := mirror.Key{ProjectID: r.workspaceProjectID, Repository: repository, Commit: r.baseCommit}
	if err := key.Validate(); err != nil {
		return mirror.Key{}, false
	}
	return key, true
}

// gitReadRow loads one workspace's binding, resolving NULLs to their zero values. It reads outside
// the control plane's transaction: a read authorization is a point-in-time question about state the
// caller cannot change, it must not queue behind the cluster-wide advisory lock that serializes
// mutations, and holding that lock per clone would make every clone a writer.
func (s *Store) gitReadRow(ctx context.Context, workspaceID string) (*gitReadState, error) {
	if !validID(workspaceID) {
		return nil, nil
	}
	var row gitReadState
	var baseCommit, sandboxID, sandboxState, requestedRef sql.NullString
	var sandboxGeneration sql.NullInt64
	var workspaceDeleted, projectDeleted sql.NullTime
	err := s.Pool.QueryRowContext(ctx, gitReadBinding, workspaceID).Scan(
		&sandboxID, &sandboxGeneration, &sandboxState,
		&row.workspaceID, &row.workspaceTenantID, &row.workspaceProjectID,
		&row.runtimeGeneration, &baseCommit, &requestedRef, &workspaceDeleted,
		&row.projectTenantID, &row.repositoryURL, &row.projectLifecycle, &projectDeleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the workspace binding: %w", err)
	}
	row.workspaceDeleted = workspaceDeleted.Valid
	row.projectDeleted = projectDeleted.Valid
	row.baseCommit = baseCommit.String
	row.sandboxID = sandboxID.String
	row.sandboxState = sandboxState.String
	row.sandboxGeneration = sandboxGeneration.Int64
	return &row, nil
}
