package core

import (
	"context"
)

// agentDispatchBatchSize bounds the retry loop's scan so a single tick claims at most this many
// queued AgentRuns. Ordering is deterministic (created_at, id) so every tick drains the oldest first
// without starving; runs beyond the bound are claimed on the next tick.
const agentDispatchBatchSize = 100

// Every Agent claim path uses the same issue-scoped admission predicate. It bars a newer run
// behind earlier queued work, any run that still owns execution or cleanup, and a previous failure
// requiring explicit recovery. A business terminal status is insufficient: a residual Workspace
// can remain after release gave up, and a delivery failure must not silently restore older work.
const agentRunTaskClaimGuard = `
	AND NOT EXISTS (SELECT 1 FROM task_conversations paused WHERE paused.issue_id=ir.issue_id AND paused.paused_run_id IS NOT NULL)
	AND NOT EXISTS (SELECT 1 FROM task_conversation_messages m WHERE m.issue_id=ir.issue_id AND m.state='failed')
	AND NOT EXISTS (
	  SELECT 1 FROM issue_runs peer
	  WHERE peer.issue_id=ir.issue_id AND peer.id<>ir.id
	    AND peer.executor_type='agent' AND peer.deleted_at IS NULL
	    AND (
	      peer.status IN ('dispatched','running')
	      OR (peer.phase IS NOT NULL AND peer.phase<>'done')
	      OR ((peer.created_at,peer.id)<(ir.created_at,ir.id) AND (
	        peer.status IN ('queued','deferred')
	        OR (peer.status IN ('failed','cancelled') AND NOT EXISTS (
	          SELECT 1 FROM task_conversations c
	          JOIN issue_runs acknowledged ON acknowledged.id=c.acknowledged_run_id
	          WHERE c.issue_id=ir.issue_id
	            AND (peer.created_at,peer.id)<=(acknowledged.created_at,acknowledged.id)
	        ))
	        OR (peer.result->>'deliveryState' IN ('failed','skipped') AND NOT EXISTS (
	          SELECT 1 FROM task_conversations c
	          JOIN issue_runs acknowledged ON acknowledged.id=c.acknowledged_run_id
	          WHERE c.issue_id=ir.issue_id
	            AND (peer.created_at,peer.id)<=(acknowledged.created_at,acknowledged.id)
	        ))
	        OR (peer.workspace_id IS NOT NULL AND EXISTS (
	          SELECT 1 FROM workspaces w WHERE w.id=peer.workspace_id AND w.deleted_at IS NULL
	        ))
	      ))
	    )
	)`

// AgentRunDispatcher is the B-side Claim owner of a queued real Space Agent IssueRun (IssueRun D6,
// B→A). It runs one short claim transaction that re-validates the run is claimable and then hands the
// run to the control plane's own in-transaction create seam, advancing the run only when that seam
// actually created (or found) the run Workspace. It never creates session or delivery work — later
// stages own those — and it never writes execution evidence: the Workspace row, its
// create_workspace operation and the run's `workspaces.issue_run_id` binding are all the A side's.
//
// Team, workflow and dev-fixture agent runs keep the legacy ExecutionDispatcher.
type AgentRunDispatcher struct {
	store *Store
}

// isSpaceAgentRun reports whether executorID maps to a real Space Agent row in the tenant, across any
// lifecycle status. It is the discriminator between real Space Agent runs (→ AgentRunDispatcher) and
// dev/integration fixture agents (which are never space_agents rows → legacy dispatcher). Callers only
// invoke it for executor_type='agent' runs, whose executor_id passed validID at creation, so the value
// is always a well-formed uuid for the parameterized lookup.
func isSpaceAgentRun(t *transaction, tid, agentID string) bool {
	return t.one("SELECT id FROM space_agents WHERE id=$1 AND tenant_id=$2", agentID, tid) != nil
}

