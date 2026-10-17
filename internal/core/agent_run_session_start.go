package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// agentSessionStartBatchSize bounds the Session Start retry-loop scan so a single tick releases at
// most this many first prompts. Ordering is deterministic (created_at, id) so every tick drains the
// oldest start first; work beyond the bound is released on the next tick.
const agentSessionStartBatchSize = 100

// StartAgentSession is the B-owned Session Start core (IssueRun D3). For a run
// that has settled into phase='starting' (the run Workspace is provisioned and admitted), it hands the
// first prompt to the Agent exactly once: it writes the immutable first produce as the Thread's first
// entry (thread_entries seq=1, source='system', kind='user_turn'), materializes the Thread's `pending`
// state, and — in the same transaction — releases exactly one 'agent_session'
// execution_work item through the control plane's own enqueue. It never touches
// issue_runs.phase/status: the run leaves 'starting' only upon authoritative session-start/Thread
// takeover evidence.
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
		// Fail closed: leave the run retryable, record no terminal state, release no work.
		return nil
	}
	snap := o.O("input")
	// The prior Revision is chosen before the first prompt is rendered: the prompt tells the Agent what
	// it resumes, and both are fixed by this one transaction (resume decision D1, D4).
	s.freezeAgentStartup(t, runID)
	resume := frozenAgentResume(t, runID)
	content := renderAgentInitialTurn(snap, resume.context)
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
	// Materialize the Thread lifecycle state in the same transaction: the session is
	// now declared, which is exactly D4's `pending` — "session execution registered, no records yet".
	// The CAS is exact rather than tolerant: this line is only reached when the INSERT above created
	// seq=1 (a replay returns early), so under the caller's advisory lock nothing else can have moved
	// the run, and zero affected rows is an invariant violation, not a race. Rolling back is the only
	// honest outcome — a committed first prompt whose Thread state never materialized would leave the
	// read model permanently inconsistent.
	var resumed any
	if resume.prior != nil {
		resumed = resume.prior.S("revisionId")
	}
	if t.execRows(`
		UPDATE issue_runs SET thread_state='pending', resume_revision_id=$2, version=version+1, updated_at=now()
		WHERE id=$1 AND thread_state IS NULL`, runID, resumed) != 1 {
		panic(databaseFailure{fmt.Errorf("session start: run %s Thread state was not materialized", runID)})
	}
	// seq=1 is a Thread entry write like any other, so the declaration queues the same invalidation
	// hints the POST and the takeover queue (Thread D5): a subscriber watching this run learns that its
	// Thread now has a first entry. The ON CONFLICT early return above is what keeps a replayed
	// declaration from publishing — no entry was written, no hint. The hints are released only if this
	// transaction commits, so a failure below rolls them back with the entry they describe.
	threadAppended(t, o)
	threadChanged(t, o)
	// Authoritative declaration in the same transaction as seq=1. The payload fixes the frozen
	// plugin identity/version and the run's git identity from the snapshot — never re-reads the roster
	// — and names the checkout execution the Node must run the session in, which
	// is the clone that produced the Workspace's recorded baseline (controller-integration D1: the
	// Controller and Cloud never pass a Node-local path).
	spec, err := s.sessionStartSpec(t, o, snap, turnID, content, resume.prior)
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
//	priorRevision:         the Revision the run resumes, when selectPriorRevision chose one and it
//	                       lives in the repository the checkout was cloned from.
//	workspaceDirectory:    the Project's Workspaces as of now, for `ora-git list`.
//
// A run Workspace with no recorded baseline, or one whose baseline names no successful clone
// execution, is an invariant violation: a session cannot run in a Workspace that was never cloned, so
// failing closed (and rolling the seq=1 write back with it) is the only honest outcome.
func (s *Store) sessionStartSpec(t *transaction, o, snap Object, turnID, content string, prior Object) (Object, error) {
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
		SELECT execution_id, input FROM clone_executions
		WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' AND result->>'commit'=$2
		ORDER BY created_at DESC, execution_id DESC LIMIT 1`, wid, base)
	if checkout == nil {
		return nil, fmt.Errorf("session start: run %s workspace %s has no successful clone execution for base commit %s", runID, wid, base)
	}
	// The prior Revision is fetched from the repository the checkout was cloned from, or not at
	// all (contract D8). Selection already required this; the frozen prior is checked again here,
	// where the two inputs are finally put side by side.
	if prior != nil && prior.S("gitRepositoryUrl") != checkout.O("input").S("repositoryUrl") {
		prior = nil
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
	if prior != nil {
		spec["priorRevision"] = prior
	}
	spec["workspaceDirectory"] = workspaceDirectory(t, wid)
	return spec, nil
}

// workspaceDirectoryLimit bounds the Workspace directory a session input carries (contract D7).
const workspaceDirectoryLimit = 100

// workspaceDirectory lists the Workspaces of the session's Project, newest first, so `ora-git list`
// can name their branches by Issue and run (contract D7). Run Workspaces stay listed after deletion
// because their branches remain on the Git Server. It is a snapshot taken when the session work is
// released, never refreshed and never used for authorization: which branches a session can read and
// write is decided by the Git Server alone.
func workspaceDirectory(t *transaction, wid string) []Object {
	rows := t.list(`
		SELECT w.id AS workspace_id,
		       CASE WHEN w.issue_run_id IS NOT NULL THEN 'run' ELSE w.kind END AS kind,
		       r.issue_id, i.title AS issue_title, r.id AS issue_run_id, r.status AS run_status
		FROM workspaces w
		JOIN workspaces self ON self.id=$1 AND self.project_id=w.project_id AND self.tenant_id=w.tenant_id
		LEFT JOIN issue_runs r ON r.id=w.issue_run_id AND r.tenant_id=w.tenant_id
		LEFT JOIN issues i ON i.id=r.issue_id AND i.tenant_id=r.tenant_id
		WHERE w.deleted_at IS NULL OR w.issue_run_id IS NOT NULL
		ORDER BY w.created_at DESC, w.id DESC
		LIMIT $2`, wid, workspaceDirectoryLimit)
	out := make([]Object, 0, len(rows))
	for _, row := range rows {
		listing := Object{"workspaceId": row.S("workspaceId"), "kind": row.S("kind")}
		if row.S("issueRunId") != "" {
			listing["issueId"], listing["issueTitle"], listing["issueRunId"], listing["runStatus"] = row.S("issueId"), row.S("issueTitle"), row.S("issueRunId"), row.S("runStatus")
		}
		out = append(out, listing)
	}
	return out
}

// renderAgentInitialTurn produces the deterministic first-prompt content from the frozen run-create
// snapshot. It reads only the immutable issue_runs.input the business layer froze at run
// creation (issue_run_lifecycle.buildRunContext + agent_target.snapshotAgentRunInput), never the
// current roster. Determinism: the section order is fixed and each embedded structure is canonicalised
// with encoding/json, which sorts map keys, so two renders of the same snapshot are byte-identical —
// the replay once-guard depends on the content being stable.
//
// `resume` is the run's resume context (resume decision D4); it joins the Context only when the Issue
// had a Revision to resume or skip, so the prompt of a run without history is unchanged.
func renderAgentInitialTurn(input, resume Object) string {
	s := input.S("task")
	if s == "" {
		s = "[no task statement]"
	}
	sections := Object{
		"inputs":      input["interactionValues"],
		"target":      input["target"],
		"contextRefs": input["contextRefs"],
	}
	if resume != nil {
		sections["resume"] = resume
	}
	return fmt.Sprintf(
		"Begin this task for the Space agent.\n\nTask\n%s\n\nContext\n%s",
		s,
		canonicalJSON(sections),
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
// out a second run binding, so this predicate's remaining job is the soft-delete case. Callers
// fail closed when it is false — the run stays 'starting' and the retry loop resurfaces it, recording
// no terminal state.
func runWorkspaceLive(t *transaction, wid, runID string) bool {
	return t.one(`SELECT id FROM workspaces WHERE id=$1 AND issue_run_id=$2 AND deleted_at IS NULL`, wid, runID) != nil
}

// scanStartingAgentRuns returns bounded, deterministically ordered starting runs needing declaration
// or reconciliation. A first prompt excludes a repeated declaration, but not withdrawal, capability
// refusal or budget expiry. Eligibility is rechecked under the same transaction lock as registration;
// this read-only scan never decides whether an external execution is absent. Roster changes cannot
// hide an already-created run's ending or recovery responsibility.
func (s *Store) scanStartingAgentRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND (ir.phase='starting' OR (ir.phase='running' AND ir.startup_ending_deadline_at IS NOT NULL))
		  AND ir.status IN ('dispatched','running')
		  AND ir.workspace_id IS NOT NULL AND ir.deleted_at IS NULL
		  AND (ir.startup_deadline_at IS NULL OR ir.startup_ending_deadline_at<=clock_timestamp()
		   OR (ir.phase='starting' AND (
		    (ir.thread_state IS NULL AND ir.cancel_requested_at IS NULL)
		    OR (COALESCE(ir.result->'startup'->>'state','')!='awaiting_node' AND ir.startup_deadline_at<=clock_timestamp())
		    OR (NOT EXISTS (SELECT 1 FROM node_executions e WHERE e.operation_id=ir.id AND e.kind='agent_session')
		      AND (ir.thread_state='ending' OR ir.cancel_requested_at IS NOT NULL
		       OR (ir.input->>'modelBindingId' IS NOT NULL AND EXISTS (
		         SELECT 1 FROM node_instances n JOIN sandbox_instances si ON si.id=n.sandbox_instance_id
		         JOIN workspaces w ON w.id=n.workspace_id AND w.runtime_generation=si.generation
		         WHERE n.workspace_id=ir.workspace_id AND n.initialized AND n.connection_state='connected'
		           AND n.ended_at IS NULL AND si.terminated_at IS NULL
		           AND n.last_seen_at>clock_timestamp()-interval '30 seconds' AND NOT n.model_proxy)))))))
		ORDER BY ir.created_at, ir.id LIMIT $1`, limit)
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

// StartQueuedAgentSessionsOnce is one bounded retry-loop pass: reconcile eligible starting runs and
// declare first prompts in separate short transactions, with the sleep between ticks owned by the
// caller. Per-run failures are deliberately not surfaced here — the transaction
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
		// Freeze an upgraded run's policy even if a later declaration cannot reach its Node.
		// Otherwise that declaration rollback would let the next deployment choose a new window.
		if _, err := s.transact(ctx, func(t *transaction) Object {
			s.freezeAgentStartup(t, id)
			return Object{}
		}); err != nil {
			continue
		}
		if _, err := s.transact(ctx, func(t *transaction) Object {
			if s.reconcileAgentStartup(t, id) {
				return Object{}
			}
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
