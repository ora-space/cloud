package core

import (
	"fmt"
	"strings"
)

// residualForWorkspace also finds cleared responsibilities: cleanup never closes the original
// identity's evidence intake. Soft-deleted Workspace rows remain the identity authority.
func residualForWorkspace(t *transaction, workspaceID string) Object {
	if workspaceID == "" {
		return nil
	}
	if row := t.one("SELECT * FROM agent_run_residuals WHERE workspace_id=$1", workspaceID); row != nil {
		return row
	}
	return legacyResidualForWorkspace(t, workspaceID)
}

func residualObjects(value any) []Object {
	var out []Object
	switch values := value.(type) {
	case []Object:
		return values
	case []any:
		for _, item := range values {
			switch v := item.(type) {
			case map[string]any:
				out = append(out, Object(v))
			case Object:
				out = append(out, v)
			}
		}
	}
	return out
}

// appendResidualFact records one validated fact without editing the original responsibility.
// False means a contradictory terminal was durably audited; callers must not apply it to the
// canonical execution/effect. Control returns the stable error only after this transaction commits.
func appendResidualFact(t *transaction, run, key string, fact Object) bool {
	row := residualForRun(t, run)
	require(row != nil, 409, "operation_not_abandoned")
	var previous Object
	entries := residualObjects(row["reclaimEvidence"])
	for _, entry := range entries {
		if entry.S("key") != key {
			continue
		}
		old := entry.O("fact")
		if old.S("kind") == "conflict" {
			if jsonText(old.O("incoming")) == jsonText(fact) {
				t.residualFactConflict = true
				return false
			}
			continue
		}
		if jsonText(old) == jsonText(fact) {
			return true
		}
		previous = old
	}
	// Pre-migration lateFacts stay intact. They may supply the original terminal, never a reason
	// to rewrite the legacy JSON or to infer cleanup eligibility during migration.
	if previous == nil {
		previous = row.O("responsibility").O("lateFacts").O(key)
	}
	if len(previous) > 0 && residualFactTerminal(previous) && !residualCleanupFailureRetryable(t, row, key, previous) && !sameResidualFact(previous, fact) {
		return appendResidualConflict(t, row, key, previous, fact)
	}
	appendResidualEntry(t, row, key, fact, false)
	return true
}

// A3 retries the same bound cleanup operation and effect. Its failed observation does not prove
// disposal and can advance after fresh evidence; keep that observation instead of treating the
// later success as an A4 contradiction. Historical provisioning failures and every successful
// conclusion remain immutable, and external binding conflicts are checked before this path.
func residualCleanupFailureRetryable(t *transaction, row Object, key string, fact Object) bool {
	if fact.S("state") != "failed" || fact.S("kind") != "effect" || (row.S("reclaimMode") != "owned_delete" && row.S("reclaimMode") != "controlled") ||
		row.S("reclaimOperationId") == "" || !strings.HasPrefix(key, "effect:") {
		return false
	}
	id := strings.TrimPrefix(key, "effect:")
	return validID(id) && t.one(`SELECT id FROM external_effects WHERE id=$1 AND operation_id=$2
 AND kind IN ('sandbox_terminate','workspace_data_delete')`, id, row.S("reclaimOperationId")) != nil
}

func appendResidualConflict(t *transaction, row Object, key string, previous, incoming Object) bool {
	t.residualFactConflict = true
	for _, entry := range residualObjects(row["reclaimEvidence"]) {
		old := entry.O("fact")
		if entry.S("key") == key && old.S("kind") == "conflict" && jsonText(old.O("incoming")) == jsonText(incoming) {
			return false
		}
	}
	appendResidualEntry(t, row, key, Object{"kind": "conflict", "state": "conflict", "original": previous, "incoming": incoming}, true)
	return false
}

