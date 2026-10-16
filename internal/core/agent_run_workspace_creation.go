package core

import (
	"context"
	"fmt"
	"time"
)

// The bounded-agent-startup ADR's D5: a run Workspace creation whose result stayed unknown gets a
// bounded business abandonment. The control plane parks such a creation in `blocked`; the run
// Workspace has no public API (a retry cannot reach it), a provisioning cancel waits for the
// creation to settle, and the blocked operation holds the Project's single one_project_operation
// slot, so nothing but Cloud's own scan can end the wait. This file owns that half: freezing the
// persistent deadline, and abandoning the run when it expires. The late-fact takeover entry that
// keeps receipting Controller evidence afterwards lives in agent_run_provisioning_fact.go.

// agentWorkspaceCreationBatchSize bounds one abandonment pass so a single tick claims at most this
// many runs, ordered deterministically. Runs beyond the bound are handled on the next tick.
const agentWorkspaceCreationBatchSize = 100

// creationUnfinished lists the operation states whose creation result is still unknown. A creation
// that already reached `succeeded` or `failed` settled its run in that terminal's own transaction.
const creationUnfinished = "('queued','running','retry_wait','blocked')"

// freezeWorkspaceCreationDeadline anchors the absolute abandonment deadline of one run Workspace
// creation the first time it persists `blocked`. It runs in the defer action's own transaction, so
// the anchor and the first `blocked` write commit together: the recorded start is the database time
// of that write, not a later observation (ADR D5, 上限与终态). The UPDATE is a no-op unless the run
// is still provisioning without a frozen deadline, which is what makes every later defer, manual
// retry, re-claim, restart or configuration change a non-renewal: they all leave the column set.
// The anchor deliberately fires only for run-bound create_workspace operations; a public Workspace
// creation has no business run to abandon and keeps its own retry semantics.
func (s *Store) freezeWorkspaceCreationDeadline(t *transaction, workspaceID string) {
	window := startupDuration(s.AgentWorkspaceBlockedTimeout, 30*time.Minute)
	t.exec(`UPDATE issue_runs SET workspace_blocked_at=clock_timestamp(),
	 workspace_deadline_at=clock_timestamp()+make_interval(secs=>$2),
	 workspace_blocked_source='blocked_write',version=version+1,updated_at=now()
	 WHERE workspace_id=$1 AND executor_type='agent' AND phase='provisioning' AND status='dispatched'
	   AND deleted_at IS NULL AND workspace_deadline_at IS NULL`, workspaceID, window.Seconds())
}

// observeWorkspaceCreationDeadline is the scan-side anchor. Pre-0035 blocked creations carry no
// immutable first-blocked evidence (operations.updated_at is mutable), so the migration backfills
// nothing: the first policy scan anchors them as first_observed and grants one default window,
// explicitly not claiming the original window was in force before the upgrade (ADR D5, 兼容与责任
// 范围). The same anchor also covers a `blocked` write that reached the table without passing
// through the defer path's freeze, which is why it runs before the expiry decision in the same
// transaction.
func (s *Store) observeWorkspaceCreationDeadline(t *transaction, run Object) {
	if run["workspaceDeadlineAt"] != nil {
		return
	}
	window := startupDuration(s.AgentWorkspaceBlockedTimeout, 30*time.Minute)
	t.exec(`UPDATE issue_runs SET workspace_blocked_at=clock_timestamp(),
	 workspace_deadline_at=clock_timestamp()+make_interval(secs=>$2),
	 workspace_blocked_source='first_observed',version=version+1,updated_at=now()
	 WHERE id=$1 AND workspace_deadline_at IS NULL`, run.S("id"), window.Seconds())
}

// scanBlockedWorkspaceCreations returns the bounded, deterministically ordered ids of agent runs
// whose creation deadline needs policy attention: either the deadline is due and the creation
// operation is still unfinished, or the operation is `blocked` without a frozen deadline and needs
// the first-observed anchor. A run whose creation reached a real terminal does not match: it left
// `provisioning` in the terminal's own transaction.
func (s *Store) scanBlockedWorkspaceCreations(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type='agent' AND ir.phase='provisioning' AND ir.status='dispatched'
		  AND ir.deleted_at IS NULL AND ir.workspace_id IS NOT NULL
		  AND (
		    (ir.workspace_deadline_at IS NOT NULL AND ir.workspace_deadline_at<=clock_timestamp()
		      AND EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id=ir.workspace_id
		                   AND o.kind='create_workspace' AND o.state IN `+creationUnfinished+`))
		    OR (ir.workspace_deadline_at IS NULL
		      AND EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id=ir.workspace_id
		                   AND o.kind='create_workspace' AND o.state='blocked'))
		  )
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

