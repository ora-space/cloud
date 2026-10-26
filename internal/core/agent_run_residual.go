package core

import "sort"

// residualIdentity re-reads the original ownership in the caller's serialized transaction.
// Neither a request's reclaim ID nor a UUID foreign key alone establishes this scope.
func residualIdentity(t *transaction, row Object) (run, workspace, project Object) {
	require(row != nil, 404, "residual_not_found")
	run = t.one("SELECT * FROM issue_runs WHERE id=$1", row.S("runId"))
	workspace = t.one("SELECT * FROM workspaces WHERE id=$1", row.S("workspaceId"))
	project = t.one("SELECT * FROM projects WHERE id=$1", row.S("projectId"))
	require(run != nil && workspace != nil && project != nil && run.S("executorType") == "agent" &&
		run.S("tenantId") == row.S("tenantId") && run.S("workspaceId") == workspace.S("id") &&
		workspace.S("issueRunId") == run.S("id") && workspace.S("tenantId") == row.S("tenantId") &&
		workspace.S("projectId") == project.S("id") && project.S("tenantId") == row.S("tenantId") &&
		workspace.S("kind") == "isolated" && workspace.N("runtimeGeneration") == row.N("runtimeGeneration"), 409, "residual_identity_conflict")
	return run, workspace, project
}

// collectResidualTargets is a union, never a replacement snapshot. It includes original NULL-scope
// effects by their original operation, and retains missing historical references so absence cannot
// silently turn an unknown target into permission to delete data. The bound delete is not a new
// provisioning responsibility. Each target has kind/id plus its original associations.
func collectResidualTargets(t *transaction, row Object) []Object {
	_, w, _ := residualIdentity(t, row)
	byKey := map[string]Object{}
	add := func(target Object) {
		if target.S("id") == "" {
			return
		}
		key := target.S("kind") + ":" + target.S("id")
		if previous := byKey[key]; previous != nil {
			for k, v := range target {
				if _, exists := previous[k]; !exists {
					previous[k] = v
				}
			}
			return
		}
		byKey[key] = target
	}
	for _, target := range residualObjects(row["reclaimTargets"]) {
		add(target)
	}
	ref := row.O("responsibility")
	for _, spec := range []struct{ list, kind, key string }{
		{"operations", "operation", "id"},
		{"effects", "effect", "id"},
		{"sandboxes", "sandbox", "id"},
		{"nodes", "node", "id"},
		{"executions", "execution", "executionId"},
		{"clones", "clone", "executionId"},
	} {
		for _, original := range residualObjects(ref[spec.list]) {
			target := Object{"kind": spec.kind, "id": original.S(spec.key), "workspaceId": w.S("id")}
			for _, key := range []string{"operationId", "sandboxInstanceId", "generation", "nodeId", "workId"} {
				if v, exists := original[key]; exists {
					target[key] = v
				}
			}
			add(target)
		}
	}
	for _, op := range t.list("SELECT * FROM operations WHERE workspace_id=$1 AND id IS DISTINCT FROM $2::uuid", w.S("id"), nullable(row.S("reclaimOperationId"))) {
		add(Object{"kind": "operation", "id": op.S("id"), "workspaceId": w.S("id")})
	}
	// Resolve all original operations, including a Project-scoped operation named in responsibility.
	operations := []string{}
	for _, target := range byKey {
		if target.S("kind") == "operation" {
			operations = append(operations, target.S("id"))
		}
	}
	for _, id := range operations {
		for _, e := range t.list("SELECT * FROM external_effects WHERE operation_id=$1 AND (workspace_id=$2 OR workspace_id IS NULL)", id, w.S("id")) {
			add(Object{"kind": "effect", "id": e.S("id"), "operationId": id, "workspaceId": w.S("id")})
		}
	}
	for _, e := range t.list("SELECT * FROM external_effects WHERE workspace_id=$1 AND operation_id IS DISTINCT FROM $2::uuid", w.S("id"), nullable(row.S("reclaimOperationId"))) {
		add(Object{"kind": "effect", "id": e.S("id"), "operationId": e.S("operationId"), "workspaceId": w.S("id")})
	}
	// Every planned ensure is itself a stable sandbox identity, even before a sandbox row arrives.
	effects := []string{}
	for _, target := range byKey {
		if target.S("kind") == "effect" {
			effects = append(effects, target.S("id"))
		}
	}
	for _, id := range effects {
		e := t.one("SELECT * FROM external_effects WHERE id=$1", id)
		if e != nil && e.S("kind") == "sandbox_ensure" {
			add(Object{"kind": "sandbox", "id": id, "ensureEffectId": id, "operationId": e.S("operationId"), "workspaceId": w.S("id")})
		}
	}
	for _, s := range t.list("SELECT * FROM sandbox_instances WHERE workspace_id=$1", w.S("id")) {
		add(Object{"kind": "sandbox", "id": s.S("id"), "generation": s.N("generation"), "workspaceId": w.S("id")})
	}
	for _, n := range t.list("SELECT * FROM node_instances WHERE workspace_id=$1", w.S("id")) {
		add(Object{"kind": "node", "id": n.S("id"), "sandboxInstanceId": n.S("sandboxInstanceId"), "nodeId": n.S("nodeId"), "workspaceId": w.S("id")})
	}
	for _, e := range t.list("SELECT * FROM node_executions WHERE workspace_id=$1 OR operation_id=$2", w.S("id"), row.S("runId")) {
		add(Object{"kind": "execution", "id": e.S("executionId"), "operationId": e.S("operationId"), "nodeId": e.S("nodeId"), "workId": e.S("workId"), "workspaceId": w.S("id")})
	}
	for _, e := range t.list("SELECT * FROM clone_executions WHERE workspace_id=$1", w.S("id")) {
		add(Object{"kind": "clone", "id": e.S("executionId"), "operationId": e.S("operationId"), "nodeId": e.S("nodeId"), "workspaceId": w.S("id")})
	}
	for _, work := range t.list("SELECT * FROM execution_work WHERE run_id=$1", row.S("runId")) {
		add(Object{"kind": "work", "id": work.S("id"), "workspaceId": w.S("id")})
		if sid := work.O("target").S("sandboxInstanceId"); sid != "" {
			add(Object{"kind": "sandbox", "id": sid, "workspaceId": w.S("id"), "generation": work.O("target").N("runtimeGeneration")})
		}
	}
	add(Object{"kind": "data", "id": w.S("id"), "workspaceId": w.S("id")})
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	targets := make([]Object, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, byKey[key])
	}
	return targets
}

