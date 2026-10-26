package core

// nodeExecutionColumns is the Controller-facing projection of node_executions: every column of the
// table except workflow_node_id and workflow_node_attempt. Those columns are Cloud-internal routing —
// which graph node a session ran for, and which attempt of it — and no Controller contract has a field
// for either: the proto ExecutionRecord declares none, and the JSON binding's NodeExecution schema is
// closed, so a `SELECT *` that carried the column failed the response contract the moment migration
// 0041 added it.
//
// So a row that reaches a response body is projected explicitly, the way workflowRunSummary projects
// a run row, rather than read whole. A column added to the table therefore does not reach the wire by
// itself; putting one there is a deliberate edit to this list and to the schema it must satisfy.
const nodeExecutionColumns = `execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,result,dispatched_epoch,last_event_sequence,terminated_by_force_stop_id,created_at,updated_at`

// nodeDispatch registers a plugin, session or delivery execution before any Node sees it.
// The same execution and input is idempotent. A different input or Node conflicts.
func nodeDispatch(t *transaction, r *ControlRequest) Object {
	operation, execution, node := r.Body.S("operationId"), r.Body.S("executionId"), r.Body.S("nodeId")
	input := r.Body.O("input")
	if input.S("kind") == "agent_session" || input.S("kind") == "deliver_revision" || input.S("kind") == "deliver_scheduled_artifacts" {
		require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", operation) == nil, 409, "dispatch_conflict")
	}
	require(execution != "" && node != "" && len(input) > 0, 400, "invalid_dispatch")
	require(t.one("SELECT execution_id FROM clone_executions WHERE execution_id=$1", execution) == nil, 409, "dispatch_conflict")
	if existing := t.one("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE execution_id=$1", execution); existing != nil {
		require(existing.S("operationId") == operation && existing.S("nodeId") == node && jsonText(existing.O("input")) == jsonText(input), 409, "dispatch_conflict")
		return existing
	}
	switch input.S("kind") {
	case "install_plugins", "remove_plugins":
		return dispatchPluginExecution(t, r, operation, execution, node, input)
	case "agent_session", "deliver_revision", "deliver_scheduled_artifacts":
		return dispatchRunExecution(t, r, operation, execution, node, input)
	default:
		reject(400, "invalid_dispatch")
	}
	return nil
}

func dispatchPluginExecution(t *transaction, r *ControlRequest, operation, execution, node string, input Object) Object {
	o := t.one("SELECT * FROM operations WHERE id=$1 AND workspace_id IS NOT NULL", operation)
	require(o != nil, 404, "not_found")
	require(o.S("step") == "plugin" && o.S("state") == "running", 409, "dispatch_conflict")
	require(o.N("controllerEpoch") == r.Body.N("epoch"), 409, "stale_operation")
	require(jsonText(pluginInputOf(o)) == jsonText(input), 409, "dispatch_conflict")
	wid := o.S("workspaceId")
	requireNoForceStop(t, wid)
	current := currentNode(t, wid)
	require(current.S("nodeId") == node, 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM node_executions WHERE operation_id=$1 AND kind IN ('install_plugins','remove_plugins') AND terminated_by_force_stop_id IS NULL AND (result IS NULL OR result->>'outcome'='plugins_result')", operation) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO node_executions(execution_id,kind,operation_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch) VALUES($1,$2,$3,$4,$5,$1,$6,$7)",
		execution, input.S("kind"), operation, wid, node, jsonText(input), r.Body.N("epoch"))
	return t.one("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE execution_id=$1", execution)
}