// GiveUpBlockedWorkspaceCreationsOnce is one bounded pass of the D5 abandonment scan: anchor or
// abandon each due run in its own short transaction, with the sleep between ticks owned by the
// caller. Per-run failures are deliberately not surfaced — the transaction already rolled back and
// keeps the run provisioning for a later tick, and the loop must not flood logs because one run is
// transiently stuck. Only a scan-level failure cancels the pass.
func (s *Store) GiveUpBlockedWorkspaceCreationsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanBlockedWorkspaceCreations(ctx, agentWorkspaceCreationBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, _ = s.transact(ctx, func(t *transaction) Object {
			s.settleBlockedWorkspaceCreation(t, id)
			return Object{}
		})
	}
	return nil
}

// settleBlockedWorkspaceCreation is the D5 abandonment core, run on the caller-owned transaction
// under the store's advisory lock. It decides for exactly one provisioning run whose creation
// deadline is due or whose creation is blocked without a deadline:
//
//	creation blocked, no frozen deadline    → anchor once as first_observed (no settlement)
//	deadline due, creation still unfinished → atomic abandonment below
//	creation reached a real terminal        → deterministic no-op: the terminal transaction owns
//	                                          the run, and re-scans confirm, never rebuild
//
// The abandonment commits every one of its writes together or not at all (ADR D5, 同一事务解除
// Project 阻塞): the independent residual ledger, the admission fence and model revocation, the
// discarded user turns, the business terminal (failed/done) and the creation operation's failure —
// whose leaving the unfinished states is what releases the Project's one_project_operation slot.
// A ledger or protection write failure therefore rolls the operation failure back with it: the slot
// is never released without residual ownership. It never fabricates a session, a Node result, a
// receipt or a deletion, never queues a delete for the residual Workspace, and never renews the
// deadline — an accepted provisioning cancel keeps its originator, time and reason and is settled
// by this same abandonment rather than being reported as a physical cancellation.
func (s *Store) settleBlockedWorkspaceCreation(t *transaction, runID string) {
	// The due-or-unanchored predicate is repeated under the lock so a scan id that went stale is
	// harmless: a run that left provisioning, or whose deadline is not yet due, does not match.
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND executor_type='agent'
	 AND phase='provisioning' AND status='dispatched' AND deleted_at IS NULL
	 AND (workspace_deadline_at IS NULL OR workspace_deadline_at<=clock_timestamp())`, runID)
	if run == nil {
		return
	}
	op := t.one(`SELECT * FROM operations WHERE workspace_id=$1 AND kind='create_workspace'
	 ORDER BY created_at DESC, id DESC LIMIT 1`, run.S("workspaceId"))
	if op == nil || op.S("state") == "succeeded" || op.S("state") == "failed" {
		// A real terminal owns this run through settleRunWorkspace in its own transaction; the
		// scan predicate raced a settled creation and this pass must not second-guess it.
		return
	}
	if run["workspaceDeadlineAt"] == nil {
		if op.S("state") == "blocked" {
			s.observeWorkspaceCreationDeadline(t, run)
		}
		return
	}
	wid := run.S("workspaceId")
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND issue_run_id=$2", wid, runID)
	if w == nil {
		// The scan only matches runs with a bound Workspace; a missing row here is an invariant
		// violation, and aborting keeps the operation unfinished rather than settling a run whose
		// residual target cannot even be named.
		panic(databaseFailure{fmt.Errorf("blocked creation settlement: run %s has no workspace row", runID)})
	}
	// A D4-style responsibility snapshot, widened to the creation's own scope: effects planned for
	// this operation without a Workspace binding yet (their id is the preallocated sandbox instance
	// id), the clone and plugin executions the creation dispatched, and the Node identities that
	// may still be alive. Nothing here is deleted or rewritten; the ledger is what the later
	// recovery ADR reconciles against.
	refs := Object{
		"executions": t.list("SELECT execution_id,node_id,operation_id,work_id FROM node_executions WHERE workspace_id=$1", wid),
		"sandboxes":  t.list("SELECT id,generation FROM sandbox_instances WHERE workspace_id=$1", wid),
		"nodes":      t.list("SELECT id,sandbox_instance_id FROM node_instances WHERE workspace_id=$1", wid),
		"operations": t.list("SELECT id FROM operations WHERE workspace_id=$1", wid),
		"effects":    t.list("SELECT id,operation_id,kind,workspace_id,state FROM external_effects WHERE workspace_id=$1 OR (operation_id=$2 AND workspace_id IS NULL)", wid, op.S("id")),
		"clones":     t.list("SELECT execution_id FROM clone_executions WHERE workspace_id=$1", wid),
	}
	t.exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason)
	 VALUES($1,$2,$3,$4,$5,$6,'workspace_unavailable') ON CONFLICT(run_id) DO NOTHING`,
		runID, run.S("tenantId"), w.S("projectId"), wid, w.N("runtimeGeneration"), jsonText(refs))
	// Close the Workspace to new execution admission and retire control eligibility without
	// pretending anything about the physical runtime: the observed state stays as it was, the
	// admission epoch bump fences old claims, and reconcileRuntimeControls keeps refusing to idle
	// a residual Workspace (its agent_run_residuals condition).
	t.exec(`UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,version=version+1 WHERE id=$1`, wid)
	t.exec(`UPDATE runtime_controls SET state='reconciling',binding_confirmed=false,input_closed=false,
	 version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1`, wid)
	t.exec("UPDATE run_model_bindings SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE run_id=$1", runID)
	t.exec(`UPDATE model_access_grants SET revoked_at=COALESCE(revoked_at,clock_timestamp())
	 WHERE binding_id IN (SELECT id FROM run_model_bindings WHERE run_id=$1)`, runID)
	t.exec("UPDATE thread_entries SET status='discarded' WHERE run_id=$1 AND source='user' AND status='queued'", runID)
	// The business terminal. `deliveryState=skipped`: nothing was delivered because nothing ever
	// started. A Thread that was never declared stays undeclared — no fabricated session end.
	abandoned := Object{"state": "abandoned", "reason": "creation_blocked_timeout", "source": "cloud", "deadlineAt": run["workspaceDeadlineAt"]}
	if t.execRows(`UPDATE issue_runs SET phase='done',status='failed',failure_reason='workspace_unavailable',
	 thread_state=CASE WHEN thread_state IS NULL THEN NULL ELSE 'ended' END,idle_since=NULL,
	 completed_at=clock_timestamp(),result=COALESCE(result,'{}'::jsonb)||jsonb_build_object('deliveryState','skipped','workspace',$2::jsonb),
	 version=version+1,updated_at=clock_timestamp()
	 WHERE id=$1 AND executor_type='agent' AND phase='provisioning' AND status='dispatched'`, runID, jsonText(abandoned)) != 1 {
		// Under the advisory lock the row was re-read in this transaction; zero rows means the
		// run legitimately left provisioning between the scan and now — the same no-op as a real
		// terminal, and the ledger rows above describe exactly the responsibility that terminal
		// already owns.
		return
	}
	// The creation operation's failure is what releases one_project_operation. The error code is
	// the ADR's provisioning_result_unknown: the original effect, clone and plugin executions keep
	// their unknown results — this is Cloud giving up waiting, never a claim they failed.
	t.exec(`UPDATE operations SET state='failed',error_code='provisioning_result_unknown',retry_at=NULL,
	 result=result||jsonb_build_object('abandonedBy','cloud','runId',$2::text,'deadlineAt',$3::text),
	 version=version+1,updated_at=clock_timestamp()
	 WHERE id=$1 AND state IN `+creationUnfinished, op.S("id"), runID, run.S("workspaceDeadlineAt"))
	appendActivity(t, run.S("tenantId"), run.S("issueId"), activityActor(run.S("executorType")), run.S("executorId"),
		"run.failed", runActivityDetails(runID, run.S("executorType"), run.S("executorId"), Object{"failureReason": "workspace_unavailable", "deliveryState": "skipped"}))
	if run.S("threadState") != "" {
		appendSessionFailure(t, run, "workspace_unavailable")
		threadChanged(t, run)
	}
}
