package core

// PluginExecutionCapability makes the absent production executor explicit.
type PluginExecutionCapability int

const (
	// PluginExecutionUnavailable preserves pending work without authorizing a side effect.
	PluginExecutionUnavailable PluginExecutionCapability = iota
	// PluginExecutionSimulation is only for explicitly configured development test doubles.
	PluginExecutionSimulation
)

// schedulePluginMaintenance runs only database work. Recovery scans and ordinary claims revisit
// every pending target; no network, filesystem or executor is called under the transaction lock.
func schedulePluginMaintenance(t *transaction) {
	rows := t.list("SELECT wi.*,p.space_id,sp.desired_state,sp.desired_version,sp.requested_by_user_id,sp.selected_release FROM workspace_plugin_instances wi JOIN workspaces w ON w.id=wi.workspace_id JOIN projects p ON p.id=w.project_id JOIN space_plugins sp ON sp.space_id=p.space_id AND sp.source_namespace=wi.source_namespace AND sp.identifier=wi.identifier WHERE wi.observed_state='pending' AND wi.maintenance_operation_id IS NULL AND w.deleted_at IS NULL ORDER BY wi.created_at,wi.workspace_id,wi.identifier")
	for _, row := range rows {
		w := t.one("SELECT * FROM workspaces WHERE id=$1", row.S("workspaceId"))
		c := runtimeControl(t, w.S("id"))
		reason := ""
		switch {
		case executionNonAcceptanceConflicted(t, w.S("id")):
			reason = "waiting_idle"
		case row.S("requestedByUserId") == "":
			reason = "legacy_selection_unverified"
		case w.S("observedState") == "stopped":
			reason = "waiting_start"
		case c.S("state") != "idle":
			reason = "waiting_control"
		case w.S("observedState") != "ready" || t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", w.S("projectId")) != nil:
			reason = "waiting_lifecycle"
		case t.one("SELECT id FROM execution_tickets WHERE workspace_id=$1 AND state='active' AND terminated_by_force_stop_id IS NULL", w.S("id")) != nil || t.one("SELECT execution_id FROM clone_executions WHERE workspace_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", w.S("id")) != nil || t.one("SELECT execution_id FROM node_executions WHERE workspace_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", w.S("id")) != nil:
			reason = "waiting_execution"
		}
		t.exec("UPDATE workspace_plugin_instances SET pending_reason=$4 WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3", w.S("id"), row.S("sourceNamespace"), row.S("identifier"), nullable(reason))
		if reason != "" {
			continue
		}
		kind := "install_plugin"
		if row.S("desiredState") == "removed" {
			kind = "remove_plugin"
		}
		req := Object{"pluginId": row.S("sourceNamespace") + "/" + row.S("identifier"), "version": row.S("desiredVersion"), "desiredRevision": row.N("desiredRevision"), "release": row.O("selectedRelease")}
		r := &PublicRequest{TenantID: row.S("tenantId"), Key: "maintenance-" + newID()}
		op := newOperation(t, r, row.S("requestedByUserId"), w.S("projectId"), w.S("id"), kind, "plugin", requestHash("maintenance", w.S("id"), req), req)
		enterPluginStep(t, op)
		reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
		t.exec("UPDATE workspace_plugin_instances SET maintenance_operation_id=$4 WHERE workspace_id=$1 AND source_namespace=$2 AND identifier=$3", w.S("id"), row.S("sourceNamespace"), row.S("identifier"), op.S("id"))
	}
}

// pluginSelection is a shared safe summary; runtime-specific errors and identifiers stay private.
func pluginSelection(t *transaction, sid, namespace, identifier string) Object {
	row := t.spacePluginRow(sid, namespace, identifier)
	if kind := row.O("selectedRelease").S("kind"); kind != "" {
		row["kind"] = kind
	}
	delete(row, "selectedRelease")
	row["installError"] = nil // Shared summaries never expose another runtime's failure output.
	counts := t.one("SELECT count(*) AS affected_count,count(*) FILTER(WHERE wi.observed_state IN ('installed','removed')) AS completed_count,count(*) FILTER(WHERE wi.pending_reason='waiting_start') AS waiting_start_count,count(*) FILTER(WHERE wi.pending_reason IN ('waiting_control','waiting_execution','waiting_lifecycle')) AS waiting_control_count,count(*) FILTER(WHERE wi.pending_reason='executor_capability_unavailable') AS unavailable_count,count(*) FILTER(WHERE wi.observed_state='failed') AS failed_count FROM workspace_plugin_instances wi JOIN workspaces w ON w.id=wi.workspace_id JOIN projects p ON p.id=w.project_id WHERE p.space_id=$1 AND wi.source_namespace=$2 AND wi.identifier=$3 AND w.deleted_at IS NULL", sid, namespace, identifier)
	for key, v := range counts {
		row[key] = v
	}
	return row
}
