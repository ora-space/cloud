package core

import (
	"fmt"
	"time"
)

// The WorkflowRun session machinery: the scheduler's agent-node execution path (specs decisions
// 0-cloud-side-scheduler D1 条件②/④ and 20261015-executor-slot-mappings D2/D3). Sessions
// serialize per run — dispatchRunExecution enforces one unregistered agent_session work item per
// run — so a session starts only when no node is committed running, and the pass holds until its
// terminal evidence commits. Two honest deadlines bound the run instead of leaving it `running`
// forever: a session that never dispatches within the startup deadline fails its node, and a
// dispatched session that produces neither a result nor a successful turn end within the configured
// turn window fails with Cloud's own `session_turn_deadline_exceeded` (circus#299).
const workflowSessionStartDeadline = 15 * time.Minute

// workflowSessionNodeUnavailable is the reason an in-flight session settles with when the Node that
// owns it is gone. It is Cloud's own code for the diagnosis Cloud made, and deliberately not one of the
// Node-reported session end reasons (`interrupted`, `agent_failed`): nothing reported anything here,
// and borrowing the Node's vocabulary would make Cloud's judgement indistinguishable from a Node's
// report. It is also the published operation failure code for an unreachable Node — the same fact, so
// the same name.
const workflowSessionNodeUnavailable = "node_unavailable"

