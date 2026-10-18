package core

import "context"

// CreateRunWorkspace idempotently creates the isolated Workspace and its create_workspace operation
// for one Agent IssueRun. When the Project already has an operation, it returns busy and creates
// nothing: the caller retries. The Workspace is not reachable from the public Workspace API.
func (s *Store) CreateRunWorkspace(ctx context.Context, runID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object { return createRunWorkspace(t, runID) })
}

// DeleteRunWorkspace idempotently queues deletion of the run Workspace. A busy Project returns busy.
func (s *Store) DeleteRunWorkspace(ctx context.Context, runID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object { return deleteRunWorkspace(t, runID) })
}

func createRunWorkspace(t *transaction, runID string) Object {
	run := t.one("SELECT r.*,i.project_ref FROM issue_runs r JOIN issues i ON i.id=r.issue_id WHERE r.id=$1 AND r.deleted_at IS NULL AND i.deleted_at IS NULL", runID)
	require(run != nil, 404, "not_found")
	require(run.S("executorType") == "agent", 409, "invalid_run")
	require(run.S("projectRef") != "", 409, "issue_project_required")
	if existing := t.one("SELECT * FROM workspaces WHERE issue_run_id=$1", runID); existing != nil {
		op := t.one("SELECT * FROM operations WHERE workspace_id=$1 AND kind='create_workspace' ORDER BY created_at,id LIMIT 1", existing.S("id"))
		return Object{"busy": false, "workspace": existing, "operation": op}
	}
	p := t.one("SELECT * FROM "+projectView+" p WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", run.S("projectRef"), run.S("tenantId"))
	require(p != nil, 404, "not_found")
	if p.S("lifecycle") != "active" || t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", p.S("id")) != nil {
		return Object{"busy": true}
	}
	wid := newID()
	actor := runTriggerActor(t, run)
	ref := p.S("defaultBranch")
	require(ref != "" && ref != "HEAD", 400, "default_branch_required")
	insertWorkspace(t, run.S("tenantId"), p.S("ownerUserId"), actor, p.S("id"), wid, "isolated", ref, "Agent run")
	t.exec("UPDATE workspaces SET issue_run_id=$2 WHERE id=$1", wid, runID)
	op := newOperation(t, &PublicRequest{TenantID: run.S("tenantId"), Key: "run-workspace-" + runID + "-create"}, actor, p.S("id"), wid, "create_workspace", "sandbox", requestHash("run-workspace", runID, Object{"kind": "create"}), Object{})
	return Object{"busy": false, "workspace": t.one("SELECT * FROM workspaces WHERE id=$1", wid), "operation": op}
}

// The append-only enqueue evidence identifies the triggering user, not the Issue or Project owner.
// Missing/ambiguous evidence must be reconciled by business code instead of inventing an actor.
func runTriggerActor(t *transaction, run Object) string {
	actors := t.list("SELECT DISTINCT actor_id FROM issue_activities WHERE tenant_id=$1 AND issue_id=$2 AND action='run.enqueued' AND actor_type='user' AND actor_id IS NOT NULL AND details->>'runId'=$3", run.S("tenantId"), run.S("issueId"), run.S("id"))
	require(len(actors) == 1, 409, "run_actor_required")
	return actors[0].S("actorId")
}

func deleteRunWorkspace(t *transaction, runID string) Object {
	w := t.one("SELECT * FROM workspaces WHERE issue_run_id=$1", runID)
	require(w != nil, 404, "not_found")
	if w["deletedAt"] != nil {
		return Object{"busy": false, "workspace": w}
	}
	if existing := t.one("SELECT * FROM operations WHERE workspace_id=$1 AND kind='delete_workspace' AND state IN ('queued','running','retry_wait','blocked','succeeded') ORDER BY created_at DESC,id DESC LIMIT 1", w.S("id")); existing != nil {
		return Object{"busy": false, "workspace": w, "operation": existing}
	}
	if t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", w.S("projectId")) != nil {
		return Object{"busy": true}
	}
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE workspace_id=$1 AND cleared_at IS NULL", w.S("id")) == nil, 409, "resource_in_use")
	closeAdmission(t, w, "deleted")
	actor := w.S("creatorUserId")
	// The request carries the pre-declaration Workspace row, exactly as the public delete path does
	// (`workspaceAction`): it is the snapshot `restoreAdmission` restores from when a Node refuses to
	// quiesce, and a run Workspace is deleted through this path rather than through the public API.
	// Without it a refused quiesce would leave the Workspace `deleting` with admission closed and no
	// state to return to, while the operation contract requires the refusal to restore it.
	//
	// The one field the document never carries is `issueRunId`: the request is part of the operation
	// claim response, whose Workspace document deliberately omits the run binding (see
	// `transaction.list`), so the snapshot is the row minus that column.
	snapshot := Object{}
	for k, v := range w {
		if k != "issueRunId" {
			snapshot[k] = v
		}
	}
	req := Object{"previous": Object{w.S("id"): snapshot}}
	op := newOperation(t, &PublicRequest{TenantID: w.S("tenantId"), Key: "run-workspace-" + runID + "-delete"}, actor, w.S("projectId"), w.S("id"), "delete_workspace", "quiesce", requestHash("run-workspace-delete", runID, Object{}), req)
	reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
	return Object{"busy": false, "workspace": t.one("SELECT * FROM workspaces WHERE id=$1", w.S("id")), "operation": op}
}
