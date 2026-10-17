package core

import (
	"fmt"
	"time"
)

// The WorkflowRun session machinery: the scheduler's agent-node execution path (specs decisions
// 0-cloud-side-scheduler D1 条件②/④ and 20261015-executor-slot-mappings D2/D3). Sessions
// serialize per run — dispatchRunExecution enforces one unregistered agent_session work item per
// run — so a session starts only when no node is committed running, and the pass holds until its
// terminal evidence commits. Startup is bounded by an honest deadline: a session that never
// dispatches within the deadline fails its node instead of hanging the run in `running`.
const workflowSessionStartDeadline = 15 * time.Minute

// settleWorkflowSessionEnded accepts a WorkflowRun's session terminal event. A WorkflowRun has no
// Thread, no `phase` and no Revision delivery: its scheduler's next pass reads this very execution's
// result as the node's terminal evidence (workflowAgentSessionOutcome), which is why this
// settlement commits nothing. Its whole job is to prove the event belongs to this run's own session
// work — the same identity chain the IssueRun branch walks — so an honest terminal event is acked
// and a mis-addressed one still aborts the takeover.
func settleWorkflowSessionEnded(t *transaction, runID, executionID string, ended Object) error {
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1", executionID)
	if e == nil {
		return fmt.Errorf("sessionEnded: execution %s is not registered", executionID)
	}
	if e.S("kind") != "agent_session" {
		return fmt.Errorf("sessionEnded: execution %s is kind=%q, not agent_session", executionID, e.S("kind"))
	}
	work := t.one("SELECT * FROM execution_work WHERE id=$1", e.S("workId"))
	if work == nil || work.S("workflowRunId") != runID || work.S("kind") != "agent_session" {
		return fmt.Errorf("sessionEnded: execution %s does not belong to workflow run %s agent_session work", executionID, runID)
	}
	if ended.S("outcome") != agentSessionEndedOutcome {
		return fmt.Errorf("sessionEnded: run %s execution %s carries outcome %q, not %s", runID, executionID, ended.S("outcome"), agentSessionEndedOutcome)
	}
	if !sessionEndReasons[ended.S("reason")] {
		return fmt.Errorf("sessionEnded: run %s execution %s carries end reason %q, outside the closed set", runID, executionID, ended.S("reason"))
	}
	return nil
}

// workflowRunningAgentNode returns the id of the node committed `running`, if any.
func workflowRunningAgentNode(states map[string]workflowNodeState) (string, bool) {
	for id, state := range states {
		if state.Status == "running" {
			return id, true
		}
	}
	return "", false
}

// workflowAgentSessionOutcome settles a committed running agent node. Terminal evidence (this
// node's own session execution's result) commits the node's terminal state; a
// dispatched-but-unreported session is held (the Node owns it); a session that never dispatched
// fails the node once the startup deadline passes. The run advances on the next tick after any
// commit.
//
// Every lookup is scoped to the node being settled. A run has several agent sessions over its life
// (D4 executes its agent nodes serially through one Workspace), so evidence read at run scope —
// "the newest agent_session result for this run" — would let the second node inherit the first
// node's result and be recorded succeeded without ever executing (invariant 5).
func workflowAgentSessionOutcome(t *transaction, runID string, version int64, nodeID string, states map[string]workflowNodeState) (map[string]workflowNodeState, bool) {
	e := t.one(`SELECT result FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND kind='agent_session' AND result IS NOT NULL`, runID, nodeID)
	if e != nil {
		return commitWorkflowAgentTerminal(t, runID, version, nodeID, states, e.O("result"))
	}
	// Dispatched but not yet terminal: the Node owns the session until it reports.
	if t.one(`SELECT execution_id FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND kind='agent_session'`, runID, nodeID) != nil {
		return states, false
	}
	// Never dispatched: the startup deadline applies. A node committed running with no registered
	// execution past the never dispatched fails honestly — never recorded as succeeded (invariant 5).
	state := states[nodeID]
	started, err := time.Parse(time.RFC3339, state.StartedAt)
	if err == nil && time.Since(started) > workflowSessionStartDeadline {
		state.Status = "failed"
		state.FinishedAt = nowRFC3339()
		state.Error = "session_start_deadline_exceeded"
		state.Output = Object{"reason": "session_start_deadline_exceeded"}
		states[nodeID] = state
		finishWorkflowRun(t, runID, version, "failed", "session start deadline exceeded", states)
	}
	return states, false
}

