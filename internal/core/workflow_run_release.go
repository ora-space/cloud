package core

import (
	"context"
	"fmt"
	"time"
)

// The WorkflowRun Workspace release (circus#75 S5; specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md D3/D7). A run's terminal status says how the
// workflow ended; it says nothing about the isolated Workspace Cloud provisioned for it. These are
// two independent facts, and keeping them apart is the whole point of this file: a run that finished
// is not a Workspace that was released.
//
// The durable state machine (0055) is forward-only:
//
//	pending    -> releasing      terminal cleanup declared the Workspace delete
//	pending    -> not_required   the run never provisioned a Workspace
//	releasing  -> released       the delete_workspace operation succeeded, in that same transaction
//	releasing  -> abandoned      the Workspace's Node stayed unreachable past the give-up window
//
// Only `released` claims the Workspace is gone. `abandoned` keeps the Workspace row, carries the
// reason and leaves `workspace_released_at` NULL — the same "never report a release Cloud did not
// observe" rule IssueRun D8 applies, for the run kind that has no phase column to carry it. No
// transition leaves a terminal release state, so a replay of a settled delete is a no-op rather than
// a second fact.
//
// Every writer runs inside the transaction that owns the evidence it reacts to:
//
//   - terminal settlement declares the delete and writes `releasing`, so a terminal run is never
//     without a durable release intent;
//   - the delete-completion hook writes `released` in the transaction that writes the operation
//     `succeeded` and soft-deletes the Workspace, so the two cannot disagree;
//   - the bounded give-up pass writes `abandoned` in the transaction that fails the unfinished
//     operation, so a stuck delete cannot keep a run in `releasing` forever.

const (
	workflowReleasePending     = "pending"
	workflowReleaseReleasing   = "releasing"
	workflowReleaseReleased    = "released"
	workflowReleaseAbandoned   = "abandoned"
	workflowReleaseNotRequired = "not_required"
)

// workflowReleaseAbandonedReason is the run-level reason a give-up records: Cloud could not reach the
// Workspace's Node, so the release did not happen. It is the code IssueRun D8 already uses for the
// same fact, kept identical so a client reading either run kind reads the same word.
const workflowReleaseAbandonedReason = "workspace_unavailable"

// declareWorkflowRunWorkspaceRelease records the release intent of a terminal run and declares the
// Workspace delete. It is the only writer of `not_required` and the only writer of the first
// `releasing`; every later call for the same run is a no-op on the state and at most re-declares an
// absent delete operation.
//
// A run with no Workspace row never needed one (its graph had no agent node, or it was cancelled
// before provisioning), and `not_required` says exactly that instead of leaving the release
// ambiguous. A run whose Workspace row is already soft-deleted had it deleted by another path (a
// project deletion deletes every Workspace inline and never fires the per-run delete hook), so its
// own deletion instant is the release fact — the same value the completion hook would have written.
//
// Declaring the delete can legitimately do nothing: a busy Project refuses the declaration, and the
// reconciliation pass re-declares it later. The state stays `releasing` either way, because the
// intent is already durable.
func declareWorkflowRunWorkspaceRelease(t *transaction, runID string) {
	run := t.one("SELECT status, workspace_release_state FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if run == nil {
		return
	}
	switch run.S("status") {
	case "succeeded", "failed", "cancelled":
	default:
		// A release starts only when the run is terminal. A running run whose Workspace was deleted
		// is an operator intervention, not this scheduler's fact to invent.
		return
	}
	switch run.S("workspaceReleaseState") {
	case workflowReleaseReleased, workflowReleaseAbandoned, workflowReleaseNotRequired:
		return
	}
	w := t.one("SELECT id, deleted_at FROM workspaces WHERE workflow_run_id=$1", runID)
	if w == nil {
		t.exec(`UPDATE workflow_runs
			SET workspace_release_state=$2, workspace_release_error='', version=version+1, updated_at=now()
			WHERE id=$1 AND workspace_release_state='pending'`, runID, workflowReleaseNotRequired)
		return
	}
	if w["deletedAt"] != nil {
		t.exec(`UPDATE workflow_runs
			SET workspace_release_state=$2, workspace_release_error='', workspace_released_at=COALESCE($3::timestamptz, now()),
			    version=version+1, updated_at=now()
			WHERE id=$1 AND workspace_release_state IN ('pending','releasing')`,
			runID, workflowReleaseReleased, w["deletedAt"])
		return
	}
	t.exec(`UPDATE workflow_runs
		SET workspace_release_state=$2, workspace_release_error='', version=version+1, updated_at=now()
		WHERE id=$1 AND workspace_release_state='pending'`, runID, workflowReleaseReleasing)
	// deleteWorkflowRunWorkspace is idempotent: an existing live delete operation is returned, a busy
	// Project reports busy, and an absent one is declared. Its result decides nothing about the release
	// state — the intent is already committed with this transaction.
	_ = deleteWorkflowRunWorkspace(t, runID)
}