func appendResidualEntry(t *transaction, row Object, key string, fact Object, conflict bool) {
	source := fact.O("source")
	if len(source) == 0 {
		source = t.residualFactSource
		if len(source) == 0 {
			source = Object{"kind": "cloud_accepted_fact", "identity": key}
		}
	}
	entry := Object{
		"key": key, "fact": fact, "source": source,
		"receivedAt": t.one("SELECT clock_timestamp()::text AS received_at").S("receivedAt"),
	}
	if row.S("legacyIntentId") != "" {
		t.exec(`UPDATE agent_run_reclamation_intents SET evidence=evidence||jsonb_build_array($2::jsonb),version=version+1,
 conflicted_at=CASE WHEN $3 THEN COALESCE(conflicted_at,clock_timestamp()) ELSE conflicted_at END WHERE id=$1`, row.S("legacyIntentId"), jsonText(entry), conflict)
		return
	}
	t.exec(`UPDATE agent_run_residuals SET reclaim_evidence=reclaim_evidence||jsonb_build_array($2::jsonb),
	 reclaim_version=reclaim_version+1,
	 reclaim_conflicted_at=CASE WHEN $3 THEN COALESCE(reclaim_conflicted_at,clock_timestamp()) ELSE reclaim_conflicted_at END
	 WHERE run_id=$1`, row.S("runId"), jsonText(entry), conflict)
}

func residualEffectRow(t *transaction, e Object) Object {
	if row := residualForWorkspace(t, e.S("workspaceId")); row != nil {
		return row
	}
	// Some legacy preallocated ensure plans have no Workspace binding on the effect row.
	if row := t.one(`SELECT ar.* FROM agent_run_residuals ar JOIN operations o ON o.workspace_id=ar.workspace_id
	 WHERE o.id=$1 AND o.tenant_id=ar.tenant_id AND o.project_id=ar.project_id`, e.S("operationId")); row != nil {
		return row
	}
	op := t.one("SELECT workspace_id FROM operations WHERE id=$1", e.S("operationId"))
	return legacyResidualForWorkspace(t, op.S("workspaceId"))
}

func acceptResidualEffectFact(t *transaction, row, e, incoming Object) bool {
	key := "effect:" + e.S("id")
	if e.S("externalId") != "" && incoming.S("externalId") != "" && e.S("externalId") != incoming.S("externalId") {
		return appendResidualConflict(t, row, key,
			Object{"kind": "effect", "state": e.S("state"), "externalId": e.S("externalId"), "result": e.O("result")}, incoming)
	}
	for _, entry := range residualObjects(row["reclaimEvidence"]) {
		if entry.S("key") == key && jsonText(entry.O("fact")) == jsonText(incoming) && e.S("state") != incoming.S("state") {
			// An earlier observation can replay without downgrading the canonical terminal.
			return false
		}
	}
	if e.S("state") == "succeeded" || e.S("state") == "failed" {
		original := Object{
			"kind": "effect", "effectKind": e.S("kind"), "state": e.S("state"),
			"externalId": e.S("externalId"), "result": e.O("result"),
		}
		if !appendResidualFact(t, row.S("runId"), key, original) {
			return false
		}
	}
	return appendResidualFact(t, row.S("runId"), key, incoming)
}

// receipts is a constant selected by the node/clone registry call sites, never request input.
func acceptResidualReceipt(t *transaction, e Object, sequence int64, event, receipts string) bool {
	row := residualForWorkspace(t, e.S("workspaceId"))
	if row == nil {
		return true
	}
	old := t.one("SELECT event FROM "+receipts+" WHERE execution_id=$1 AND sequence=$2", e.S("executionId"), sequence)
	if old != nil && old.S("event") != event {
		prefix := "receipt:"
		// Separate primary keys do not guarantee uniqueness across the two execution registries.
		if receipts == "clone_event_receipts" {
			prefix = "clone_receipt:"
		}
		key := prefix + e.S("executionId") + ":" + fmt.Sprint(sequence)
		return appendResidualConflict(t, row, key, Object{"kind": "receipt", "event": old.S("event")}, Object{"kind": "receipt", "event": event})
	}
	return true
}

