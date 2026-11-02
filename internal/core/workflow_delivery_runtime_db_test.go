package core

import "testing"

// A reconnecting Node can report only its original workflow delivery as unfinished. Ordinary
// input admission and foreign execution identities retain the closed-slot rule.
func TestWorkflowDeliveryRuntimeReattachesOnlyItsOriginalExecution(t *testing.T) {
	s := dispatcherDB(t)
	scene := seedWorkflowDelivery(t, s)
	input := scene.work.O("input")
	execution := "resume-delivery"
	if _, err := s.transact(t.Context(), func(tx *transaction) Object {
		return nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "nodeId": scene.node, "input": input, "epoch": int64(1)}})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE runtime_controls SET control_epoch=control_epoch+1,binding_confirmed=false,bound_sandbox_id=NULL,version=version+1 WHERE workspace_id=$1", scene.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE workspaces SET observed_state='unavailable',admission_open=false WHERE id=$1", scene.workspace); err != nil {
		t.Fatal(err)
	}
	ack := func(ids []any) error {
		_, err := s.transact(t.Context(), func(tx *transaction) Object {
			c := runtimeControl(tx, scene.workspace)
			n := currentNode(tx, scene.workspace)
			return runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{"workspaceId": scene.workspace, "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "nodeInstanceId": n.S("id"), "inputClosed": false, "unfinishedExecutionIds": ids}})
		})
		return err
	}
	for _, ids := range [][]any{{scene.first}, {"unknown"}, {execution, execution}} {
		if err := ack(ids); ErrorCode(err).Code != "resource_in_use" {
			t.Fatalf("foreign or duplicate liabilities accepted: %v err=%v", ids, err)
		}
	}
	if err := ack([]any{execution}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var project, tenant string
	if err := s.Pool.QueryRow("SELECT runtime_generation,project_id,tenant_id FROM workspaces WHERE id=$1", scene.workspace).Scan(&generation, &project, &tenant); err != nil {
		t.Fatal(err)
	}
	grant, err := s.AuthorizeGitAccess(t.Context(), GitRuntimeScope{WorkspaceID: scene.workspace, TenantID: tenant, Generation: generation}, execution, project)
	if err != nil || len(grant.Writable) == 0 || grant.Writable[0].Ref != input.S("deliveryRef") {
		t.Fatalf("retained delivery denied: %+v err=%v", grant, err)
	}
	_, err = s.AuthorizeGitAccess(t.Context(), GitRuntimeScope{WorkspaceID: scene.workspace, TenantID: tenant, Generation: generation}, scene.first, project)
	expectGitDenied(t, err, "ended session remains revoked while delivery resumes")
}
