package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// gitScene is one run whose session execution is registered on a live, acknowledged runtime.
type gitScene struct {
	takeoverScene
	scope   GitRuntimeScope
	project string
}

// seedGitScene drives a run to a registered session execution and acknowledges its runtime binding
// through the real runtime_ack path, which is the same evidence RuntimePermit and model grants use.
func seedGitScene(t *testing.T, s *Store) gitScene {
	t.Helper()
	scene := seedTakeoverScene(t, s)
	_, err := s.transact(context.Background(), func(tx *transaction) Object {
		c := runtimeControl(tx, scene.ws)
		n := currentNode(tx, scene.ws)
		return runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{"workspaceId": scene.ws, "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "nodeInstanceId": n.S("id"), "inputClosed": false, "unfinishedExecutionIds": []any{}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := s.Pool.QueryRow("SELECT runtime_generation FROM workspaces WHERE id=$1", scene.ws).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return gitScene{takeoverScene: scene, scope: GitRuntimeScope{TenantID: scene.seed.seed.tenant, WorkspaceID: scene.ws, Generation: generation}, project: scene.seed.seed.project}
}

// delivering stages the rows a delivery leaves after the session ended: the session execution has
// a result, the run is `delivering`, and a deliver_revision execution is registered for a work item
// targeting the same sandbox, with the delivery ref and Workspace branch its fixed input names. It
// returns the delivery execution and its work item id.
func (g gitScene) delivering(t *testing.T, s *Store) (execution, work string) {
	t.Helper()
	execution, work = "exec-deliver-"+g.execution, uuid.NewString()
	input := jsonText(Object{"kind": "deliver_revision", "deliveryRef": GitDeliveryRefPrefix + work, "workspaceRef": GitWorkspaceRefPrefix(g.ws) + "main"})
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`UPDATE node_executions SET result='{"outcome":"agent_session_ended"}' WHERE execution_id=$1`, []any{g.execution}},
		{`UPDATE issue_runs SET phase='delivering' WHERE id=$1`, []any{g.run}},
		{`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) SELECT $1,run_id,'deliver_revision',$4,target,$2 FROM execution_work WHERE id=$3`, []any{work, execution, g.work.S("id"), input}},
		{`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch) VALUES($1,'deliver_revision',$2,$3,$4,$5,'op-deliver',$6,1)`, []any{execution, g.run, work, g.ws, g.node, input}},
	} {
		if _, err := s.Pool.Exec(q.query, q.args...); err != nil {
			t.Fatalf("stage delivery: %v", err)
		}
	}
	return execution, work
}

func expectGitDenied(t *testing.T, err error, why string) {
	t.Helper()
	var fault *Fault
	if !errors.As(err, &fault) || fault.Status != 403 || fault.Code != "git_access_denied" {
		t.Fatalf("%s: error=%v, want uniform 403 git_access_denied", why, err)
	}
}

// Contract D5: every live session reads the repository and may create or fast-forward branches of
// its own Workspace, restoring a prior or not; a delivery creates its own ref and fast-forwards its
// Workspace's branch; an ended session keeps nothing.
func TestGitAccessGrantsSessionBranchesAndDeliveryRefsFromCurrentState(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	ctx := context.Background()

	got, err := s.AuthorizeGitAccess(ctx, g.scope, g.execution, g.project)
	if err != nil {
		t.Fatal(err)
	}
	session := GitGrant{TenantID: g.scope.TenantID, ProjectID: g.project, Writable: []GitWritableRef{{Prefix: GitWorkspaceRefPrefix(g.ws), Rule: GitFastForward}}}
	if !reflect.DeepEqual(got, session) {
		t.Fatalf("session access = %+v, want its own Workspace prefix fast-forward", got)
	}

	delivery, work := g.delivering(t, s)
	_, err = s.AuthorizeGitAccess(ctx, g.scope, g.execution, g.project)
	expectGitDenied(t, err, "an ended session keeps no repository access")
	got, err = s.AuthorizeGitAccess(ctx, g.scope, delivery, g.project)
	if err != nil {
		t.Fatal(err)
	}
	want := GitGrant{TenantID: g.scope.TenantID, ProjectID: g.project, Writable: []GitWritableRef{
		{Ref: GitDeliveryRefPrefix + work, Rule: GitCreateOnly},
		{Ref: GitWorkspaceRefPrefix(g.ws) + "main", Rule: GitFastForward},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery access = %+v, want its own delivery ref and Workspace branch", got)
	}
}

// A delivery input naming another Workspace's branch, or a delivery ref that is not its own, grants
// no write beyond the contract's sandbox column.
func TestGitAccessNeverWidensBeyondTheCertificateWorkspace(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	delivery, work := g.delivering(t, s)
	if _, err := s.Pool.Exec(`UPDATE node_executions SET input=jsonb_set(input,'{workspaceRef}',to_jsonb($2::text)) WHERE execution_id=$1`, delivery, GitWorkspaceRefPrefix(uuid.NewString())+"main"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuthorizeGitAccess(context.Background(), g.scope, delivery, g.project)
	if err != nil {
		t.Fatal(err)
	}
	if want := []GitWritableRef{{Ref: GitDeliveryRefPrefix + work, Rule: GitCreateOnly}}; !reflect.DeepEqual(got.Writable, want) {
		t.Fatalf("another Workspace's branch was granted: %+v", got.Writable)
	}
	if _, err := s.Pool.Exec(`UPDATE node_executions SET input=jsonb_set(input,'{deliveryRef}',to_jsonb($2::text)) WHERE execution_id=$1`, delivery, GitDeliveryRefPrefix+uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeGitAccess(context.Background(), g.scope, delivery, g.project)
	expectGitDenied(t, err, "a delivery ref that is not the work item's own")
}

// Each case withdraws exactly one fact the decision depends on; the next request must be denied,
// with the same fault whichever gate refused.
func TestGitAccessDeniesWhenAnyCurrentFactIsWithdrawn(t *testing.T) {
	type mutation struct {
		name  string
		scope func(GitRuntimeScope) GitRuntimeScope
		args  func(g gitScene, delivery string) (execution, project string)
		query string
	}
	same := func(g gitScene, delivery string) (string, string) { return delivery, g.project }
	keep := func(s GitRuntimeScope) GitRuntimeScope { return s }
	for _, m := range []mutation{
		{name: "stale certificate generation", scope: func(s GitRuntimeScope) GitRuntimeScope { s.Generation--; return s }, args: same},
		{name: "future certificate generation", scope: func(s GitRuntimeScope) GitRuntimeScope { s.Generation++; return s }, args: same},
		{name: "certificate for another tenant", scope: func(s GitRuntimeScope) GitRuntimeScope { s.TenantID = uuid.NewString(); return s }, args: same},
		{name: "certificate for another workspace", scope: func(s GitRuntimeScope) GitRuntimeScope { s.WorkspaceID = uuid.NewString(); return s }, args: same},
		{name: "another project", scope: keep, args: func(g gitScene, d string) (string, string) { return d, uuid.NewString() }},
		{name: "unknown execution", scope: keep, args: func(g gitScene, _ string) (string, string) { return "exec-unknown", g.project }},
		{name: "execution finished", scope: keep, args: same, query: `UPDATE node_executions SET result='{"outcome":"revision_delivered"}' WHERE kind='deliver_revision' AND operation_id=$1`},
		{name: "run left delivering", scope: keep, args: same, query: `UPDATE issue_runs SET phase='releasing' WHERE id=$1`},
		{name: "run deleted", scope: keep, args: same, query: `UPDATE issue_runs SET deleted_at=now() WHERE id=$1`},
		{name: "trigger membership disabled", scope: keep, args: same, query: `UPDATE tenant_memberships SET status='disabled' WHERE (tenant_id,user_id)=(SELECT w.tenant_id,w.creator_user_id FROM workspaces w WHERE w.issue_run_id=$1)`},
		{name: "trigger account disabled", scope: keep, args: same, query: `UPDATE users SET status='disabled' WHERE id=(SELECT creator_user_id FROM workspaces WHERE issue_run_id=$1)`},
		{name: "Node heartbeat stale", scope: keep, args: same, query: `UPDATE node_instances SET last_seen_at=now()-interval '1 minute' WHERE workspace_id=(SELECT id FROM workspaces WHERE issue_run_id=$1)`},
		{name: "sandbox terminated", scope: keep, args: same, query: `UPDATE sandbox_instances SET observed_state='terminated',terminated_at=now() WHERE workspace_id=(SELECT id FROM workspaces WHERE issue_run_id=$1)`},
		{name: "admission closed", scope: keep, args: same, query: `UPDATE workspaces SET admission_open=false WHERE issue_run_id=$1`},
		{name: "runtime binding unconfirmed", scope: keep, args: same, query: `UPDATE runtime_controls SET binding_confirmed=false WHERE workspace_id=(SELECT id FROM workspaces WHERE issue_run_id=$1)`},
		{name: "project deleting", scope: keep, args: same, query: `UPDATE projects SET lifecycle='deleting' WHERE id=(SELECT project_id FROM workspaces WHERE issue_run_id=$1)`},
	} {
		t.Run(m.name, func(t *testing.T) {
			s := commandStore(t)
			g := seedGitScene(t, s)
			delivery, _ := g.delivering(t, s)
			if _, err := s.AuthorizeGitAccess(context.Background(), g.scope, delivery, g.project); err != nil {
				t.Fatalf("the unmodified delivery must be authorized: %v", err)
			}
			if m.query != "" {
				// A second active administrator keeps the last-administrator guard satisfied
				// when the trigger's own membership or account is withdrawn.
				backup := uuid.NewString()
				if _, err := s.Pool.Exec("INSERT INTO users(id,display_name,status) VALUES($1,'backup','active')", backup); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", g.scope.TenantID, backup); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Pool.Exec(m.query, g.run); err != nil {
					t.Fatal(err)
				}
			}
			execution, project := m.args(g, delivery)
			_, err := s.AuthorizeGitAccess(context.Background(), m.scope(g.scope), execution, project)
			expectGitDenied(t, err, m.name)
		})
	}
}

// A session loses repository access as soon as the run starts ending.
func TestGitAccessSessionAccessEndsWithTheSession(t *testing.T) {
	for _, query := range []string{
		"UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1",
		"UPDATE issue_runs SET thread_state='ending' WHERE id=$1",
	} {
		s := commandStore(t)
		g := seedGitScene(t, s)
		if _, err := s.Pool.Exec(query, g.run); err != nil {
			t.Fatal(err)
		}
		_, err := s.AuthorizeGitAccess(context.Background(), g.scope, g.execution, g.project)
		expectGitDenied(t, err, query)
	}
}

// Plugin executions of the same Workspace never reach the repository.
func TestGitAccessDeniesPluginExecutions(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	res, err := s.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
	 SELECT 'exec-plugin','install_plugins',o.id,$1,$2,'op-plugin','{"kind":"install_plugins","plugins":[]}',1
	 FROM operations o WHERE o.workspace_id=$1 LIMIT 1`, g.ws, g.node)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatal("the scene has no Workspace operation to register a plugin execution under")
	}
	_, err = s.AuthorizeGitAccess(context.Background(), g.scope, "exec-plugin", g.project)
	expectGitDenied(t, err, "a plugin execution")
}

// An unreachable database is not a denial: the caller must report unavailable and never allow.
func TestGitAccessDatabaseFailureIsNotADenial(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	if err := s.Pool.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.AuthorizeGitAccess(context.Background(), g.scope, g.execution, g.project)
	var fault *Fault
	if err == nil || errors.As(err, &fault) {
		t.Fatalf("database failure = %v, want a non-fault error", err)
	}
}