func sameResidualFact(a, b Object) bool {
	// Legacy facts also contain receipt timestamps and provisional history. Neither changes the
	// terminal's semantic identity when importing an existing receipt into the append-only column.
	for _, key := range []string{"kind", "state", "externalId", "detail"} {
		if a.S(key) != b.S(key) {
			return false
		}
	}
	return jsonText(a.O("result")) == jsonText(b.O("result"))
}

func residualFactTerminal(fact Object) bool {
	switch fact.S("state") {
	case "succeeded", "failed", "ready":
		return true
	}
	return false
}

// requireNoResidualConflict fences future mutations even after a responsibility was cleared.
func requireNoResidualConflict(t *transaction, workspaceID string) {
	require(!executionNonAcceptanceConflicted(t, workspaceID), 409, "execution_non_acceptance_conflict")
	row := residualForWorkspace(t, workspaceID)
	require(row == nil || row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
}

// residualExecutionIdentity verifies the original work and resource binding without requiring a
// live Node, open admission, an unfinished Run, or an undeleted Workspace.
func residualExecutionIdentity(t *transaction, e Object) Object {
	row := residualForWorkspace(t, e.S("workspaceId"))
	if row == nil {
		return nil
	}
	w := t.one("SELECT * FROM workspaces WHERE id=$1", row.S("workspaceId"))
	require(w != nil && w.S("tenantId") == row.S("tenantId") && w.S("projectId") == row.S("projectId") && w.S("issueRunId") == row.S("runId"), 409, "residual_identity_conflict")
	if e.S("kind") == "agent_session" || e.S("kind") == "deliver_revision" || e.S("kind") == "deliver_scheduled_artifacts" {
		require(e.S("operationId") == row.S("runId"), 409, "residual_identity_conflict")
		work := t.one("SELECT * FROM execution_work WHERE id=$1 AND run_id=$2", e.S("workId"), row.S("runId"))
		require(work != nil && work.S("executionId") == e.S("executionId") && work.S("kind") == e.S("kind"), 409, "residual_identity_conflict")
		target := work.O("target")
		require(target.S("workspaceId") == w.S("id") && target.S("nodeId") == e.S("nodeId"), 409, "residual_identity_conflict")
		sandbox := t.one("SELECT * FROM sandbox_instances WHERE id=$1 AND workspace_id=$2", target.S("sandboxInstanceId"), w.S("id"))
		require(sandbox != nil && sandbox.N("generation") <= row.N("runtimeGeneration"), 409, "residual_identity_conflict")
	} else {
		require(t.one("SELECT id FROM operations WHERE id=$1 AND workspace_id=$2 AND tenant_id=$3 AND project_id=$4", e.S("operationId"), w.S("id"), row.S("tenantId"), row.S("projectId")) != nil, 409, "residual_identity_conflict")
	}
	return row
}

func residualExecutionFact(e, result Object) (string, Object) {
	kind, prefix := e.S("kind"), "execution:"
	switch kind {
	case "install_plugins", "remove_plugins":
		kind, prefix = "plugin", "plugin:"
	case "":
		kind, prefix = "clone", "clone:"
	}
	state := executionState(Object{"result": result})
	return prefix + e.S("executionId"), Object{"kind": kind, "state": state, "result": result}
}

func acceptResidualExecutionFact(t *transaction, e, result Object) bool {
	row := residualExecutionIdentity(t, e)
	if row == nil {
		return true
	}
	key, incoming := residualExecutionFact(e, result)
	if previous := e.O("result"); len(previous) > 0 {
		_, original := residualExecutionFact(e, previous)
		if !appendResidualFact(t, row.S("runId"), key, original) {
			return false
		}
	}
	return appendResidualFact(t, row.S("runId"), key, incoming)
}
