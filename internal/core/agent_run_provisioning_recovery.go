package core

// pendingProvisioningEffects reads durable D5 query responsibilities independently of active
// operation claims. A provisional or missing receipt is not completion: only a terminal fact
// acknowledged by Cloud removes the item. The original effect request is for GET-by-ID only;
// this read neither grants a permit nor changes the failed operation's dispatch eligibility.
func pendingProvisioningEffects(t *transaction, r *ControlRequest) Object {
	after := r.Body.S("afterEffectId")
	require(after == "" || validID(after), 400, "invalid_effect_cursor")
	limit := r.Body.N("pageSize")
	if limit == 0 {
		limit = 100
	}
	require(limit >= 1 && limit <= 100, 400, "invalid_page_size")
	rows := t.list(`SELECT e.* FROM external_effects e
	 JOIN operations o ON o.id=e.operation_id AND o.project_id=e.project_id
	 JOIN workspaces w ON w.id=o.workspace_id AND w.tenant_id=o.tenant_id
	 JOIN agent_run_residuals ar ON ar.workspace_id=w.id AND ar.run_id=w.issue_run_id AND ar.tenant_id=w.tenant_id AND ar.project_id=w.project_id
	 WHERE o.kind='create_workspace' AND o.state='failed' AND o.error_code='provisioning_result_unknown'
	 AND ar.reason='workspace_unavailable' AND ar.cleared_at IS NULL
	 AND e.kind IN ('sandbox_ensure','sandbox_terminate','workspace_data_delete')
	 AND (e.workspace_id IS NULL OR e.workspace_id=w.id)
	 AND COALESCE(ar.responsibility->'lateFacts'->('effect:'||e.id::text)->>'state','') NOT IN ('succeeded','failed')
	 AND ($1='' OR e.id>NULLIF($1,'')::uuid)
	 ORDER BY e.id LIMIT $2`, after, limit+1)
	next := ""
	if int64(len(rows)) > limit {
		rows = rows[:limit]
		next = rows[len(rows)-1].S("id")
	}
	items := make([]Object, 0, len(rows))
	for _, e := range rows {
		items = append(items, Object{"operationId": e.S("operationId"), "effect": e})
	}
	return Object{"items": items, "nextAfterEffectId": next}
}
