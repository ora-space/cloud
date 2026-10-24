package core

func managedIntentView(t *transaction, id string) Object {
	intent := t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", id)
	require(intent != nil, 404, "residual_not_found")
	root := controlledIntentRoot(t, intent)
	row := controlledRootRow(t, root)
	out := Object{"intent": intent, "rootIntent": root, "residual": row, "targets": t.list("SELECT * FROM agent_run_reclamation_targets WHERE root_intent_id=$1 ORDER BY id", root.S("id")), "openTargets": controlledOpenTargets(t, root, row, intent), "audit": intent["audit"]}
	if intent.S("state") == "succeeded" {
		out["openTargets"] = []Object{}
	}
	if row.S("reclaimOperationId") != "" {
		out["operation"] = t.one("SELECT id,kind,state,step,error_code,version FROM operations WHERE id=$1", row.S("reclaimOperationId"))
	}
	return out
}

func getManagedResidual(t *transaction, r *ResidualManagementRequest) Object {
	if r.IntentID != "" {
		intent := t.one("SELECT id FROM agent_run_reclamation_intents WHERE id=$1 AND tenant_id=$2", r.IntentID, r.TenantID)
		require(intent != nil, 404, "residual_not_found")
		return managedIntentView(t, intent.S("id"))
	}
	require(r.RunID != "" || r.WorkspaceID != "" || r.ReclaimID != "", 400, "residual_target_required")
	items := listManagedResiduals(t, r)
	rows := residualObjects(items["items"])
	require(len(rows) == 1, 404, "residual_not_found")
	item := rows[0]
	if item.S("intentId") != "" {
		return managedIntentView(t, item.S("intentId"))
	}
	return Object{"resource": item, "residual": residualForWorkspace(t, item.S("workspaceId"))}
}

func listManagedResiduals(t *transaction, r *ResidualManagementRequest) Object {
	limit := r.Limit
	if limit == 0 {
		limit = 100
	}
	require(limit > 0 && limit <= 100, 400, "invalid_limit")
	items := t.list(`WITH resources AS (
 SELECT ar.tenant_id,ar.project_id,ar.run_id,ar.workspace_id,ar.reclaim_id,ar.reclaim_state AS state,
 ar.disposition,ar.reclaim_operation_id AS operation_id,NULL::uuid AS legacy_operation_id,'residual_ledger'::text AS source,
 ar.reclaim_conflicted_at AS conflicted_at,ar.reclaim_checked_at AS checked_at,ar.stop_evidence,false AS legacy_delete_operation_missing
 FROM agent_run_residuals ar
 UNION ALL
 SELECT w.tenant_id,w.project_id,i.id,w.id,ri.reclaim_id,COALESCE(ri.state,'reconciling'),COALESCE(child.disposition,ri.disposition),
 ri.delete_operation_id,o.id,'legacy_d8',ri.conflicted_at,ri.last_checked_at,'{}'::jsonb,
 COALESCE(ri.legacy_delete_operation_missing,o.id IS NULL)
 FROM workspaces w JOIN issue_runs i ON i.id=w.issue_run_id
 LEFT JOIN LATERAL (SELECT op.id FROM operations op WHERE op.workspace_id=w.id AND op.kind='delete_workspace'
 AND op.state='failed' AND op.error_code='node_unavailable' ORDER BY op.created_at,op.id LIMIT 1) o ON true
 LEFT JOIN agent_run_reclamation_intents ri ON ri.workspace_id=w.id AND ri.root_intent_id IS NULL
 LEFT JOIN agent_run_reclamation_intents child ON child.root_intent_id=ri.id AND child.state='succeeded'
 WHERE i.phase='done' AND i.executor_type='agent' AND w.kind='isolated'
 AND (o.id IS NOT NULL OR ri.legacy_delete_operation_missing OR
  (i.failure_reason='workspace_unavailable' AND w.deleted_at IS NULL AND NOT EXISTS(
   SELECT 1 FROM operations previous WHERE previous.workspace_id=w.id AND previous.kind='delete_workspace')))
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.workspace_id=w.id)
 ) SELECT r.*,ri.id AS intent_id FROM resources r
 LEFT JOIN agent_run_reclamation_intents ri ON ri.workspace_id=r.workspace_id AND ri.root_intent_id IS NULL
 WHERE r.tenant_id=$1 AND ($2::uuid IS NULL OR r.project_id=$2) AND ($3::uuid IS NULL OR r.run_id=$3)
 AND ($4::uuid IS NULL OR r.workspace_id=$4) AND ($5::uuid IS NULL OR r.operation_id=$5 OR r.legacy_operation_id=$5)
 AND ($6::uuid IS NULL OR r.reclaim_id=$6) AND ($7::uuid IS NULL OR r.run_id>$7)
 ORDER BY r.run_id LIMIT $8`, r.TenantID, nullable(r.ProjectID), nullable(r.RunID), nullable(r.WorkspaceID), nullable(r.OperationID), nullable(r.ReclaimID), nullable(r.After), limit+1)
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[len(items)-1].S("runId")
	}
	return Object{"items": items, "nextCursor": next}
}

// legacyResidualForWorkspace projects the D8 intent into the shared evidence functions; it does
// not insert a ledger row or change the source or business history.
func legacyResidualForWorkspace(t *transaction, wid string) Object {
	root := controlledRootForWorkspace(t, wid)
	if root == nil || root.S("source") != "legacy_d8" {
		return nil
	}
	return controlledRootRow(t, root)
}

func residualForRun(t *transaction, run string) Object {
	if row := t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", run); row != nil {
		return row
	}
	root := t.one("SELECT * FROM agent_run_reclamation_intents WHERE run_id=$1 AND source='legacy_d8' AND root_intent_id IS NULL", run)
	if root == nil {
		return nil
	}
	return controlledRootRow(t, root)
}
