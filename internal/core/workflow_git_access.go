package core

// workflowGitAccess grants against the current node attempt, not just the run's running label.
func workflowGitAccess(t *transaction, scope GitRuntimeScope, executionID, projectID string) GitGrant {
	e := t.one(`SELECT e.*,w.creator_user_id,w.tenant_id,r.status AS run_status,r.cancel_requested_at,
 r.delivery_state,r.node_states,r.created_by
 FROM node_executions e JOIN execution_work ew ON ew.id=e.work_id AND ew.workflow_run_id=e.operation_id
 JOIN workspaces w ON w.id=e.workspace_id AND w.workflow_run_id=e.operation_id
 JOIN workflow_runs r ON r.id=w.workflow_run_id AND r.tenant_id=w.tenant_id AND r.project_id=w.project_id
 JOIN projects p ON p.id=w.project_id AND p.tenant_id=w.tenant_id
 WHERE e.execution_id=$1 AND w.id=$2 AND w.tenant_id=$3 AND w.runtime_generation=$4 AND w.project_id=$5
 AND e.kind IN ('agent_session','deliver_revision') AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
 AND r.deleted_at IS NULL AND w.deleted_at IS NULL AND w.desired_state='running' AND w.observed_state='ready'
 AND w.admission_open AND p.deleted_at IS NULL AND p.lifecycle='active'`, executionID, scope.WorkspaceID, scope.TenantID, scope.Generation, projectID)
	require(e != nil, 403, "git_access_denied")
	access := GitGrant{TenantID: scope.TenantID, ProjectID: projectID}
	switch e.S("kind") {
	case "agent_session":
		state := workflowStatesFromJSON(e.O("nodeStates"))[e.S("workflowNodeId")]
		require(e.S("runStatus") == "running" && e["cancelRequestedAt"] == nil && e.S("deliveryState") == "pending" && state.Status == "running" && state.attemptNumber() == e.N("workflowNodeAttempt"), 403, "git_access_denied")
		require(t.one("SELECT id FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='end_session'", e.S("operationId"), e.S("workflowNodeId"), e.N("workflowNodeAttempt")) == nil, 403, "git_access_denied")
		access.Writable = []GitWritableRef{{Prefix: GitWorkspaceRefPrefix(scope.WorkspaceID), Rule: GitFastForward}}
	case "deliver_revision":
		require(e.S("deliveryState") == "delivering" && e.O("input").O("workflow").S("workflowRunId") == e.S("operationId"), 403, "git_access_denied")
		input := e.O("input")
		require(input.S("deliveryRef") == GitDeliveryRefPrefix+e.S("workId") && gitRepositoryURLNames(input.S("gitRepositoryUrl"), scope.TenantID, projectID), 403, "git_access_denied")
		access.Writable = []GitWritableRef{{Ref: input.S("deliveryRef"), Rule: GitCreateOnly}}
		if ref, ok := ParseGitRef(input.S("workspaceRef")); ok && ref.Namespace == GitWorkspaceBranch && ref.Owner == scope.WorkspaceID {
			access.Writable = append(access.Writable, GitWritableRef{Ref: input.S("workspaceRef"), Rule: GitFastForward})
		}
	default:
		reject(403, "git_access_denied")
	}
	require(runtimeUsable(t, e, e.S("createdBy")), 403, "git_access_denied")
	requireNoForceStop(t, scope.WorkspaceID)
	node := currentNode(t, scope.WorkspaceID)
	c := runtimeControl(t, scope.WorkspaceID)
	work := t.one("SELECT target FROM execution_work WHERE id=$1", e.S("workId"))
	require(node.S("nodeId") == e.S("nodeId") && work.O("target").S("sandboxInstanceId") == node.S("sandboxInstanceId"), 403, "git_access_denied")
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == e.S("operationId") && c.B("bindingConfirmed") && c.S("boundSandboxId") == node.S("sandboxInstanceId"), 403, "git_access_denied")
	return access
}