// Dispatch claims a single queued real Space Agent IssueRun. It is the exact claim switch the ADR
// requires:
//
//	queried contract:  executor_type='agent' AND status='queued' AND phase IS NULL, and the executor
//	                    id is a space_agents row in the tenant — otherwise no-op, stays queued.
//	control-plane claim: createRunWorkspace decides. It answers `busy` while the run's Project has
//	                    another operation in flight (IssueRun D3 accepts that a Project's runs queue
//	                    behind one operation slot), in which case the run stays queued for the next
//	                    tick. Its refusals (no Project, no default branch, no unambiguous trigger
//	                    actor) are control-plane Faults that abort this transaction, again leaving the
//	                    run queued rather than marking it failed.
//	atomicity:        the Workspace row, its create_workspace operation and the B-side
//	                    phase/status/workspace_id transition land in one transaction, so a run can
//	                    never be `provisioning` without the operation that will settle it.
//	CAS/replay-guard:  the phase/status UPDATE matches status='queued' AND phase IS NULL, so a claim
//	                    that lost the race to a concurrent dispatcher (defense-in-depth on top of the
//	                    global advisory tx lock) does not double-transition; the seam call it already
//	                    made is idempotent (keyed on run + create_workspace).
//
// It returns an error only for a real failure (which keeps the run queued); a busy or no-op is nil.
func (d *AgentRunDispatcher) Dispatch(ctx context.Context, runID string) error {
	_, err := d.store.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT ir.id FROM issue_runs ir
			WHERE ir.id = $1
			  AND ir.executor_type = 'agent'
			  AND ir.status = 'queued'
			  AND ir.phase IS NULL
			  AND ir.deleted_at IS NULL
			  AND EXISTS (
			    SELECT 1 FROM space_agents sa WHERE sa.id = ir.executor_id AND sa.tenant_id = ir.tenant_id
			  )`+agentRunTaskClaimGuard, runID)
		if o == nil {
			// Not a claimable real Space Agent run: leave it untouched, no Workspace, no transition.
			return Object{}
		}
		out := createRunWorkspace(t, runID)
		if out.B("busy") {
			return Object{}
		}
		wid := out.O("workspace").S("id")
		t.execRows(`
			UPDATE issue_runs
			SET phase = 'provisioning', status = 'dispatched', workspace_id = $2, version = version + 1, updated_at = now()
			WHERE id = $1 AND status = 'queued' AND phase IS NULL`, runID, wid)
		return Object{}
	})
	return err
}

// scanQueuedAgentRuns returns the bounded, deterministically ordered ids of claimable real Space Agent
// runs for the retry loop. It is a read-only short scan outside the advisory lock (claim correctness
// lives in each Dispatch transaction), bounded by LIMIT and ordered by (created_at, id). Eligibility
// exactly mirrors Dispatch: executor_type='agent', status='queued', phase IS NULL, and a space_agents
// row backing the executor.
func (s *Store) scanQueuedAgentRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND ir.status = 'queued'
		  AND ir.phase IS NULL
		  AND ir.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM space_agents sa WHERE sa.id = ir.executor_id AND sa.tenant_id = ir.tenant_id)`+agentRunTaskClaimGuard+`
		ORDER BY ir.created_at, ir.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DispatchQueuedAgentRunsOnce is one bounded retry-loop pass: scan the eligible queued real Space Agent
// runs (read-only) and claim each in its own short Dispatch transaction, with the sleep between ticks
// owned by the caller. Per-run failures are deliberately not surfaced here — Dispatch already keeps
// the run queued and rolls back on error, and the loop must not flood logs merely because a single run
// is transiently stuck. Only a scan-level failure cancels the pass.
func (s *Store) DispatchQueuedAgentRunsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.DispatchTaskConversationsOnce(ctx); err != nil {
		return err
	}
	ids, err := s.scanQueuedAgentRuns(ctx, agentDispatchBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.agentRunDispatcher().Dispatch(ctx, id)
	}
	return nil
}

// agentRunDispatcher returns the dispatcher for this Store, constructing a fresh per-call value. No
// Store field is needed: the struct only owns a pointer back to the Store, so a zero-value Store still
// has a nil-safe accessor and no package-global state exists.
func (s *Store) agentRunDispatcher() *AgentRunDispatcher {
	return &AgentRunDispatcher{store: s}
}