func refreshResidualTargets(t *transaction, row Object) Object {
	targets := collectResidualTargets(t, row)
	if row.S("legacyIntentId") != "" {
		if jsonText(targets) != jsonText(row["reclaimTargets"]) {
			t.exec("UPDATE agent_run_reclamation_intents SET targets=$2,version=version+1 WHERE id=$1", row.S("legacyIntentId"), jsonText(targets))
		}
		return residualForRun(t, row.S("runId"))
	}
	if jsonText(targets) != jsonText(row["reclaimTargets"]) {
		t.exec("UPDATE agent_run_residuals SET reclaim_targets=$2,reclaim_version=reclaim_version+1 WHERE run_id=$1", row.S("runId"), jsonText(targets))
	}
	return t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", row.S("runId"))
}

// residualSandboxTerminated requires the two distinct physical assertions for the exact stable
// sandbox ID. terminated_at, failed ensures, elapsed time and a missing sandbox row prove neither.
func residualSandboxTerminated(t *transaction, row Object, sid string) bool {
	if target := t.one(`SELECT rt.id FROM agent_run_reclamation_targets rt JOIN agent_run_reclamation_intents i ON i.id=rt.root_intent_id
 WHERE i.tenant_id=$1 AND i.workspace_id=$2 AND rt.sandbox_id=$3 AND rt.state='succeeded'
 AND rt.result->>'terminated'='true' AND rt.result->>'lateEnsureFenced'='true'`, row.S("tenantId"), row.S("workspaceId"), sid); target != nil {
		return true
	}
	for _, e := range t.list(`SELECT * FROM external_effects WHERE project_id=$1 AND workspace_id=$2
 AND kind='sandbox_terminate' AND request->>'sandboxInstanceId'=$3 AND state='succeeded'`, row.S("projectId"), row.S("workspaceId"), sid) {
		if e.O("result").B("terminated") && e.O("result").B("lateEnsureFenced") {
			return true
		}
	}
	return false
}

