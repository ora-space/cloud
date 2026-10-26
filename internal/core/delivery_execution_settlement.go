package core

// settleDeliveryExecutions closes only the named delivery family's new dispatch qualification.
// result, sequence and receipts remain execution-end facts, including after an administrative cut.
func settleDeliveryExecutions(t *transaction, runID, kind, source string) {
	t.exec(`UPDATE node_executions SET business_settled_at=clock_timestamp(),business_settlement_source=$3
		WHERE operation_id=$1 AND kind=$2 AND result IS NULL AND business_settled_at IS NULL`, runID, kind, source)
}

// executionWorkEligible is shared by claim and registration; a stale claim cannot outlive the
// family's own decision while its peer still keeps the run in delivering.
const executionWorkEligible = `(ew.run_id IS NULL OR (
    ew.run_id IN (` + executionWorkLiveRuns + `)
    AND (ew.kind<>'deliver_scheduled_artifacts' OR EXISTS (
        SELECT 1 FROM scheduled_run_artifact_deliveries d WHERE d.issue_run_id=ew.run_id
        AND d.state='delivering' AND d.deadline_at>clock_timestamp()))
    AND (ew.kind<>'deliver_revision' OR NOT EXISTS (
        SELECT 1 FROM issue_runs r WHERE r.id=ew.run_id AND r.result->>'deliveryState' IS NOT NULL))
))`

// freezeExecutionOrigin preserves the actual dispatch-instance and frozen work input. An upgraded
// process cannot invent this snapshot for old rows from the current incarnation's empty ledger.
func freezeExecutionOrigin(t *transaction, control, node, work, input Object, execution string, epoch int64) Object {
	binding := runtimeBinding(t, control, node)
	binding["controllerEpoch"] = epoch
	binding["executionId"], binding["nodeOperationId"] = execution, execution
	return Object{"binding": binding, "workId": work.S("id"), "inputJson": jsonText(input)}
}

// executionFences requests facts from the current connection to the original sandbox. A new
// incarnation may replay an old persisted proof, but cannot create one for an empty replacement.
func executionFences(t *transaction, r *ControlRequest) []Object {
	rows := t.list(`SELECT e.* FROM node_executions e
		JOIN workspaces w ON w.id=e.workspace_id
		WHERE e.node_id=$1 AND e.business_settled_at IS NOT NULL AND e.result IS NULL
		AND e.non_acceptance_proof IS NULL AND e.dispatch_identity IS NOT NULL
		AND e.terminated_by_force_stop_id IS NULL
		ORDER BY e.created_at,e.execution_id LIMIT 100`, r.Body.S("nodeId"))
	out := []Object{}
	for _, e := range rows {
		origin := e.O("dispatchIdentity")
		original := origin.O("binding")
		n := t.one(`SELECT * FROM node_instances WHERE sandbox_instance_id=$1 AND node_id=$2
			AND ended_at IS NULL AND initialized AND connection_state='connected'
			AND last_seen_at>clock_timestamp()-interval '30 seconds'`, original.S("sandboxId"), e.S("nodeId"))
		if n == nil {
			continue
		}
		c := runtimeControl(t, e.S("workspaceId"))
		authority := runtimeBinding(t, c, n)
		if authority.N("runtimeGeneration") != original.N("runtimeGeneration") {
			continue
		}
		authority["controllerEpoch"] = r.Body.N("epoch")
		out = append(out, Object{"authority": authority, "origin": origin})
	}
	return out
}

// recordExecutionNonAcceptance is deliberately distinct from executionResult: no fabricated
// sequence or receipt can turn an administrative cut into a terminal event.
func recordExecutionNonAcceptance(t *transaction, r *ControlRequest) Object {
	proof := r.Body.O("proof")
	fence := proof.O("fence")
	origin := fence.O("origin")
	binding := origin.O("binding")
	execution := binding.S("executionId")
	require(validID(proof.S("proofId")), 400, "invalid_non_acceptance_proof")
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1", execution)
	require(e != nil && e["businessSettledAt"] != nil && e["dispatchIdentity"] != nil, 409, "execution_fence_required")
	require(jsonText(e.O("dispatchIdentity")) == jsonText(origin), 409, "execution_origin_conflict")
	authority := fence.O("authority")
	for _, key := range []string{"tenantId", "workspaceId", "sandboxId", "nodeId", "nodeInstanceId", "nodeIncarnationId"} {
		require(authority.S(key) != "" && authority.S(key) == binding.S(key), 409, "execution_origin_conflict")
	}
	require(authority.N("runtimeGeneration") == binding.N("runtimeGeneration"), 409, "execution_origin_conflict")
	if existing := e.O("nonAcceptanceProof"); len(existing) > 0 {
		if jsonText(existing) != jsonText(proof) {
			recordNonAcceptanceConflict(t, execution, Object{"proof": proof})
		}
		return Object{}
	}
	if e["result"] != nil || e.N("lastEventSequence") != 0 {
		recordNonAcceptanceConflict(t, execution, Object{"proof": proof})
		return Object{}
	}
	require(e["terminatedByForceStopId"] == nil, 409, "execution_already_settled")
	t.exec("UPDATE node_executions SET non_acceptance_proof=$2 WHERE execution_id=$1", execution, jsonText(proof))
	return Object{}
}

// recordNonAcceptanceConflict commits contradictory evidence but never applies it or returns ACK.
func recordNonAcceptanceConflict(t *transaction, execution string, fact Object) {
	t.exec(`INSERT INTO execution_non_acceptance_conflicts(execution_id,fact_key,fact,source)
		VALUES($1,$2,$3,$4) ON CONFLICT(execution_id,fact_key) DO NOTHING`, execution, requestHash("non_acceptance_conflict", execution, fact), jsonText(fact), jsonText(t.residualFactSource))
	t.executionNonAcceptanceConflict = true
}

// executionNonAcceptanceConflicted keeps contradictory evidence from becoming an idle decision.
func executionNonAcceptanceConflicted(t *transaction, workspace string) bool {
	return t.one(`SELECT c.execution_id FROM execution_non_acceptance_conflicts c
		JOIN node_executions e USING(execution_id) WHERE e.workspace_id=$1 LIMIT 1`, workspace) != nil
}

// committedNonAcceptanceExecutions replays explicit proof facts after a lost response. Absence
// from the ordinary pending list alone must never be treated as execution-end evidence.
func committedNonAcceptanceExecutions(t *transaction, node string) []Object {
	return t.list(`SELECT e.execution_id FROM node_executions e WHERE e.node_id=$1
		AND e.non_acceptance_proof IS NOT NULL AND NOT EXISTS(
		SELECT 1 FROM execution_non_acceptance_conflicts c WHERE c.execution_id=e.execution_id)
		ORDER BY e.execution_id`, node)
}
