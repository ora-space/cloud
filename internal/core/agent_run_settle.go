package core

import (
	"fmt"
)

// settleRunWorkspace is the B-owned Phase 2A settlement core behind the A→B hook
// OnRunWorkspaceSettled (controller-integration D6 runWorkspaceSettled, phase 2A).
//
// It runs on the caller-owned *transaction — the run Workspace's create_workspace
// operation terminal transaction — inside which its B transition and the deleting
// declaration must commit or roll back together. It never opens its own transaction,
// spawns a goroutine, or defers a post-commit effect.
//
// Contract (IssueRun D3/D6):
//
//	provisioning + no cancel + ready=true   → phase=starting, status stays dispatched
//	provisioning + no cancel + ready=false  → phase=releasing, status=failed, failure_reason=workspace_unavailable (+ DeleteRunWorkspace)
//	provisioning + cancel_requested_at set  → phase=releasing, status=cancelled, result.deliveryState=skipped (+ DeleteRunWorkspace)
//	phase != provisioning                   → stale / replay no-op
//
// The caller names the run and the operation's own outcome word; every business field used for the
// transition (phase, status, cancel_requested_at, workspace_id) is re-read from authoritative
// issue_runs state in this transaction, never taken from the caller. The Phase 2 state
// machine is not re-designed here; only the committed matrix above is implemented.
//
// The plugin-specific failure (agent_plugin_unavailable) is deliberately NOT produced
// here: the hook carries only a boolean ready and the create_workspace plugin step is
// not wired, so there is no authoritative evidence to distinguish it. A
// ready=false settlement always maps to the generic workspace_unavailable. Deferred
// to Phase 2B.
func (s *Store) settleRunWorkspace(t *transaction, runID, status string) error {
	if !validID(runID) {
		return fmt.Errorf("runWorkspaceSettled: invalid run id %q", runID)
	}
	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		// D8: a run Workspace may belong to a WorkflowRun. A create_workspace terminal firing
		// under a WorkflowRun reaches this hook with the WorkflowRun's id; the evidence-driven
		// scheduler reads terminal operation facts directly, so settlement writes nothing — but
		// it must ACCEPT the fact, or the terminal transaction aborts and a workflow run's
		// Workspace can never settle. An id that is neither a live issue run nor a live
		// workflow run is still an invariant violation, not a replay.
		if t.one("SELECT id FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil {
			return nil
		}
		// A create_workspace terminal firing for a run that does not exist is an invariant
		// violation, not a replay: returning an error aborts the whole transaction.
		return fmt.Errorf("runWorkspaceSettled: issue_run %s not found", runID)
	}
	if o.S("executorType") != "agent" {
		return fmt.Errorf("runWorkspaceSettled: run %s is executor_type=%q, not agent", runID, o.S("executorType"))
	}
	// The control plane states the operation's outcome as its own word rather than a boolean, so the
	// only value that means "the Workspace came up" is `ready`; anything else is the failure row.
	// `failed` is the only other value it sends today, and an unknown word is an internal
	// contradiction rather than a third outcome this matrix has a row for.
	ready := status == "ready"
	if !ready && status != "failed" {
		return fmt.Errorf("runWorkspaceSettled: run %s carries operation outcome %q, not ready or failed", runID, status)
	}

	// Replay/stale: a settlement arriving after the run already left provisioning is a
	// deterministic no-op. It must not regress phase, return a user-facing 409, create a
	// new delete intent, or enqueue execution.
	if o.S("phase") != "provisioning" {
		return nil
	}

	switch {
	case o.S("cancelRequestedAt") != "":
		// IssueRun D6: cancel during provisioning (no session yet) releases with
		// status=cancelled and deliveryState=skipped, and declares the delete.
		return s.settleCancelled(t, o)
	case ready:
		// The run's creation succeeded at the infra level, but the run is
		// only ready when its *required pinned agent plugin* (IssueRun D1/D6 snapshot key) is
		// installed at the pinned version on the run workspace. A failed or absent-at-version
		// pinned-plugin instance settles agent_plugin_unavailable (plugin-marketplace D3),
		// even though the operation terminal itself succeeded. Runs whose snapshot pins no agent
		// plugin (legacy engine / other executors) and runs with no instance row yet are treated
		// as ready, exactly as the pre-plugin-engine settle (legacy compatibility).
		if inst, req := s.agentPluginInstance(t, o); req && inst != nil &&
			(inst.S("observedState") != "installed" || inst.S("observedVersion") != o.O("input").S("agentPluginVersion")) {
			return s.settleFailed(t, o, "agent_plugin_unavailable")
		}
		// Ready settles provisioning → starting, status stays dispatched.
		if s.settleReadyGuard(t, runID) == 0 {
			return nil // stale under the serialized lock; see call site doc
		}
		s.freezeAgentStartup(t, runID)
		return nil
	default:
		// ready=false with no cancel: generic create_workspace terminal failure.
		return s.settleFailed(t, o, "workspace_unavailable")
	}
}

