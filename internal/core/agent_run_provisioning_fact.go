package core

// The D5 late-fact takeover entry. Cloud's abandonment of a blocked creation ends the business
// wait, not the outside world: permits issued before the deadline, Substrate ensures in flight and
// work a Node already accepted can still complete afterwards. The operation's own actions are
// closed to a failed operation by design (a re-claim must not become a new dispatch right), so
// without a dedicated entry those facts would be refused or, worse, silently dropped by consumers
// that only look through an active operation. `provisioning_fact` is that entry: the Controller
// submits real results with its current service identity and lease, Cloud validates them against
// the original operation, effect and execution identities, stores them through the exact same
// appliers the live paths use, and receipts them onto the residual ledger before acknowledging.
// Nothing reopens: no admission, no starting, no new dispatch, no second settlement.

// provisioningFact is the control action behind RecordProvisioningFact. One call carries one fact;
// the submission identity makes a lost-reply retry a replay of the recorded response.
func provisioningFact(t *transaction, r *ControlRequest) Object {
	o := t.one("SELECT * FROM operations WHERE id=$1", r.OperationID)
	require(o != nil, 404, "not_found")
	// Only a creation Cloud itself abandoned accepts late facts. An unfinished creation must be
	// driven through its normal actions, and a creation that failed on its own evidence settled
	// its run through the ordinary hook — neither is a residual whose facts need receipting.
	require(o.S("kind") == "create_workspace" && o.S("state") == "failed" && o.S("errorCode") == "provisioning_result_unknown",
		409, "operation_not_abandoned")
	w := t.one("SELECT * FROM workspaces WHERE id=$1", o.S("workspaceId"))
	require(w != nil && w.S("issueRunId") != "", 409, "operation_not_abandoned")
	run := w.S("issueRunId")
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", run) != nil,
		409, "operation_not_abandoned")

	switch r.Body.S("kind") {
	case "effect":
		return provisioningEffectFact(t, r, o, run)
	case "clone", "plugin":
		return provisioningExecutionFact(t, r, run)
	case "creation":
		return provisioningCreationFact(t, r, run)
	default:
		reject(400, "invalid_fact_kind")
	}
	return nil
}

// provisioningEffectFact stores what the Substrate really reported for one effect of the abandoned
// creation. The evidence rules are the live path's own (effectResult), applied to the original
// effect row; the fact is receipted onto the ledger under the submitting epoch, because it is the
// current lease holder that reconciled it.
func provisioningEffectFact(t *transaction, r *ControlRequest, o Object, run string) Object {
	if r.EffectID == "" {
		r.EffectID = r.Body.S("effectId")
	}
	require(validID(r.EffectID), 404, "not_found")
	// effectResult reads exactly the operation's id and controller epoch; a shallow copy keeps the
	// real operation's fields (a legacy plugin writeback would still resolve them) while recording
	// the fact under the epoch that submitted it, not the one that last held the operation.
	factOp := Object{}
	for k, v := range o {
		factOp[k] = v
	}
	factOp["controllerEpoch"] = r.Body.N("epoch")
	out := effectResult(t, r, factOp)
	e := out.O("effect")
	// effectResult appends the same validated fact on both the live and late intake paths.
	return Object{"operation": t.one("SELECT * FROM operations WHERE id=$1", o.S("id")), "effect": e}
}

// provisioningExecutionFact stores the real terminal outcome of one execution the abandoned
// creation dispatched, through the same appliers the live takeover and query paths use: a clone
// fact lands on its clone_executions row, a plugin fact on its node_executions row and plugin
// instances. Identical facts replay; a different fact for the same identity conflicts.
func provisioningExecutionFact(t *transaction, r *ControlRequest, run string) Object {
	execution := r.Body.S("executionId")
	require(execution != "" && len(execution) <= 256, 400, "invalid_execution")
	// The appliers resolve the row by (execution, operation) themselves, which is the identity
	// check: a fact naming another operation's execution is a plain conflict, not a fact about
	// this creation.
	body := Object{"epoch": r.Body.N("epoch"), "operationId": r.OperationID, "executionId": execution, "result": r.Body.O("result")}
	factRequest := &ControlRequest{Action: "clone_queried", Body: body}
	var stored Object
	if r.Body.S("kind") == "clone" {
		stored = cloneResult(t, factRequest, false)
	} else {
		e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2", execution, r.OperationID)
		require(e != nil, 404, "not_found")
		require(e.S("kind") == "install_plugins" || e.S("kind") == "remove_plugins", 409, "invalid_execution")
		stored = nodeExecutionResult(t, factRequest, false)
	}
	// The execution applier owns the ledger receipt as well as the canonical result.
	return Object{"operation": t.one("SELECT * FROM operations WHERE id=$1", r.OperationID), "execution": stored}
}

// executionState reduces one stored execution row to its outcome word for the ledger: no result is
// still pending, a terminal outcome is succeeded or failed.
func executionState(e Object) string {
	switch e.O("result").S("outcome") {
	case "clone_ready", "plugins_result", "agent_session_ended", "revision_delivered", "revision_unchanged":
		return "succeeded"
	case "":
		return "pending"
	default:
		return "failed"
	}
}

// provisioningCreationFact records the creation's own real outcome: the Controller asserts the
// Workspace really came up, or really failed, with evidence Cloud cannot obtain itself. It is
// evidence only — a late success can never reopen admission or move the run out of its business
// terminal, exactly as a late terminal cannot resurrect an abandoned startup (D4).
func provisioningCreationFact(t *transaction, r *ControlRequest, run string) Object {
	outcome := r.Body.S("outcome")
	require(outcome == "ready" || outcome == "failed", 400, "invalid_outcome")
	detail := r.Body.S("detail")
	require(len(detail) <= 2000, 400, "invalid_detail")
	fact := Object{"kind": "creation", "state": outcome}
	if detail != "" {
		fact["detail"] = detail
	}
	appendResidualFact(t, run, "creation", fact)
	return Object{"operation": t.one("SELECT * FROM operations WHERE id=$1", r.OperationID)}
}