func residualAllSandboxesTerminated(t *transaction, row Object) bool {
	count := 0
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") == "sandbox" {
			count++
			if !residualSandboxTerminated(t, row, target.S("id")) {
				return false
			}
		}
	}
	return count > 0
}

// residualTargetsLocated checks references against the real tables. Unknown historical references
// cannot be normalized to the newest generation or dropped from the responsibility.
func residualTargetsLocated(t *transaction, row Object) bool {
	for _, target := range residualObjects(row["reclaimTargets"]) {
		id, wid := target.S("id"), row.S("workspaceId")
		switch target.S("kind") {
		case "sandbox":
			s := t.one("SELECT * FROM sandbox_instances WHERE id=$1 AND workspace_id=$2", id, wid)
			if s == nil {
				e := t.one(`SELECT e.id FROM external_effects e JOIN operations o ON o.id=e.operation_id
 WHERE e.id=$1 AND e.kind='sandbox_ensure' AND e.project_id=$2 AND o.tenant_id=$3
 AND (e.workspace_id=$4 OR (e.workspace_id IS NULL AND o.workspace_id=$4))`, id, row.S("projectId"), row.S("tenantId"), wid)
				if e == nil {
					return false
				}
			} else if target.N("generation") > 0 && target.N("generation") != s.N("generation") {
				return false
			}
		case "operation":
			if t.one("SELECT id FROM operations WHERE id=$1 AND tenant_id=$2 AND project_id=$3 AND (workspace_id=$4 OR workspace_id IS NULL)", id, row.S("tenantId"), row.S("projectId"), wid) == nil {
				return false
			}
		case "effect":
			if t.one(`SELECT e.id FROM external_effects e JOIN operations o ON o.id=e.operation_id
 WHERE e.id=$1 AND o.tenant_id=$2 AND e.project_id=$3 AND (e.workspace_id=$4 OR (e.workspace_id IS NULL AND o.workspace_id=$4))`, id, row.S("tenantId"), row.S("projectId"), wid) == nil {
				return false
			}
		case "node":
			if t.one("SELECT id FROM node_instances WHERE id=$1 AND workspace_id=$2 AND sandbox_instance_id=$3", id, wid, target.S("sandboxInstanceId")) == nil {
				return false
			}
		case "execution":
			if t.one("SELECT execution_id FROM node_executions WHERE execution_id=$1 AND workspace_id=$2", id, wid) == nil {
				return false
			}
		case "clone":
			if t.one("SELECT execution_id FROM clone_executions WHERE execution_id=$1 AND workspace_id=$2", id, wid) == nil {
				return false
			}
		case "work":
			if t.one("SELECT id FROM execution_work WHERE id=$1 AND run_id=$2 AND target->>'workspaceId'=$3", id, row.S("runId"), wid) == nil {
				return false
			}
		case "data":
			if id != wid {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func residualExecutionsTerminal(t *transaction, row Object) bool {
	for _, target := range residualObjects(row["reclaimTargets"]) {
		var e Object
		switch target.S("kind") {
		case "execution":
			e = t.one("SELECT * FROM node_executions WHERE execution_id=$1", target.S("id"))
		case "clone":
			e = t.one("SELECT * FROM clone_executions WHERE execution_id=$1", target.S("id"))
		default:
			continue
		}
		if e == nil || e.O("result").S("outcome") == "" {
			return false
		}
	}
	return true
}

func residualInputClosed(t *transaction, row Object) bool {
	c := t.one("SELECT * FROM runtime_controls WHERE workspace_id=$1", row.S("workspaceId"))
	if c == nil || !c.B("inputClosed") {
		return false
	}
	for _, receipt := range residualObjects(row["reclaimEvidence"]) {
		fact := receipt.O("fact")
		if fact.S("kind") != "runtime_closed" || !fact.B("inputClosed") || fact.S("workspaceId") != row.S("workspaceId") ||
			fact.N("controlEpoch") != c.N("controlEpoch") || fact.S("sandboxId") != c.S("boundSandboxId") {
			continue
		}
		// The full list is retained by the verified runtime_ack path; an omitted list is unknown.
		ids, exists := fact["unfinishedExecutionIds"]
		if !exists {
			continue
		}
		switch values := ids.(type) {
		case []any:
			if len(values) == 0 {
				return true
			}
		case []string:
			if len(values) == 0 {
				return true
			}
		}
	}
	return false
}

func residualDeliveryComplete(t *transaction, row Object) bool {
	// There is presently no durable explicit delivery-abandonment record type. Startup/creation
	// abandoned, failed/skipped, timeout and revision_delivery_skips are not such a record. Until
	// an existing explicit source is identified, only the already verified Revision path qualifies.
	verified := t.one(`SELECT r.id FROM revisions r JOIN revision_verifications v ON v.execution_id=r.execution_id
 JOIN node_executions e ON e.execution_id=r.execution_id JOIN execution_work ew ON ew.id=e.work_id
 WHERE r.run_id=$1 AND r.tenant_id=$2 AND r.project_id=$3 AND r.workspace_id=$4
 AND e.operation_id=r.run_id AND e.workspace_id=r.workspace_id AND e.kind='deliver_revision'
 AND ew.run_id=r.run_id AND ew.kind='deliver_revision' AND ew.execution_id=e.execution_id
 AND v.outcome IN ('delivered','unchanged') AND e.result->>'outcome' IN ('revision_delivered','revision_unchanged')`, row.S("runId"), row.S("tenantId"), row.S("projectId"), row.S("workspaceId"))
	if verified == nil {
		return false
	}
	// Every actual delivery must have its own successful verification; a first saved result cannot
	// cover a different late delivery that still needs rescue.
	return t.one(`SELECT e.execution_id FROM node_executions e WHERE e.workspace_id=$1 AND e.kind='deliver_revision'
 AND NOT EXISTS(SELECT 1 FROM revision_verifications v JOIN revisions r ON r.execution_id=v.execution_id
 WHERE v.execution_id=e.execution_id AND v.outcome IN ('delivered','unchanged') AND r.run_id=$2
 AND r.workspace_id=$1 AND r.tenant_id=$3 AND r.project_id=$4)`, row.S("workspaceId"), row.S("runId"), row.S("tenantId"), row.S("projectId")) == nil
}

func residualNeverPlanned(t *transaction, row Object) bool {
	for _, target := range residualObjects(row["reclaimTargets"]) {
		switch target.S("kind") {
		case "sandbox", "node", "execution", "clone", "work":
			return false
		case "effect":
			e := t.one("SELECT kind FROM external_effects WHERE id=$1", target.S("id"))
			if e == nil || e.S("kind") == "sandbox_ensure" {
				return false
			}
		}
	}
	return true
}

// residualEligibility is deliberately conservative about additional historical IDs: only the one
// actual current sandbox may be passed to owned delete. Every other ID needs prior dual confirmation.
func residualEligibility(t *transaction, row Object) bool {
	_, w, p := residualIdentity(t, row)
	if row["reclaimConflictedAt"] != nil || w.B("admissionOpen") || p.S("lifecycle") != "active" || !residualTargetsLocated(t, row) || !residualDeliveryComplete(t, row) {
		return false
	}
	allGone := residualAllSandboxesTerminated(t, row)
	if !allGone && !residualNeverPlanned(t, row) && (!residualInputClosed(t, row) || !residualExecutionsTerminal(t, row)) {
		return false
	}
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") != "sandbox" || residualSandboxTerminated(t, row, target.S("id")) {
			continue
		}
		if t.one("SELECT id FROM sandbox_instances WHERE id=$1 AND workspace_id=$2 AND generation=$3 AND terminated_at IS NULL", target.S("id"), w.S("id"), row.N("runtimeGeneration")) == nil {
			return false
		}
	}
	return t.one("SELECT id FROM execution_tickets WHERE workspace_id=$1 AND state='active' AND terminated_by_force_stop_id IS NULL", w.S("id")) == nil
}
