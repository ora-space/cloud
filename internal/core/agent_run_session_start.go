package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// agentSessionStartBatchSize bounds the Session Start retry-loop scan so a single tick releases at
// most this many first prompts. Ordering is deterministic (created_at, id) so every tick drains the
// oldest start first; work beyond the bound is released on the next tick (§19).
const agentSessionStartBatchSize = 100

// StartAgentSession is the B-owned Session Start core (IssueRun D3, D-012..D-015, G-007). For a run
// that has settled into phase='starting' (the run Workspace is provisioned and admitted), it hands the
// first prompt to the Agent exactly once: it writes the immutable first produce as the Thread's first
// entry (thread_entries seq=1, source='system', kind='user_turn'), materializes the Thread's `pending`
// state (D-4C-01, closing G-016), and — in the same transaction — releases exactly one 'agent_session'
// execution_work item through the control plane's own enqueue (D-014). It never touches
// issue_runs.phase/status (§16): the run leaves 'starting' only upon authoritative session-start/Thread
// takeover evidence (D-014).
//
// Ownership boundary: this is the B side. The control plane's enqueueExecutionWork runs inside the
// caller's transaction and commits with it, and the work item's own identity (its row, its target's
// re-validation at dispatch, its registration against a Node) stays A's. B never writes execution_work
// directly.
func (s *Store) startAgentSession(t *transaction, runID string) error {
	o := t.one(`
		SELECT ir.* FROM issue_runs ir
		WHERE ir.id = $1
		  AND ir.executor_type = 'agent'
		  AND ir.phase = 'starting'
		  AND ir.status = 'dispatched'
		  AND ir.workspace_id IS NOT NULL
		  AND ir.cancel_requested_at IS NULL
		  AND ir.deleted_at IS NULL`, runID)
	if o == nil {
		// Not a startable run: already advanced, cancelled, or not an agent run.
		return nil
	}
	wid := o.S("workspaceId")
	if !runWorkspaceLive(t, wid, runID) {
		// Fail closed (G-011): leave the run retryable, record no terminal state, release no work.
		return nil
	}
	snap := o.O("input")
	content := renderAgentInitialTurn(snap)
	turnID := newID()
	// Exactly-once marker write. 0 affected rows → a prior start already declared the first produce;
	// keep the run 'starting' and skip the enqueue.
	if t.execRows(`
		INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id)
		VALUES($1, 1, 'system', 'user_turn', $2, $3)
		ON CONFLICT (run_id, seq) DO NOTHING`,
		runID, jsonText(Object{"content": content}), turnID) == 0 {
		return nil
	}
	// Materialize the Thread lifecycle state in the same transaction (D-4C-01, G-016): the session is
	// now declared, which is exactly D4's `pending` — "session execution registered, no records yet".
	// The CAS is exact rather than tolerant: this line is only reached when the INSERT above created
	// seq=1 (a replay returns early), so under the caller's advisory lock nothing else can have moved
	// the run, and zero affected rows is an invariant violation, not a race. Rolling back is the only
	// honest outcome — a committed first prompt whose Thread state never materialized would leave the
	// read model permanently inconsistent.
	if t.execRows(`
		UPDATE issue_runs SET thread_state='pending', version=version+1, updated_at=now()
		WHERE id=$1 AND thread_state IS NULL`, runID) != 1 {
		panic(databaseFailure{fmt.Errorf("session start: run %s Thread state was not materialized", runID)})
	}
	// seq=1 is a Thread entry write like any other, so the declaration queues the same invalidation
	// hints the POST and the takeover queue (Thread D5): a subscriber watching this run learns that its
	// Thread now has a first entry. The ON CONFLICT early return above is what keeps a replayed
	// declaration from publishing — no entry was written, no hint. The hints are released only if this
	// transaction commits, so a failure below rolls them back with the entry they describe.
	threadAppended(t, o)
	threadChanged(t, o)
	// Authoritative declaration in the same transaction as seq=1 (§14). The payload fixes the frozen
	// plugin identity/version and the run's git identity from the snapshot — never re-reads the roster
	// (D-013, G-007/G-009) — and names the checkout execution the Node must run the session in, which
	// is the clone that produced the Workspace's recorded baseline (controller-integration D1: the
	// Controller and Cloud never pass a Node-local path).
	spec, err := s.sessionStartSpec(t, o, snap, turnID, content)
	if err != nil {
		return err
	}
	enqueueExecutionWork(t, runID, "agent_session", spec, sessionStartTarget(t, wid), time.Time{})
	return nil
}