// settleWorkflowRunWorkspaceDeleted is the WorkflowRun half of the delete-completion hook
// (settleRunWorkspaceDeleted): it runs in the transaction that writes the run Workspace's
// delete_workspace operation `succeeded`, and records the one fact that operation proves.
//
// `released` is written from `pending`, `releasing` or `not_required`. The last one would contradict
// the operation's own existence (a Workspace row bound to this run means something needed releasing),
// but the delete's success is the stronger evidence and the row is already being deleted in this
// transaction, so it is corrected rather than refused.
//
// A run already `released` is a replay and writes nothing. A run `abandoned` writes nothing either:
// the give-up failed the very operation this hook would settle, so reaching here means a late or
// duplicated completion for an operation Cloud already gave up on, and D8's rule is that a give-up is
// never turned back into a reported release. Any other state is a contradiction and aborts the
// caller's transaction rather than silently accepting a delete the run's lifecycle never declared.
func settleWorkflowRunWorkspaceDeleted(t *transaction, runID string) error {
	run := t.one("SELECT status, workspace_release_state FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if run == nil {
		return fmt.Errorf("runWorkspaceDeleted: workflow run %s not found", runID)
	}
	switch run.S("status") {
	case "succeeded", "failed", "cancelled":
	default:
		return fmt.Errorf("runWorkspaceDeleted: workflow run %s is status=%q, not terminal", runID, run.S("status"))
	}
	switch run.S("workspaceReleaseState") {
	case workflowReleaseReleased, workflowReleaseAbandoned:
		return nil
	case workflowReleasePending, workflowReleaseReleasing, workflowReleaseNotRequired:
		if t.execRows(`UPDATE workflow_runs
			SET workspace_release_state=$2, workspace_release_error='', workspace_released_at=now(),
			    version=version+1, updated_at=now()
			WHERE id=$1 AND workspace_release_state IN ('pending','releasing','not_required')`,
			runID, workflowReleaseReleased) != 1 {
			return fmt.Errorf("runWorkspaceDeleted: workflow run %s was not moved to released", runID)
		}
		return nil
	default:
		return fmt.Errorf("runWorkspaceDeleted: workflow run %s carries release state %q, outside the closed set", runID, run.S("workspaceReleaseState"))
	}
}

// abandonWorkflowRunWorkspaceRelease is D8's terminal transition for a WorkflowRun: an unreachable
// run Workspace stops being retried, and the run records that Cloud gave up without ever claiming the
// Workspace was released.
//
// The writes are the same three facts as the IssueRun give-up, and nothing else:
//
//   - the unfinished delete_workspace operation becomes terminally failed. Zero rows is a legal
//     outcome: the run can be `releasing` with no operation (a busy Project that has not accepted the
//     declaration yet), and the condition is about the Workspace, not about an operation existing.
//   - `releasing -> abandoned` (or `pending -> abandoned`) as one CAS on the state the caller read.
//     Zero affected rows is an invariant contradiction and rolls the operation failure back with it.
//   - `workspace_release_error = workspace_unavailable`, and `workspace_released_at` stays NULL.
//
// Deliberately not here: anything about `workspaces`. The Workspace is still there, and clearing the
// binding, soft-deleting the row or writing a success-shaped operation result is the lie D8 forbids.
// The row is the record of a residual resource and must stay readable.
func abandonWorkflowRunWorkspaceRelease(t *transaction, runID, wid string) error {
	// `node_unavailable` is the published operation failure code for an unreachable Node, the same code
	// the IssueRun give-up and the session settlement use. The operation keeps its step and request:
	// they are the record of how far the delete got.
	t.exec(`UPDATE operations SET state='failed', error_code='node_unavailable', version=version+1, updated_at=now()
		WHERE workspace_id=$1 AND kind='delete_workspace' AND state IN ('queued','running','retry_wait','blocked')`, wid)
	if t.execRows(`UPDATE workflow_runs
		SET workspace_release_state=$2, workspace_release_error=$3, version=version+1, updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL AND status IN ('succeeded','failed','cancelled')
		  AND workspace_release_state IN ('pending','releasing')`,
		runID, workflowReleaseAbandoned, workflowReleaseAbandonedReason) != 1 {
		run := t.one("SELECT status, workspace_release_state FROM workflow_runs WHERE id=$1", runID)
		return fmt.Errorf("abandon workflow run release: run %s was not moved to abandoned (status=%q release=%q)",
			runID, run.S("status"), run.S("workspaceReleaseState"))
	}
	return nil
}