// workflowSessionEndReason maps a session execution's terminal reason onto the node/run
// vocabulary. agent_failed and interrupted fail the node; cancelled cancels it (and the run);
// idle_timeout and user_ended are natural session ends — the work completed its conversation —
// so they succeed the node (mappings decision D3: the session end reason is the evidence).
func workflowSessionEndReason(reason string) string {
	switch reason {
	case "agent_failed", "interrupted":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "succeeded"
	}
}

// commitWorkflowAgentTerminal commits a running agent node's terminal state from the session
// execution's result document. A failed or cancelled node finishes the run (the wave resolves
// downstream casualties on the next pass); a succeeded node commits and reports that the run can
// continue advancing in the same pass — the next wave executes downstream of it.
func commitWorkflowAgentTerminal(t *transaction, runID string, version int64, nodeID string, states map[string]workflowNodeState, result Object) (map[string]workflowNodeState, bool) {
	state := states[nodeID]
	state.Status = workflowSessionEndReason(result.S("reason"))
	state.FinishedAt = nowRFC3339()
	state.Output = Object{"reason": result.S("reason")}
	// Only a failed node carries an error: a cancelled node was cancelled, not broken.
	if state.Status == "failed" {
		state.Error = result.S("reason")
	}
	states[nodeID] = state
	switch state.Status {
	case "failed":
		finishWorkflowRun(t, runID, version, "failed", result.S("reason"), states)
		return states, false
	case "cancelled":
		finishWorkflowRun(t, runID, version, "cancelled", result.S("reason"), states)
		return states, false
	default:
		// The node succeeded; downstream waves continue in this same pass.
		settleWorkflowRunStates(t, runID, version, "running", "", states)
		return states, true
	}
}

// finishWorkflowRun settles the run terminal and does the terminal cleanup: unregistered
// execution work is deleted so claimWorkItem never serves a session of a finished run, and the
// run Workspace's deletion is declared (the operation machinery drains it asynchronously).
// Declaring delete on a busy Project returns busy — a later pass re-declares, so a terminal run
// is never left with a live Workspace forever.
func finishWorkflowRun(t *transaction, runID string, version int64, status, runErr string, states map[string]workflowNodeState) {
	settleWorkflowRunStates(t, runID, version, status, runErr, states)
	finishWorkflowRunCleanup(t, runID)
}

// finishWorkflowRunCleanup deletes the run's unregistered execution work and re-declares the
// run Workspace's deletion for a terminal run. Safe to call on any terminal run in any pass.
func finishWorkflowRunCleanup(t *transaction, runID string) {
	t.exec("DELETE FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL", runID)
	if w := t.one("SELECT * FROM workspaces WHERE workflow_run_id=$1", runID); w != nil {
		// Re-declaring an already-declared delete is a no-op; a busy Project leaves the
		// declaration to a later pass — a terminal run's Workspace is never left live forever.
		deleteWorkflowRunWorkspace(t, runID)
	}
}

// workflowSessionTarget resolves the Workspace's current connected Node the way currentNode does,
// but returns nil instead of refusing: the scheduler waits for admission rather than aborting.
func workflowSessionTarget(t *transaction, wid string) Object {
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	if w.S("desiredState") != "running" {
		return nil
	}
	return t.one(`SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id
		WHERE s.workspace_id=$1 AND s.generation=$2 AND s.terminated_at IS NULL AND n.ended_at IS NULL
		AND n.initialized AND n.connection_state='connected' AND n.last_seen_at>clock_timestamp()-interval '30 seconds'`, wid, w.N("runtimeGeneration"))
}