// sessionStartSpec builds the fixed AgentSession snapshot the session work item carries
// (controller-integration D1's AgentSession). Everything is read from authoritative rows in the
// caller's transaction:
//
//	agentPluginId/Version: the run-create snapshot, so a later plugin install never changes what this
//	                       run executes.
//	checkoutExecutionId:   the clone execution that produced the run Workspace's recorded baseline.
//	gitIdentity:           the run input's frozen identity when it carries one, else the trigger
//	                       actor's default (identity-access D1; agent_run_identity.go names the
//	                       divergence from D2's creation-time freeze).
//	initialTurn:           the deterministic first prompt this transaction just wrote as seq=1.
//
// A run Workspace with no recorded baseline, or one whose baseline names no successful clone
// execution, is an invariant violation: a session cannot run in a Workspace that was never cloned, so
// failing closed (and rolling the seq=1 write back with it) is the only honest outcome.
func (s *Store) sessionStartSpec(t *transaction, o, snap Object, turnID, content string) (Object, error) {
	runID, wid := o.S("id"), o.S("workspaceId")
	if wid == "" {
		return nil, fmt.Errorf("session start: run %s has no run workspace", runID)
	}
	w := t.one("SELECT base_commit_id FROM workspaces WHERE id=$1 AND issue_run_id=$2 AND deleted_at IS NULL", wid, runID)
	if w == nil {
		return nil, fmt.Errorf("session start: run %s has no live run workspace %s", runID, wid)
	}
	base := w.S("baseCommitId")
	if !commitID(base) {
		return nil, fmt.Errorf("session start: run %s workspace %s has no usable base commit (%q)", runID, wid, base)
	}
	checkout := t.one(`
		SELECT execution_id FROM clone_executions
		WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' AND result->>'commit'=$2
		ORDER BY created_at DESC, execution_id DESC LIMIT 1`, wid, base)
	if checkout == nil {
		return nil, fmt.Errorf("session start: run %s workspace %s has no successful clone execution for base commit %s", runID, wid, base)
	}
	identity, err := runGitIdentity(t, o)
	if err != nil {
		return nil, err
	}
	// The wire carries the first prompt as an ordered list of text blocks; the snapshot's single
	// deterministic string is projected as exactly one block, the shape the contract defines rather
	// than a reinterpretation of it.
	spec := Object{
		"kind":                "agent_session",
		"agentPluginId":       snap.S("agentPluginId"),
		"agentPluginVersion":  snap.S("agentPluginVersion"),
		"checkoutExecutionId": checkout.S("executionId"),
		"gitIdentity":         identity,
		"initialTurn":         Object{"turnId": turnID, "content": []Object{{"text": content}}},
	}
	if bindingID := snap.S("modelBindingId"); bindingID != "" {
		spec["modelBindingId"] = bindingID
	}
	return spec, nil
}

// renderAgentInitialTurn produces the deterministic first-prompt content from the frozen run-create
// snapshot (D-013, G-007). It reads only the immutable issue_runs.input the business layer froze at run
// creation (issue_run_lifecycle.buildRunContext + agent_target.snapshotAgentRunInput), never the
// current roster. Determinism: the section order is fixed and each embedded structure is canonicalised
// with encoding/json, which sorts map keys, so two renders of the same snapshot are byte-identical —
// the replay once-guard depends on the content being stable.
func renderAgentInitialTurn(input Object) string {
	s := input.S("task")
	if s == "" {
		s = "[no task statement]"
	}
	return fmt.Sprintf(
		"Begin this task for the Space agent.\n\nTask\n%s\n\nContext\n%s",
		s,
		canonicalJSON(Object{
			"inputs":      input["interactionValues"],
			"target":      input["target"],
			"contextRefs": input["contextRefs"],
		}),
	)
}

// canonicalJSON marshals v deterministically (encoding/json sorts map keys) so identical objects
// produce identical bytes. A marshaling failure (a snapshot value no JSON can represent) returns ""
// rather than panicking a business transaction over a cosmetic renderer.
func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// runWorkspaceLive reports whether the run's Workspace is alive for a session start: it still exists,
// is bound to this exact run, and is not soft-deleted. The workspaces.issue_run_id unique already rules
// out a second run binding, so this predicate's remaining job is the soft-delete case (G-011). Callers
// fail closed when it is false — the run stays 'starting' and the retry loop resurfaces it, recording
// no terminal state (§6/§11/G-011).
func runWorkspaceLive(t *transaction, wid, runID string) bool {
	return t.one(`SELECT id FROM workspaces WHERE id=$1 AND issue_run_id=$2 AND deleted_at IS NULL`, wid, runID) != nil
}

// scanStartingAgentRuns returns the bounded, deterministically ordered ids of runs that settled into
// 'starting' but have not yet declared their first produce, for the retry loop. It is a read-only short
// scan outside the advisory lock (exactness lives in each start transaction), bounded by LIMIT and
// ordered by (created_at, id). Eligibility mirrors startAgentSession: an agent run currently
// 'starting'/'dispatched' with a live run Workspace, not cancelled, not soft-deleted, backing a real
// space_agents executor, and with no Thread entry seq=1 yet (so already-started runs are excluded from
// restart; the in-transaction once guard is still authoritative for races).
func (s *Store) scanStartingAgentRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND ir.phase = 'starting'
		  AND ir.status = 'dispatched'
		  AND ir.workspace_id IS NOT NULL
		  AND ir.cancel_requested_at IS NULL
		  AND ir.deleted_at IS NULL
		  AND EXISTS (SELECT 1 FROM space_agents sa WHERE sa.id = ir.executor_id AND sa.tenant_id = ir.tenant_id)
		  AND NOT EXISTS (
		    SELECT 1 FROM thread_entries te WHERE te.run_id = ir.id AND te.seq = 1
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

// StartQueuedAgentSessionsOnce is one bounded retry-loop pass: scan the eligible 'starting' runs
// (read-only) and start each in its own short transaction, with the sleep between ticks owned by the
// caller (D-015, G-007). Per-run failures are deliberately not surfaced here — the start transaction
// already rolls back on error and keeps the run 'starting' for a later tick, and the loop must not
// flood logs because one run is transiently stuck. Only a scan-level failure cancels the pass.
func (s *Store) StartQueuedAgentSessionsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanStartingAgentRuns(ctx, agentSessionStartBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.transact(ctx, func(t *transaction) Object {
			if err := s.startAgentSession(t, id); err != nil {
				panic(databaseFailure{err})
			}
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	return nil
}