// workflowSessionTurnDeadlineExceeded is the reason a dispatched session settles with when it has
// produced neither a result nor a successful `turnEnded` within the configured turn window
// (circus#299 option A). Like node_unavailable it is Cloud's own diagnosis, not a Node-reported end
// reason, so it is kept outside the Node's vocabulary. It is also the published operation failure
// code for the same fact.
const workflowSessionTurnDeadlineExceeded = "session_turn_deadline_exceeded"

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
// dispatched-but-unreported session is held while the Node can still report it, but only within the
// configured turn window; a session whose Node is gone settles failed, and a session that never
// dispatched fails the node once the startup deadline passes. The run advances on the next tick
// after any commit.
//
// Every lookup is scoped to the node being settled and to its current attempt. A run has several
// agent sessions over its life (D4 executes its agent nodes serially through one Workspace), so
// evidence read at run scope — "the newest agent_session result for this run" — would let the second
// node inherit the first node's result and be recorded succeeded without ever executing (invariant
// 5). The same argument applies inside one node once it retries: attempt 1's committed result stays
// on its own execution row, and a lookup that does not name the current attempt would settle the
// in-flight attempt 2 with it (retry decision D4). The attempt comes from the committed state, so a
// restart, a replica change and a replay all scope to the same row.
func workflowAgentSessionOutcome(t *transaction, runID string, version int64, node workflowNodeView, states map[string]workflowNodeState, unreachableAfter time.Duration) (map[string]workflowNodeState, bool) {
	nodeID, nodeState := node.ID, states[node.ID]
	attempt := nodeState.attemptNumber()
	e := t.one(`SELECT result FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='agent_session' AND result IS NOT NULL`, runID, nodeID, attempt)
	if e != nil {
		return commitWorkflowAgentTerminal(t, runID, version, node, states, e.O("result"))
	}
	// Dispatched but not yet terminal: the Node owns the session until it reports — and a session may
	// legitimately run for hours, so elapsed time is not evidence of anything. The Node is. When the
	// session's Node can no longer report, holding the run `running` waits forever for evidence that
	// cannot arrive, so the node settles failed with Cloud's own reason (invariant 5's converse: no
	// committed result, so never succeeded). The judgement and the window are the ones the IssueRun
	// release paths already make (runWorkspaceNodeUnknown), so "the Node is unreachable" has one
	// definition across run kinds, and the state a force stop leaves behind — sandbox terminated, the
	// executions it hosted marked — settles here instead of hanging.
	//
	// The polarity stays the shared judgement's: a run with no Workspace bound to it is not "unknown"
	// and the session is held, exactly as D5 and D8 refuse to read a lookup problem as an abandoned
	// run. The session path cannot reach that state anyway — startWorkflowAgentSession creates the
	// Workspace before it commits the node `running` — and the binding is never cleared, only
	// soft-deleted, which the live-sandbox half of the judgement already covers.
	if e := t.one(`SELECT execution_id FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='agent_session'`, runID, nodeID, attempt); e != nil {
		if w := t.one("SELECT id FROM workspaces WHERE workflow_run_id=$1", runID); w != nil &&
			runWorkspaceNodeUnknown(t, runID, w.S("id"), unreachableAfter) {
			state := states[nodeID]
			state.Status = "failed"
			state.FinishedAt = nowRFC3339()
			state.Error = workflowSessionNodeUnavailable
			state.Output = Object{"reason": workflowSessionNodeUnavailable}
			states[nodeID] = state
			finishWorkflowRun(t, runID, version, "failed", workflowSessionNodeUnavailable, states)
			return states, false
		}
		// The Node is still there. A successful initial turn ends the session — Cloud releases one
		// EndSession command and waits for the terminal result, which stays the node's only evidence
		// (D4); the confirmation window bounds that wait (D7). A session with no successful turn is
		// held, but not forever: once it has been dispatched longer than the configured turn window
		// without either a result or a successful turn end, Cloud's own deadline settles it
		// (circus#299 option A). The turn-ended check comes first so a session that did finish its
		// turn keeps the end-confirmation path and is never reported as over-window.
		executionID := e.S("executionId")
		turnEnded := workflowSessionTurnEnded(t, executionID)
		if !turnEnded && workflowSessionTurnExpired(t, executionID, startupDuration(t.store.WorkflowSessionTurnTimeout, 2*time.Hour)) {
			state := states[nodeID]
			state.Status = "failed"
			state.FinishedAt = nowRFC3339()
			state.Error = workflowSessionTurnDeadlineExceeded
			state.Output = Object{"reason": workflowSessionTurnDeadlineExceeded}
			states[nodeID] = state
			finishWorkflowRun(t, runID, version, "failed", workflowSessionTurnDeadlineExceeded, states)
			return states, false
		}
		return workflowSessionEndRequest(t, runID, version, nodeID, attempt, executionID, turnEnded, states)
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

// workflowSessionEndRequest drives one dispatched session toward its terminal result. A WorkflowRun
// session is single-turn: the initial turn's `turnEnded` receipt is the node's completion signal, so
// the scheduler releases one EndSession command with `idle_timeout` through the existing Controller
// relay, and the Node ends the session and reports its result (D4). The trigger is this recompute
// rather than the takeover hook so a crash between ticks re-reads the receipt and re-releases the
// same command (the per-node uniqueness in 0049 makes that a replay, not a second request).
//
// The wait is bounded (D7). Once the command has been outstanding longer than the confirmation
// window, the node fails with a named reason and the run is finished, which declares the Workspace
// deletion. turnEnded is the caller's read of the session's latest receipt; passing it in keeps one
// pass from reading the same receipt twice and disagreeing with itself. Without this a Node that
// never claims the command would leave the run `running` forever — the same permanent stall this
// path exists to remove. The deadline is measured from the command's own `created_at`, so retries
// and replicas all read the same window.
func workflowSessionEndRequest(t *transaction, runID string, version int64, nodeID string, attempt int64, executionID string, turnEnded bool, states map[string]workflowNodeState) (map[string]workflowNodeState, bool) {
	// The end request is one per (run, node, attempt): 0069 re-keys 0049's uniqueness on the attempt,
	// so attempt 1's outstanding request is neither reused nor delivered to attempt 2's session.
	cmd := t.one("SELECT * FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='end_session'", runID, nodeID, attempt)
	if cmd == nil {
		if !turnEnded {
			return states, false
		}
		cmd = enqueueWorkflowThreadCommand(t, runID, nodeID, attempt, "end_session", Object{"reason": "idle_timeout"})
	}
	window := startupDuration(t.store.AgentEndConfirmTimeout, 5*time.Minute)
	// The comparison runs on the database clock, the same clock `created_at` came from: a process
	// clock skew must not be able to shorten or extend the confirmation window.
	if t.one(`SELECT id FROM thread_commands WHERE id=$1
		AND created_at<=clock_timestamp()-make_interval(secs=>$2)`, cmd.S("id"), window.Seconds()) == nil {
		return states, false
	}
	state := states[nodeID]
	state.Status = "failed"
	state.FinishedAt = nowRFC3339()
	state.Error = "session_end_deadline_exceeded"
	state.Output = Object{"reason": "session_end_deadline_exceeded"}
	states[nodeID] = state
	finishWorkflowRun(t, runID, version, "failed", "session end deadline exceeded", states)
	return states, false
}

// workflowSessionTurnEnded reports whether the session execution's most recent receipt is a
// successful `turnEnded` — the same idle evidence the Thread lifecycle reads (Thread D4). It reads
// the latest receipt rather than scanning for any turn end: a turn the agent followed with another
// record is not the end of the conversation, and a `cancelled` stop reason is not idle evidence
// either (a cancellation is settled by the authoritative session end). The type tag is the closed
// record discriminator Cloud already validates on takeover; Cloud still does not interpret the
// record's business fields.
func workflowSessionTurnEnded(t *transaction, executionID string) bool {
	last := t.one(`SELECT (event::jsonb->>'record')::jsonb AS record
		FROM node_event_receipts WHERE execution_id=$1 ORDER BY sequence DESC LIMIT 1`, executionID)
	if last == nil {
		return false
	}
	record := last.O("record")
	// `stop_reason` is the field as `ora-history` serializes it: serde's container-level
	// `rename_all = "camelCase"` renames the enum variants (`turnEnded`), not the variant fields,
	// which stay snake_case. Reading the camelCase spelling silently yields "" and would count a
	// cancelled turn as idle evidence.
	return record.S("type") == "turnEnded" && record.S("stop_reason") != "cancelled"
}

// workflowSessionTurnExpired reports whether a dispatched session execution has been registered
// longer than the configured turn window without committing a result (circus#299 option A). The
// instant is node_executions.created_at, judged on the database clock: the window belongs to the
// execution row, so a retry's new row starts a fresh window, while a scheduler replay, a replica
// change, a Controller restart and an operator retrying a pass all read the same immutable instant
// and cannot reset or shorten it. The result predicate is repeated rather than inferred from the
// caller's earlier read so the deadline can never settle an execution that has terminal evidence:
// a committed result wins (invariant 5's converse).
func workflowSessionTurnExpired(t *transaction, executionID string, window time.Duration) bool {
	return t.one(`SELECT execution_id FROM node_executions
		WHERE execution_id=$1 AND result IS NULL
		  AND created_at<=clock_timestamp()-make_interval(secs=>$2)`, executionID, window.Seconds()) != nil
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

// commitWorkflowAgentTerminal commits a running agent node's state from the session execution's
// result document. A retryable failure becomes a committed `retrying` state and the pass stops — the
// run stays running and the attempt is retried after its committed wait. Otherwise a failed or
// cancelled node finishes the run (the wave resolves downstream casualties on the next pass), and a
// succeeded node commits and reports that the run can continue advancing in the same pass — the next
// wave executes downstream of it.
//
// The retry decision is made from result.reason before the reason is mapped (D2). The mapping sends
// several unrelated reasons to `failed` — interrupted, Cloud's own deadlines, node_unavailable —
// and treats every unknown reason as success; deriving the retry from the mapped status would both
// retry what the ADR forbids and skip the decision for reasons that map to success. The decision
// reads the session's reason and not its detail: a pinned-plugin failure is agent_failed with
// detail=agent_plugin_unavailable, so D2's closed set keeps it retryable; the lifted public reason
// is applied only when the node settles.
func commitWorkflowAgentTerminal(t *transaction, runID string, version int64, node workflowNodeView, states map[string]workflowNodeState, result Object) (map[string]workflowNodeState, bool) {
	nodeID := node.ID
	state := states[nodeID]
	state.Attempt = state.attemptNumber()
	states[nodeID] = state
	if wait, retry := workflowRetryDecision(workflowRetryPolicyOf(node.Data), state.attemptNumber(), result.S("reason")); retry {
		state.Status = "retrying"
		state.RetryAt = workflowRetryAt(t, wait)
		// The attempt that failed keeps its start instant and its reason; it has no finish instant,
		// because the run has not finished with this node — it is waiting. error/output are the failed
		// attempt's evidence and stay readable while it waits (D3).
		state.FinishedAt = ""
		state.Error = result.S("reason")
		state.Output = Object{"reason": result.S("reason")}
		states[nodeID] = state
		// A waiting node is not terminal: the run keeps status running with an empty error, retains
		// its Workspace and releases no work. The state and the run row commit together under the
		// existing version CAS, so every later pass wakes against exactly this retryAt.
		settleWorkflowRunStates(t, runID, version, "running", "", states)
		return states, false
	}
	applyWorkflowAgentTerminal(states, nodeID, result)
	reason := workflowAgentTerminalReason(result)
	switch states[nodeID].Status {
	case "failed":
		finishWorkflowRun(t, runID, version, "failed", reason, states)
		return states, false
	case "cancelled":
		finishWorkflowRun(t, runID, version, "cancelled", reason, states)
		return states, false
	default:
		// The node succeeded; downstream waves continue in this same pass.
		return states, settleWorkflowRunStates(t, runID, version, "running", "", states)
	}
}

// finishWorkflowRun settles the run terminal and does the terminal cleanup: unregistered
// execution work is deleted so claimWorkItem never serves a session of a finished run, and the
// run Workspace's deletion is declared (the operation machinery drains it asynchronously).
// Declaring delete on a busy Project returns busy — a later pass re-declares, so a terminal run
// is never left with a live Workspace forever.
func finishWorkflowRun(t *transaction, runID string, version int64, status, runErr string, states map[string]workflowNodeState) {
	if settleWorkflowRunStates(t, runID, version, status, runErr, states) {
		finishWorkflowRunCleanup(t, runID)
	}
}

// finishWorkflowRunCleanup deletes the run's unregistered execution work and records the run
// Workspace's release: `not_required` when the run never provisioned one, otherwise the `releasing`
// intent together with the delete declaration (declareWorkflowRunWorkspaceRelease). The release fact
// and the declaration commit in the transaction that settles the run, so a terminal run is never
// without either. Safe to call on any terminal run in any pass.
func finishWorkflowRunCleanup(t *transaction, runID string) {
	if prepareWorkflowRunDelivery(t, runID) {
		declareWorkflowRunWorkspaceRelease(t, runID)
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
	spec["workspaceDirectory"] = workspaceDirectory(t, w.S("id"))
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
	// A ready Workspace is not yet evidence that this run's pinned plugin is usable: the plugin step
	// opens admission even when an item install failed, and it installs the space's current selection
	// rather than the version the run froze (circus#527). Dispatching a session anyway makes the Node
	// report agent_plugin_unavailable on the first turn, after the run has visibly started. The
	// evidence to refuse it is already on the Workspace, so fail the node here and dispatch nothing.
	if reason := workflowPinnedPluginGap(t, run, w); reason != "" {
		failWorkflowAgentNode(t, runID, version, states, node, reason)
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
	// The attempt follows the committed state: the first start is attempt 1 and the start after a
	// committed `retrying` is the attempt it was waiting for plus one (D3). Nothing about the counter
	// lives in process memory, so a restart between the wait and this start reads the same number.
	// The fresh state deliberately drops the previous attempt's retryAt, error and finishedAt.
	attempt := int64(1)
	if prior, started := states[node.ID]; started {
		attempt = prior.attemptNumber() + 1
	}
	states[node.ID] = workflowNodeState{Status: "running", DisplayName: workflowNodeDisplayName(node), StartedAt: nowRFC3339(), Attempt: attempt}
	if !settleWorkflowRunStates(t, runID, version, "running", "", states) {
		return
	}
	enqueueExecutionWork(t, runID, "agent_session", spec, target, node.ID, attempt, time.Time{})
}

// failWorkflowAgentNode commits one agent node failed with a named reason and finishes the run. It
// keeps the attempt the state already names — the attempt whose start could not proceed — and reads
// an absent state as the first attempt; the budget is history, not something a failure resets (D6).
func failWorkflowAgentNode(t *transaction, runID string, version int64, states map[string]workflowNodeState, node workflowNodeView, reason string) {
	prior := states[node.ID]
	states[node.ID] = workflowNodeState{Status: "failed", DisplayName: workflowNodeDisplayName(node), StartedAt: nowRFC3339(), FinishedAt: nowRFC3339(), Attempt: prior.attemptNumber(), Error: reason, Output: Object{"reason": reason}}
	finishWorkflowRun(t, runID, version, "failed", reason, states)
}

// workflowPinnedPluginGap reports the public reason the run's pinned Agent plugin cannot start on
// this Workspace, or "" when it is installed at the frozen version. The plugin identity is the one
// workflowSessionSpec dispatches (opencodeAgentCLI); the version is the run's frozen one, never the
// space's current selection. A run with no frozen version is left to workflowSessionSpec's own
// agent_plugin_version_unfrozen reason: there is no pinned version to compare, and substituting the
// current one would execute something the run was not created against.
func workflowPinnedPluginGap(t *transaction, run, w Object) string {
	version := run.S("pluginVersion")
	if version == "" {
		return ""
	}
	namespace, identifier, ok := pluginIdentity(opencodeAgentCLI)
	if !ok {
		return "agent_plugin_unavailable"
	}
	inst := t.one(`SELECT * FROM workspace_plugin_instances
		WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3`, w.S("id"), namespace, identifier)
	if inst == nil || inst.S("observedState") != "installed" || inst.S("observedVersion") != version {
		return "agent_plugin_unavailable"
	}
	return ""
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