// workflowSessionSpec builds the AgentSession snapshot for a workflow node's session — the same
// wire shape sessionStartSpec produces for an IssueRun. The plugin identity is fixed (slots
// decision D4) and its version is the one the creation transaction froze (D3), never the space's
// current version: the IssueRun path pins agentPluginVersion in the run input at create and never
// re-reads it (see agent_run_settle.go), and a workflow run now does the same, so an upgrade during
// the run cannot change what it executes. The initial turn is the node's prompt rendered as one
// text block; the model binding is the frozen binding of the node's slot. A permanent gap returns
// a named reason instead of an error so the node fails honestly rather than retrying forever.
func workflowSessionSpec(t *transaction, run Object, node workflowNodeView, w Object) (spec Object, reason string) {
	runID := run.S("id")
	// A run created before the plugin version was frozen carries none, and there is no honest value to
	// substitute: the space's current version is not what this run was created against. It is reported
	// as its own reason rather than as `agent_plugin_unavailable` so that rows affected by the missing
	// freeze are countable, because what should happen to them is still an open owner decision.
	pv := run.S("pluginVersion")
	if pv == "" {
		return nil, "agent_plugin_version_unfrozen"
	}
	base := w.S("baseCommitId")
	if !commitID(base) {
		return nil, "workspace_unavailable"
	}
	checkout := t.one(`SELECT execution_id FROM clone_executions
		WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' AND result->>'commit'=$2
		ORDER BY created_at DESC, execution_id DESC LIMIT 1`, w.S("id"), base)
	if checkout == nil {
		return nil, "workspace_unavailable"
	}
	git := t.one("SELECT name,email FROM user_git_identities WHERE user_id=$1", run.S("createdBy"))
	if git == nil {
		var err error
		git, err = defaultGitIdentity(t, run.S("createdBy"))
		if err != nil {
			panic(databaseFailure{err})
		}
	}
	prompt := node.Data.O("agentConfig").S("prompt")
	if prompt == "" {
		prompt = "[no task statement]"
	}
	spec = Object{
		"kind":                "agent_session",
		"agentPluginId":       opencodeAgentCLI,
		"agentPluginVersion":  pv,
		"checkoutExecutionId": checkout.S("executionId"),
		"gitIdentity":         git,
		"initialTurn":         Object{"turnId": newID(), "content": []Object{{"text": prompt}}},
	}
	slot := node.Data.O("agentConfig").S("executorSlot")
	if slot == "" {
		slot = workflowDefaultSlot
	}
	// The run-creation gate freezes one binding per referenced slot, so a missing row here is not a
	// transient state waiting to be retried — it is a run whose freeze never happened. Starting the
	// session regardless would hand the Node a session with no model to run on, which is the silent
	// downgrade the gate exists to prevent, so the node fails under the gate's own reason.
	binding := t.one("SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name=$2", runID, slot)
	if binding == nil {
		return nil, "workflow_executor_unconfigured"
	}
	spec["modelBindingId"] = binding.S("id")
	return spec, ""
}

// startWorkflowAgentSession wires one ready agent node into real execution: it provisions (or
// waits for) the run Workspace, waits for a connected Node, builds the session spec from the
// frozen per-slot bindings, commits the node `running` under the run's version CAS, and releases
// the session work item. Anything not yet true returns without writing — the next tick retries;
// anything permanently false fails the node honestly and finishes the run.
func startWorkflowAgentSession(t *transaction, run Object, node workflowNodeView, version int64, states map[string]workflowNodeState) {
	runID := run.S("id")
	if run.S("projectId") == "" {
		failWorkflowAgentNode(t, runID, version, states, node, "workflow_run_project_required")
		return
	}
	w := t.one("SELECT * FROM workspaces WHERE workflow_run_id=$1", runID)
	if w == nil {
		lc := createWorkflowRunWorkspace(t, runID)
		if lc.B("busy") {
			return
		}
		w = lc.O("workspace")
	}
	// Provisioning waits; a failed create operation fails the node — the workspace will never
	// become ready, and the run must not hang.
	if w.S("observedState") != "ready" || !w.B("admissionOpen") {
		op := t.one(`SELECT state FROM operations WHERE workspace_id=$1 AND kind='create_workspace'
			ORDER BY created_at DESC, id DESC LIMIT 1`, w.S("id"))
		if op != nil && op.S("state") == "failed" {
			failWorkflowAgentNode(t, runID, version, states, node, "workspace_unavailable")
		}
		return
	}
	target := workflowSessionTarget(t, w.S("id"))
	if target == nil {
		return // no connected Node yet; the startup deadline eventually fails the node
	}
	spec, reason := workflowSessionSpec(t, run, node, w)
	if reason != "" {
		failWorkflowAgentNode(t, runID, version, states, node, reason)
		return
	}
	states[node.ID] = workflowNodeState{Status: "running", DisplayName: workflowNodeDisplayName(node), StartedAt: nowRFC3339()}
	settleWorkflowRunStates(t, runID, version, "running", "", states)
	enqueueExecutionWork(t, runID, "agent_session", spec, target, node.ID, time.Time{})
}

// failWorkflowAgentNode commits one agent node failed with a named reason and finishes the run.
func failWorkflowAgentNode(t *transaction, runID string, version int64, states map[string]workflowNodeState, node workflowNodeView, reason string) {
	states[node.ID] = workflowNodeState{Status: "failed", DisplayName: workflowNodeDisplayName(node), StartedAt: nowRFC3339(), FinishedAt: nowRFC3339(), Error: reason, Output: Object{"reason": reason}}
	finishWorkflowRun(t, runID, version, "failed", reason, states)
}

// workflowAnyCancelled reports whether any committed node was cancelled.
func workflowAnyCancelled(states map[string]workflowNodeState) bool {
	for _, state := range states {
		if state.Status == "cancelled" {
			return true
		}
	}
	return false
}