// giveUpWorkflowRunWorkspaceReleases is one bounded pass of D8's give-up policy for WorkflowRuns: it
// finds terminal runs still holding a live Workspace whose release has not settled, and abandons the
// ones whose Node state has been unknown past the configured window. Each decision runs in its own
// short transaction; a per-run failure rolls that transaction back and the next tick retries it, so
// one transiently stuck run never cancels the pass.
//
// The condition is runWorkspaceReleaseGivenUp — the same live-sandbox-plus-heartbeat judgement the
// IssueRun release paths use, reusing the shared runWorkspaceNodeUnknown definition of "the Node is
// gone". It is never taken from the delete operation's state: an operation that failed repeatedly, or
// one that never existed, is not evidence about reachability, and a refusal by a reachable Node is
// not a reason to give up. `pending` is included because a terminal run can wait a long time for a
// busy Project to accept its delete declaration; if the Node is already gone, waiting for the Project
// changes nothing.
//
// An unconfigured window (zero or negative) is a no-op, not an immediate give-up: runWorkspaceReleaseGivenUp
// fails closed, and a deployment that never set the limit loses nothing it was promised. This is the
// same fail-closed rule and the same `DeliveryUnreachableAfter` window as IssueRun D5/D8 — there is no
// second limit to tune.
func (s *Store) giveUpWorkflowRunWorkspaceReleases(ctx context.Context) error {
	if s.DeliveryUnreachableAfter <= 0 {
		return nil
	}
	type releaseCandidate struct{ runID, workspaceID string }
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT r.id, w.id FROM workflow_runs r
		JOIN workspaces w ON w.workflow_run_id=r.id
		WHERE r.status IN ('succeeded','failed','cancelled') AND r.deleted_at IS NULL
		  AND r.workspace_release_state IN ('pending','releasing')
		  AND w.deleted_at IS NULL
		ORDER BY r.updated_at, r.id
		LIMIT $1`, workflowAdvanceBatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	var candidates []releaseCandidate
	for rows.Next() {
		var c releaseCandidate
		if err := rows.Scan(&c.runID, &c.workspaceID); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range candidates {
		if _, _, err := s.transactWorkflowRun(ctx, c.runID, func(t *transaction) Object {
			// Re-read the predicate inside the transaction that writes: a run that settled its release
			// between the scan and this pass is a deterministic no-op, never a wrongful abandonment.
			if t.one(`
				SELECT r.id FROM workflow_runs r
				JOIN workspaces w ON w.workflow_run_id=r.id
				WHERE r.id=$1 AND w.id=$2 AND r.status IN ('succeeded','failed','cancelled')
				  AND r.deleted_at IS NULL AND r.workspace_release_state IN ('pending','releasing')
				  AND w.deleted_at IS NULL`, c.runID, c.workspaceID) == nil {
				return Object{}
			}
			if !runWorkspaceReleaseGivenUp(t, c.runID, c.workspaceID, s.DeliveryUnreachableAfter) {
				return Object{}
			}
			if err := abandonWorkflowRunWorkspaceRelease(t, c.runID, c.workspaceID); err != nil {
				panic(databaseFailure{err})
			}
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	return nil
}

// reconcileWorkflowRunWorkspaceReleases finishes the release fact of every terminal run the scheduler
// itself no longer scans, in two steps.
//
// The first heals runs whose Workspace was already deleted by a path that never fires the per-run
// delete hook — a project deletion deletes every Workspace of the project inline. The Workspace row's
// own deletion instant is the release fact, so the state is corrected to `released` rather than left
// `releasing` beside a Workspace that no longer exists.
//
// The second re-declares the delete of every terminal run still holding a live Workspace and no
// unfinished delete operation. Terminal settlement declares it in the settling transaction; a busy
// Project refuses that declaration, so this scan is the retry. Idempotent:
// deleteWorkflowRunWorkspace treats an already-declared or already-deleted Workspace as done.
//
// Runs whose release already reached a terminal state are out of scope in both steps. In particular
// an `abandoned` run is never re-entered: D8's residual Workspace is intentionally left with no
// operation and no retry.
func (s *Store) reconcileWorkflowRunWorkspaceReleases(ctx context.Context) error {
	// The heal runs per run, not as one set-based UPDATE: every WorkflowRun writer takes that run's
	// exclusive transaction right (transactWorkflowRun), and a statement outside any transaction
	// would be the one writer that bypasses it. The scan only proposes; the transaction re-reads the
	// predicate before writing, so a run that settled meanwhile is a no-op.
	healIDs, err := s.scanWorkflowRunReleasesNeedingHeal(ctx)
	if err != nil {
		return err
	}
	for _, id := range healIDs {
		if _, _, err := s.transactWorkflowRun(ctx, id, func(t *transaction) Object {
			t.exec(`
				UPDATE workflow_runs r
				SET workspace_release_state='released', workspace_release_error='',
				    workspace_released_at=w.deleted_at, version=r.version+1, updated_at=now()
				FROM workspaces w
				WHERE r.id=$1 AND w.workflow_run_id=r.id AND w.deleted_at IS NOT NULL
				  AND r.status IN ('succeeded','failed','cancelled') AND r.deleted_at IS NULL
				  AND r.workspace_release_state IN ('pending','releasing')`, id)
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	ids, err := s.scanWorkflowRunReleasesNeedingDeclaration(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err := s.transactWorkflowRun(ctx, id, func(t *transaction) Object {
			finishWorkflowRunCleanup(t, id)
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	return nil
}

// scanWorkflowRunReleasesNeedingHeal returns the bounded, deterministically ordered ids of terminal
// runs whose Workspace was already soft-deleted while the run still reads `pending` or `releasing`.
func (s *Store) scanWorkflowRunReleasesNeedingHeal(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT r.id FROM workflow_runs r
		JOIN workspaces w ON w.workflow_run_id=r.id
		WHERE w.deleted_at IS NOT NULL
		  AND r.status IN ('succeeded','failed','cancelled') AND r.deleted_at IS NULL
		  AND r.workspace_release_state IN ('pending','releasing')
		ORDER BY r.created_at, r.id LIMIT $1`, workflowAdvanceBatchSize)
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

