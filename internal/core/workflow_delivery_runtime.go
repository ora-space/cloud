package core

// workflowDeliveryRuntimeResume validates unfinished responsibility, never new mutation admission.
// The binding retains the same maintenance run, Node, sandbox and generation after a Node restart.
func workflowDeliveryRuntimeResume(t *transaction, c, n Object, unfinished []any) bool {
	if len(unfinished) == 0 || c.S("state") != "maintenance" || c.S("maintenanceRunId") == "" {
		return false
	}
	seen := map[string]bool{}
	for _, value := range unfinished {
		id, ok := value.(string)
		if !ok || seen[id] {
			return false
		}
		seen[id] = true
		e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND workspace_id=$3 AND kind='deliver_revision' AND result IS NULL AND terminated_by_force_stop_id IS NULL", id, c.S("maintenanceRunId"), c.S("workspaceId"))
		if e == nil || e.O("input").O("workflow").S("workflowRunId") != c.S("maintenanceRunId") || e.S("nodeId") != n.S("nodeId") {
			return false
		}
		work := t.one("SELECT * FROM execution_work WHERE id=$1 AND workflow_run_id=$2 AND execution_id=$3", e.S("workId"), c.S("maintenanceRunId"), id)
		if work == nil || work.O("target").S("sandboxInstanceId") != n.S("sandboxInstanceId") || jsonText(work.O("input")) != jsonText(e.O("input")) {
			return false
		}
		workflowDeliveryIdentity(t, e)
		if t.one("SELECT id FROM workflow_runs WHERE id=$1 AND delivery_state='delivering' AND deleted_at IS NULL", c.S("maintenanceRunId")) == nil {
			return false
		}
	}
	return true
}