// agentPluginInstance resolves the run's required pinned agent plugin, taken from the
// run-create snapshot (issue_runs.input keys agentPluginId / agentPluginVersion, never re-read
// from the current roster — IssueRun D1/D6), to its workspace_plugin_instances row for the run
// workspace, in the caller's transaction. required=false means the run snapshot pins no agent
// plugin (legacy engine or a non-space-agent executor), so the caller must treat the run as
// ready rather than downgrade. The returned instance is nil when the workspace has no row for
// the pinned plugin yet (which the pre-plugin-engine settle treats as ready; the plugin step creates the
// row on the plugin step).
func (s *Store) agentPluginInstance(t *transaction, run Object) (inst Object, required bool) {
	pid := run.O("input").S("agentPluginId")
	version := run.O("input").S("agentPluginVersion")
	if pid == "" || version == "" {
		return nil, false
	}
	namespace, identifier, ok := pluginIdentity(pid)
	if !ok {
		return nil, false
	}
	wid := run.S("workspaceId")
	if !validID(wid) {
		return nil, true
	}
	row := t.one("SELECT * FROM workspace_plugin_instances WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3", wid, namespace, identifier)
	return row, true
}

// settleReadyGuard is the CAS that moves provisioning → starting. It is guarded by
// phase='provisioning' AND status='dispatched'. Returns affected rows;
// under the global advisory lock the SELECT and this CAS are serialized against other
// writers, so after re-reading provisioning an affected count of 0 can only mean the
// caller's evidence raced a legitimate concurrent settle — treated as a stale no-op.
func (s *Store) settleReadyGuard(t *transaction, runID string) int64 {
	return t.execRows(`
		UPDATE issue_runs
		SET phase='starting', version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND phase='provisioning' AND status='dispatched'`, runID)
}

// settleFailed moves a provisioning run to releasing/failed with the given reason and,
// in the same transaction, declares the run Workspace delete via the B→A seam. It is
// shared by the generic workspace failure path; the plugin-specific code is deferred to
// Phase 2B because the reason is a parameter supplied by the caller of this
// helper, not guessed here.
func (s *Store) settleFailed(t *transaction, o Object, reason string) error {
	runID := o.S("id")
	if t.execRows(`
		UPDATE issue_runs
		SET phase='releasing', status='failed', failure_reason=$2, completed_at=now(),
		    version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND phase='provisioning' AND status='dispatched'`, runID, reason) == 0 {
		return nil // stale under the serialized lock
	}
	appendActivity(t, o.S("tenantId"), o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.failed",
		runActivityDetails(runID, o.S("executorType"), o.S("executorId"), Object{"failureReason": reason}))
	return s.declareDelete(t, o)
}

// settleCancelled moves a provisioning run to releasing/cancelled with
// result.deliveryState=skipped and declares the delete, all in the caller's transaction.
func (s *Store) settleCancelled(t *transaction, o Object) error {
	runID := o.S("id")
	if t.execRows(`
		UPDATE issue_runs
		SET phase='releasing', status='cancelled', completed_at=now(),
		    result = COALESCE(result, '{}'::jsonb) || '{"deliveryState":"skipped"}'::jsonb,
		    version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND phase='provisioning' AND status='dispatched'`, runID) == 0 {
		return nil // stale under the serialized lock
	}
	appendActivity(t, o.S("tenantId"), o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.cancelled",
		runActivityDetails(runID, o.S("executorType"), o.S("executorId"), nil))
	return s.declareDelete(t, o)
}