// scanWorkflowRunReleasesNeedingDeclaration returns the bounded, deterministically ordered ids of
// terminal runs still holding a live Workspace and no unfinished delete operation: the retry of a
// release declaration a busy Project refused when the run settled.
func (s *Store) scanWorkflowRunReleasesNeedingDeclaration(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT r.id FROM workflow_runs r
		JOIN workspaces w ON w.workflow_run_id=r.id
		WHERE r.status IN ('failed','cancelled','succeeded') AND r.deleted_at IS NULL
		  AND r.workspace_release_state IN ('pending','releasing')
		  AND w.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id=w.id AND o.kind='delete_workspace'
		   AND o.state IN ('queued','running','retry_wait','blocked','succeeded'))
		ORDER BY r.created_at, r.id LIMIT $1`, workflowAdvanceBatchSize)
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

// runWorkspaceReleaseGivenUp is D8's single give-up condition, shared by both run kinds: the run
// Workspace still holds a sandbox, and that sandbox's Node state has been unknown for longer than the
// deployment's unreachability window.
//
// The live-sandbox requirement is what makes the shared heartbeat judgement the right one here. A
// Workspace usually has a sandbox for the whole release, because the delete cannot confirm quiescence
// without a Node; once that sandbox is terminated the remaining cleanup runs against the Substrate
// with no Node involved, and abandoning it would report a release as failed while a live Controller
// was still completing it. So a silent Node only counts while the sandbox it belongs to is still
// there.
//
// The window is judged on the database clock through runWorkspaceNodeUnknown, and an unconfigured
// window fails closed. An explicit quiesce refusal does not reach here at all: refusing leaves the
// Node connected and freshly seen, so the heartbeat is present and the condition is false — "a failed
// delete is not a reason to give up", not an extra rule.
func runWorkspaceReleaseGivenUp(t *transaction, runID, wid string, window time.Duration) bool {
	if !validID(wid) {
		return false
	}
	if t.one(`SELECT 1 FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL`, wid) == nil {
		return false
	}
	return runWorkspaceNodeUnknown(t, runID, wid, window)
}