func dispatchRunExecution(t *transaction, r *ControlRequest, runID, execution, node string, input Object) Object {
	if root := t.one("SELECT workspace_id FROM agent_run_reclamation_intents WHERE run_id=$1 AND root_intent_id IS NULL", runID); root != nil {
		requireNoControlledReclamation(t, root.S("workspaceId"), "")
	}
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", runID) == nil, 409, "dispatch_conflict")
	// The work item names its owner — IssueRun or WorkflowRun (D8); operation_id is that run's id
	// either way, so the ownership check re-reads whichever column the row carries.
	work := t.one("SELECT * FROM execution_work WHERE (run_id=$1 OR workflow_run_id=$1) AND execution_id IS NULL AND kind=$2", runID, input.S("kind"))
	require(work != nil, 409, "dispatch_conflict")
	// A claim is only a snapshot. Recheck business eligibility under the same transaction lock as
	// settlement so a pre-release claim cannot register a fresh session or delivery afterwards. Only
	// IssueRun-owned work carries run_id; workflow work names its run in workflow_run_id and this
	// predicate does not read the workflow_runs lifecycle.
	if work.S("runId") != "" {
		require(t.one(executionWorkLiveRuns+" AND id=$1", runID) != nil, 409, "dispatch_conflict")
	}
	require(jsonText(work.O("input")) == jsonText(input), 409, "dispatch_conflict")
	target := work.O("target")
	require(target.S("nodeId") == node, 409, "dispatch_conflict")
	wid := target.S("workspaceId")
	requireNoForceStop(t, wid)
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	require((w.S("issueRunId") == runID || w.S("workflowRunId") == runID) && w.S("observedState") == "ready" && w.B("admissionOpen"), 409, "dispatch_conflict")
	c := runtimeControl(t, wid)
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == runID, 409, "runtime_control_required")
	current := currentNode(t, wid)
	require(current.S("nodeId") == node && current.S("sandboxInstanceId") == target.S("sandboxInstanceId"), 409, "dispatch_conflict")
	if input.S("kind") == "agent_session" {
		if input.S("modelBindingId") != "" {
			require(current.B("modelProxy"), 409, "model_proxy_required")
		}
		require(declaredAgentCapable(current, input.O("agentConfiguration")), 409, "agent_adapter_required")
		// An IssueRun runs exactly one agent session in its life. A WorkflowRun runs one per agent
		// node (D4 executes them serially through one Workspace), so its sessions are bounded by the
		// work item this dispatch consumes — the unregistered item is gone once dispatched (below),
		// and the in-flight check that follows admits only one at a time. The per-run lifetime
		// uniqueness would refuse every node after the first.
		if w.S("issueRunId") == runID {
			require(t.one("SELECT execution_id FROM node_executions WHERE operation_id=$1 AND kind='agent_session'", runID) == nil, 409, "dispatch_conflict")
		}
	}
	if input.S("kind") == "deliver_revision" || input.S("kind") == "deliver_scheduled_artifacts" {
		require(t.one("SELECT execution_id FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND workspace_id=$3 AND kind='agent_session' AND result->>'outcome'='agent_session_ended'", input.S("sessionExecutionId"), runID, wid) != nil, 409, "dispatch_conflict")
	}
	// Both deliveries may run together after their session ended. A session still excludes all
	// run executions, and each delivery excludes another pending execution of its own kind.
	require(t.one(`SELECT execution_id FROM node_executions WHERE operation_id=$1
		AND kind IN ('agent_session','deliver_revision','deliver_scheduled_artifacts')
		AND (kind='agent_session' OR kind=$2 OR $2='agent_session')
		AND result IS NULL AND terminated_by_force_stop_id IS NULL`, runID, input.S("kind")) == nil, 409, "dispatch_conflict")
	// The attempt is copied from the claimed work item, never from the wire: it is Cloud's routing
	// identity for the execution (retry decision D4), and the Controller neither knows nor needs it.
	t.exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch,workflow_node_id,workflow_node_attempt)
		VALUES($1,$2,$3,$4,$5,$6,$1,$7,$8,NULLIF($9,''),$10)`,
		execution, input.S("kind"), runID, work.S("id"), wid, node, jsonText(input), r.Body.N("epoch"), work.S("workflowNodeId"), work.N("workflowNodeAttempt"))
	t.exec("UPDATE execution_work SET execution_id=$2 WHERE id=$1", work.S("id"), execution)
	return t.one("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE execution_id=$1", execution)
}

func executionResult(t *transaction, r *ControlRequest, withReceipt bool) Object {
	if t.one("SELECT execution_id FROM node_executions WHERE execution_id=$1", r.Body.S("executionId")) != nil {
		return nodeExecutionResult(t, r, withReceipt)
	}
	return cloneResult(t, r, withReceipt)
}

// nodeExecutionResult stores a terminal fact. Session and delivery facts require a receipt whose
// sequence is the next one, so a session cannot end before its Thread events.
func nodeExecutionResult(t *transaction, r *ControlRequest, withReceipt bool) Object {
	operation, execution := r.Body.S("operationId"), r.Body.S("executionId")
	result := r.Body.O("result")
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2", execution, operation)
	require(e != nil, 404, "not_found")
	require(result.O("node").S("nodeId") == e.S("nodeId"), 409, "result_conflict")
	validateNodeResult(e, result)
	if withReceipt && !acceptResidualReceipt(t, e, r.Body.N("sequence"), r.Body.S("event"), "node_event_receipts") {
		return e
	}
	if !acceptResidualExecutionFact(t, e, result) {
		return e
	}
	fresh := len(e.O("result")) == 0
	lateDelivery := false
	if !fresh {
		require(jsonText(e.O("result")) == jsonText(result), 409, "result_conflict")
	} else {
		validateNodeResult(e, result)
		if e.S("kind") == "deliver_revision" {
			lateDelivery = deliveryFactIsLate(t, e)
		}
		if e.S("kind") == "install_plugins" || e.S("kind") == "remove_plugins" {
			applyPluginResults(t, e, result)
		}
		t.exec("UPDATE node_executions SET result=$2,updated_at=now() WHERE execution_id=$1", execution, jsonText(result))
	}
	if withReceipt || e.S("kind") == "agent_session" || e.S("kind") == "deliver_revision" || e.S("kind") == "deliver_scheduled_artifacts" {
		require(withReceipt, 400, "invalid_receipt")
		// Plugin queries settle the result without an ACK receipt. Only their first real
		// terminal event may add that missing receipt; later terminal replays, sessions and
		// deliveries still require the original sequence. recordNodeReceipt fences gaps/bytes.
		firstPluginReceipt := e.N("lastEventSequence") == 0 && (e.S("kind") == "install_plugins" || e.S("kind") == "remove_plugins")
		if !fresh && !firstPluginReceipt {
			require(t.one("SELECT sequence FROM node_event_receipts WHERE execution_id=$1 AND sequence=$2", execution, r.Body.N("sequence")) != nil, 409, "receipt_conflict")
		}
		recordNodeReceipt(t, execution, r.Body.N("sequence"), r.Body.S("event"))
	}
	if fresh && e.S("kind") == "agent_session" {
		sessionEnded(t, operation, execution, result)
	}
	if fresh && !lateDelivery && e.S("kind") == "deliver_revision" && result.S("outcome") == "revision_failed" {
		deliverySettled(t, operation, execution, Object{"outcome": "failed", "reason": result.S("reason")})
	}
	if fresh && e.S("kind") == "deliver_revision" && revisionSuccess(result) {
		settleVerifiedRevision(t, r, e, lateDelivery)
	}
	if fresh && e.S("kind") == "deliver_scheduled_artifacts" {
		settleVerifiedArtifacts(t, r, operation, execution, result)
	}
	return t.one("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE execution_id=$1", execution)
}

func validateNodeResult(e, result Object) {
	switch e.S("kind") {
	case "install_plugins", "remove_plugins":
		require(result.S("outcome") == "plugins_result" || result.S("outcome") == "plugins_failed", 409, "result_conflict")
		if result.S("outcome") == "plugins_failed" {
			require(result.S("reason") == "plugin_root_unavailable" || result.S("reason") == "interrupted", 400, "invalid_result")
		}
	case "agent_session":
		require(result.S("outcome") == "agent_session_ended", 409, "result_conflict")
		require(sessionEndReasons[result.S("reason")], 400, "invalid_result")
	case "deliver_scheduled_artifacts":
		// A scheduled run reports either what it uploaded, as objects Cloud verifies itself, or why it
		// could not. The declared set is never taken from this payload: Cloud reads it from the
		// session's immutable terminal result.
		switch result.S("outcome") {
		case "scheduled_artifacts_delivered":
			uploaded := objectRows(result["artifacts"])
			require(len(uploaded) <= maxScheduledArtifactItems, 400, "invalid_result")
			for _, object := range uploaded {
				require(object.S("key") != "" && object.N("size") >= 0 && lowerHexDigest(object.S("sha256")), 400, "invalid_result")
			}
		case "scheduled_artifacts_failed":
			require(scheduledArtifactsFailureReasons[result.S("reason")], 400, "invalid_result")
		default:
			reject(409, "result_conflict")
		}
	default:
		switch result.S("outcome") {
		case "revision_delivered", "revision_unchanged":
			validateDeliveryDeclaration(e, result)
		case "revision_failed":
			require(revisionFailureReasons[result.S("reason")], 400, "invalid_result")
		default:
			reject(409, "result_conflict")
		}
	}
}

var sessionEndReasons = map[string]bool{
	"user_ended": true, "idle_timeout": true, "cancelled": true, "agent_failed": true, "interrupted": true,
}

// settleVerifiedArtifacts hands a scheduled run's artifact delivery to the business side. A reported
// failure carries no verification (there is nothing to verify), while a reported upload must come
// with the object-store verdict prepared outside this transaction.
func settleVerifiedArtifacts(t *transaction, r *ControlRequest, runID, executionID string, result Object) {
	if result.S("outcome") == "scheduled_artifacts_failed" {
		scheduledArtifactsSettled(t, runID, executionID, Object{"outcome": "failed", "reason": result.S("reason")})
		return
	}
	require(r.artifacts != nil, 409, "invalid_delivery_verification")
	scheduledArtifactsSettled(t, runID, executionID, Object{
		"outcome": "delivered", "input": r.artifacts.input, "objects": r.artifacts.objects,
	})
}

// revisionFailureReasons is the closed set a Node may report. The retired bundle_failed value is
// reserved on the wire, so a result carrying it decodes to a reason outside this set and is refused.
var revisionFailureReasons = map[string]bool{
	"session_not_settled": true, "checkout_unavailable": true, "snapshot_failed": true,
	"history_unavailable": true, "upload_failed": true, "push_rejected": true, "push_failed": true,
}

func recordNodeReceipt(t *transaction, execution string, sequence int64, event string) {
	require(sequence >= 1 && event != "", 400, "invalid_receipt")
	e := t.one("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", execution)
	require(e != nil, 404, "not_found")
	if old := t.one("SELECT * FROM node_event_receipts WHERE execution_id=$1 AND sequence=$2", execution, sequence); old != nil {
		require(old.S("event") == event, 409, "receipt_conflict")
		return
	}
	require(sequence == e.N("lastEventSequence")+1, 409, "receipt_conflict")
	t.exec("INSERT INTO node_event_receipts(execution_id,sequence,event) VALUES($1,$2,$3)", execution, sequence, event)
	t.exec("UPDATE node_executions SET last_event_sequence=$2 WHERE execution_id=$1", execution, sequence)
}

func lookupExecution(t *transaction, execution string) Object {
	e := t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
	if e == nil {
		e = t.one("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE execution_id=$1", execution)
	}
	require(e != nil, 404, "not_found")
	return e
}

func pendingExecutions(t *transaction, node string) Object {
	var clones, nodes []Object
	if node == "" {
		clones = t.list("SELECT * FROM clone_executions WHERE result IS NULL AND terminated_by_force_stop_id IS NULL ORDER BY created_at,execution_id")
		nodes = t.list("SELECT " + nodeExecutionColumns + " FROM node_executions WHERE result IS NULL AND terminated_by_force_stop_id IS NULL ORDER BY created_at,execution_id")
	} else {
		clones = t.list("SELECT * FROM clone_executions WHERE result IS NULL AND terminated_by_force_stop_id IS NULL AND node_id=$1 ORDER BY created_at,execution_id", node)
		nodes = t.list("SELECT "+nodeExecutionColumns+" FROM node_executions WHERE result IS NULL AND terminated_by_force_stop_id IS NULL AND node_id=$1 ORDER BY created_at,execution_id", node)
	}
	return Object{"executions": append(clones, nodes...)}
}
