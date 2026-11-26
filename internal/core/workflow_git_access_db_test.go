package core

import (
	"reflect"
	"testing"
)

// Authorization is re-read for each request and rejects a stale attempt even on a running run.
func TestWorkflowGitAccessFencesAttemptCancelAndIndependentDelivery(t *testing.T) {
	s := dispatcherDB(t)
	run, tenant, _, _, project := seedWorkflowSessionScene(t, s)
	if err := s.AdvanceWorkflowRunsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace := settleSessionWorkspace(t, s, run)
	node, spec := startSessionNode(t, s, run, workspace, "a")
	execution := dispatchSession(t, s, run, "a", node, spec)
	if _, err := s.Pool.Exec("UPDATE runtime_controls SET binding_confirmed=true,bound_sandbox_id=(SELECT sandbox_instance_id FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL) WHERE workspace_id=$1", workspace); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := s.Pool.QueryRow("SELECT runtime_generation FROM workspaces WHERE id=$1", workspace).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	scope := GitRuntimeScope{TenantID: tenant, WorkspaceID: workspace, Generation: generation}
	want := GitGrant{TenantID: tenant, ProjectID: project, Writable: []GitWritableRef{{Prefix: GitWorkspaceRefPrefix(workspace), Rule: GitFastForward}}}
	grant, err := s.AuthorizeGitAccess(t.Context(), scope, execution, project)
	if err != nil || !reflect.DeepEqual(grant, want) {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if _, err := s.Pool.Exec("UPDATE workflow_runs SET node_states=jsonb_set(node_states,'{a,attempt}','2') WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeGitAccess(t.Context(), scope, execution, project)
	expectGitDenied(t, err, "previous attempt")
	if _, err := s.Pool.Exec("UPDATE workflow_runs SET node_states=jsonb_set(node_states,'{a,attempt}','1'),cancel_requested_at=now() WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeGitAccess(t.Context(), scope, execution, project)
	expectGitDenied(t, err, "cancel immediately revokes session")
	old := scope
	old.Generation--
	_, err = s.AuthorizeGitAccess(t.Context(), old, execution, project)
	expectGitDenied(t, err, "old generation")
}
