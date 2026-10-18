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

// restoring marks the session execution as one that restores a prior Revision.
func (g gitScene) restoring(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Pool.Exec(`UPDATE node_executions SET input=input||'{"priorRevision":{"revisionId":"r","finalCommit":"c"}}' WHERE execution_id=$1`, g.execution); err != nil {
		t.Fatal(err)
	}
}

// delivering stages the rows a delivery leaves after the session ended: the session execution has
// a result, the run is `delivering`, and a deliver_revision execution is registered for a work item
// targeting the same sandbox. It returns the delivery execution and its work item id.
func (g gitScene) delivering(t *testing.T, s *Store) (execution, work string) {
	t.Helper()
	execution, work = "exec-deliver-"+g.execution, uuid.NewString()
	for _, q := range []struct {
		query string
		args  []any
	}{
		{`UPDATE node_executions SET result='{"outcome":"agent_session_ended"}' WHERE execution_id=$1`, []any{g.execution}},
		{`UPDATE issue_runs SET phase='delivering' WHERE id=$1`, []any{g.run}},
		{`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) SELECT $1,run_id,'deliver_revision','{}',target,$2 FROM execution_work WHERE id=$3`, []any{work, execution, g.work.S("id")}},
		{`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch) VALUES($1,'deliver_revision',$2,$3,$4,$5,'op-deliver','{}',1)`, []any{execution, g.run, work, g.ws, g.node}},
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

func TestGitAccessGrantsRestoreReadAndDeliveryWriteFromCurrentState(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	ctx := context.Background()

	_, err := s.AuthorizeGitAccess(ctx, g.scope, g.execution, g.project)
	expectGitDenied(t, err, "a session that restores nothing has no repository access")

	g.restoring(t, s)
	got, err := s.AuthorizeGitAccess(ctx, g.scope, g.execution, g.project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, GitGrant{TenantID: g.scope.TenantID, ProjectID: g.project}) {
		t.Fatalf("restoring session access = %+v, want read-only", got)
	}

	delivery, work := g.delivering(t, s)
	_, err = s.AuthorizeGitAccess(ctx, g.scope, g.execution, g.project)
	expectGitDenied(t, err, "an ended session keeps no repository access")
	got, err = s.AuthorizeGitAccess(ctx, g.scope, delivery, g.project)
	if err != nil {
		t.Fatal(err)
	}
	if want := (GitGrant{TenantID: g.scope.TenantID, ProjectID: g.project, Writable: []GitWritableRef{{Ref: GitDeliveryRefPrefix + work, Rule: GitCreateOnly}}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery access = %+v, want write to its own delivery ref", got)
	}
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

// The restoring session loses read access as soon as the run starts ending.
func TestGitAccessRestoreReadEndsWithTheSession(t *testing.T) {
	for _, query := range []string{
		"UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1",
		"UPDATE issue_runs SET thread_state='ending' WHERE id=$1",
	} {
		s := commandStore(t)
		g := seedGitScene(t, s)
		g.restoring(t, s)
		if _, err := s.Pool.Exec(query, g.run); err != nil {
			t.Fatal(err)
		}
		_, err := s.AuthorizeGitAccess(context.Background(), g.scope, g.execution, g.project)
		expectGitDenied(t, err, query)
	}
}

// An unreachable database is not a denial: the caller must report unavailable and never allow.
func TestGitAccessDatabaseFailureIsNotADenial(t *testing.T) {
	s := commandStore(t)
	g := seedGitScene(t, s)
	g.restoring(t, s)
	if err := s.Pool.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := s.AuthorizeGitAccess(context.Background(), g.scope, g.execution, g.project)
	var fault *Fault
	if err == nil || errors.As(err, &fault) {
		t.Fatalf("database failure = %v, want a non-fault error", err)
	}
}
