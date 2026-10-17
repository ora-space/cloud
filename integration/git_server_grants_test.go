package integration

// Contract D5 (specs decisions/cloud/controller-integration/20261016-snapshot-clone-input-and-
// execution-ref-grants.md) through a real git client and a real Git Server: a clone execution reads
// for exactly as long as its operation is in the clone step and writes nothing; an Agent session
// reads the whole Project repository and creates or fast-forwards only its own Workspace's branches,
// until the session ends; a certificate of an earlier sandbox generation is refused even when every
// other condition holds.

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

func TestGitServerGrantsACloneExecutionReadOnlyDuringItsStep(t *testing.T) {
	f := setup(t)
	created := f.create("clone-grant")
	pid, wid, oid := created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
	at := f.stepTo(oid, "clone")
	frozen := f.frozenClone(oid)
	var node string
	var generation int64
	must(t, f.store.Pool.QueryRow(`SELECT n.node_id,w.runtime_generation FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id JOIN workspaces w ON w.id=s.workspace_id WHERE w.id=$1 AND n.ended_at IS NULL`, wid).Scan(&node, &generation))
	ctx := asController(f.client.Subject)
	_, err := f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "clone-grant", Epoch: at.N("epoch"), OperationId: oid, ExecutionId: "clone-grant-1", NodeId: node, Input: cloneInputSpec(frozen)})
	must(t, err)

	server := startGitServer(t, f, f.git.Root)
	server.sandbox(t, "main", f.tid, wid, generation)
	remote := server.remote(f.tid, pid)
	// The main Workspace clones while its Project is still provisioning.
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='provisioning'", pid) != 1 {
		t.Fatal("the scene must clone during create_project")
	}
	listed, err := server.git(t.TempDir(), "main", "clone-grant-1", "ls-remote", remote)
	if err != nil || !strings.Contains(listed, frozen.S("snapshotRef")) {
		t.Fatalf("the clone execution must read the snapshot during its step: %v %s", err, listed)
	}
	checkout := t.TempDir()
	if out, err := server.git(checkout, "main", "", "init", "--quiet"); err != nil {
		t.Fatal(out)
	}
	if out, err := server.git(checkout, "main", "clone-grant-1", "fetch", "--quiet", remote, frozen.S("snapshotRef")); err != nil {
		t.Fatalf("the clone execution must fetch its snapshot: %s", out)
	}
	if out, err := server.git(checkout, "main", "clone-grant-1", "push", remote, "FETCH_HEAD:"+frozen.S("workspaceRef")); err == nil {
		t.Fatalf("a clone execution pushed: %s", out)
	}
	if got := serverRef(t, f.git.Root, f.tid, pid, frozen.S("workspaceRef")); got != "" {
		t.Fatalf("a refused push left %s at %s", frozen.S("workspaceRef"), got)
	}

	// The clone succeeds and the operation leaves the clone step: the execution reads no more.
	ready := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: "inc"}, Outcome: &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: "/checkout", Commit: frozen.S("baseCommit")}}}
	_, err = f.executions.RecordQueriedResult(ctx, &controlpb.RecordQueriedResultRequest{SubmissionId: "clone-grant-result", Epoch: at.N("epoch"), OperationId: oid, ExecutionId: "clone-grant-1", Result: ready})
	must(t, err)
	if out, err := server.git(t.TempDir(), "main", "clone-grant-1", "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("an execution with a result still read the repository: %v %s", err, out)
	}
	_, err = f.store.Pool.Exec(`UPDATE clone_executions SET result=NULL WHERE execution_id='clone-grant-1'`)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE operations SET step='plugin' WHERE id=$1`, oid)
	must(t, err)
	if out, err := server.git(t.TempDir(), "main", "clone-grant-1", "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("an execution whose operation left the clone step still read the repository: %v %s", err, out)
	}
}

// Contract D5: a clone execution's read is decided again on every request. It ends when the
// operation's actor can no longer use the runtime, when a force stop is pending on the Workspace or
// has terminated the execution, and it is never granted for an input naming another repository.
func TestGitServerDeniesACloneExecutionItsRuntimeNoLongerAllows(t *testing.T) {
	f := setup(t)
	created := f.create("clone-denials")
	pid, wid, oid := created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
	at := f.stepTo(oid, "clone")
	frozen := f.frozenClone(oid)
	var node, actor string
	var generation int64
	must(t, f.store.Pool.QueryRow(`SELECT n.node_id,w.runtime_generation FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id JOIN workspaces w ON w.id=s.workspace_id WHERE w.id=$1 AND n.ended_at IS NULL`, wid).Scan(&node, &generation))
	must(t, f.store.Pool.QueryRow(`SELECT actor_user_id FROM operations WHERE id=$1`, oid).Scan(&actor))
	_, err := f.executions.RecordDispatch(asController(f.client.Subject), &controlpb.RecordDispatchRequest{SubmissionId: "clone-denials", Epoch: at.N("epoch"), OperationId: oid, ExecutionId: "clone-denials-1", NodeId: node, Input: cloneInputSpec(frozen)})
	must(t, err)
	scope := core.GitRuntimeScope{TenantID: f.tid, WorkspaceID: wid, Generation: generation}
	authorize := func() error {
		_, err := f.store.AuthorizeGitAccess(t.Context(), scope, "clone-denials-1", pid)
		return err
	}
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := f.store.Pool.Exec(q, args...)
		must(t, err)
	}
	if grant, err := f.store.AuthorizeGitAccess(t.Context(), scope, "clone-denials-1", pid); err != nil || len(grant.Writable) != 0 {
		t.Fatalf("the clone execution must read, and only read, during its step: %v %v", grant, err)
	}
	// A second administrator lets the tenant stay active while the actor's account is disabled.
	admin := uuid.NewString()
	exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Second admin','active')`, admin)
	exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, f.tid, admin)
	stop := uuid.NewString()
	for _, c := range []struct {
		name          string
		apply, revert func()
	}{
		{
			"the actor's account is disabled",
			func() { exec(`UPDATE users SET status='disabled' WHERE id=$1`, actor) },
			func() { exec(`UPDATE users SET status='active' WHERE id=$1`, actor) },
		},
		{
			"a force stop is pending on the Workspace",
			func() {
				exec(`INSERT INTO runtime_force_stops(id,tenant_id,workspace_id,actor_user_id,reason,state,control_epoch,runtime_generation) VALUES($1,$2,$3,$4,'stop','registered',0,$5)`, stop, f.tid, wid, actor, generation)
			},
			func() { exec(`UPDATE runtime_force_stops SET state='succeeded',confirmed_at=now() WHERE id=$1`, stop) },
		},
		{
			"a force stop terminated the execution",
			func() {
				exec(`UPDATE clone_executions SET terminated_by_force_stop_id=$2 WHERE execution_id=$1`, "clone-denials-1", stop)
			},
			func() {
				exec(`UPDATE clone_executions SET terminated_by_force_stop_id=NULL WHERE execution_id=$1`, "clone-denials-1")
			},
		},
		{
			"the input names another repository",
			func() {
				exec(`UPDATE clone_executions SET input=jsonb_set(input,'{repositoryUrl}',to_jsonb($2::text)) WHERE execution_id=$1`, "clone-denials-1", simulator.GitServerBaseURL+"/"+f.tid+"/"+uuid.NewString()+".git")
			},
			func() {
				exec(`UPDATE clone_executions SET input=jsonb_set(input,'{repositoryUrl}',to_jsonb($2::text)) WHERE execution_id=$1`, "clone-denials-1", frozen.S("repositoryUrl"))
			},
		},
	} {
		c.apply()
		var fault *core.Fault
		if err := authorize(); !errors.As(err, &fault) || fault.Code != "git_access_denied" {
			t.Fatalf("%s: the clone execution must be denied, got %v", c.name, err)
		}
		c.revert()
		if err := authorize(); err != nil {
			t.Fatalf("%s: undoing it must restore the read, got %v", c.name, err)
		}
	}
}

// sessionGitScene is a live Agent session execution on a run Workspace whose Project repository is
// served by the scene's Git Server.
type sessionGitScene struct {
	server                                   *gitServerHarness
	run, execution, workspace, project, base string
	remote                                   string
}

func newSessionGitScene(t *testing.T, f *fixture) sessionGitScene {
	t.Helper()
	run, execution, _, _ := f.registeredSession(t, "session-grant")
	var wid, pid string
	var generation int64
	must(t, f.store.Pool.QueryRow(`SELECT w.id,w.project_id,w.runtime_generation FROM workspaces w WHERE w.issue_run_id=$1`, run).Scan(&wid, &pid, &generation))
	_, err := f.store.Pool.Exec(`UPDATE issue_runs SET phase='running',status='running' WHERE id=$1`, run)
	must(t, err)
	// The sandbox is the Workspace's second generation, so a certificate of the first one is a
	// well-formed certificate of a replaced sandbox rather than a malformed one.
	_, err = f.store.Pool.Exec(`UPDATE sandbox_instances SET generation=generation+1 WHERE workspace_id=$1 AND terminated_at IS NULL`, wid)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE workspaces SET runtime_generation=runtime_generation+1 WHERE id=$1`, wid)
	must(t, err)
	generation++
	server := startGitServer(t, f, f.git.Root)
	server.sandbox(t, "run", f.tid, wid, generation)
	server.sandbox(t, "stale", f.tid, wid, generation-1)
	return sessionGitScene{server: server, run: run, execution: execution, workspace: wid, project: pid, base: f.commit, remote: server.remote(f.tid, pid)}
}

func TestGitServerGrantsASessionItsOwnWorkspaceBranches(t *testing.T) {
	f := setup(t)
	s := newSessionGitScene(t, f)
	git := s.server.git
	checkout := t.TempDir()
	if out, err := git(checkout, "run", "", "init", "--quiet"); err != nil {
		t.Fatal(out)
	}
	// Every session reads the whole Project repository, whether or not it resumes a Revision.
	listed, err := git(checkout, "run", s.execution, "ls-remote", s.remote)
	if err != nil || !strings.Contains(listed, "refs/ora/upstreams/") {
		t.Fatalf("a session must read the Project repository: %v %s", err, listed)
	}
	if out, err := git(checkout, "run", s.execution, "fetch", "--quiet", s.remote, "refs/ora/upstreams/*:refs/remotes/ora/upstreams/*"); err != nil {
		t.Fatalf("fetch upstream: %s", out)
	}
	if out, err := git(checkout, "run", "", "checkout", "--quiet", "-B", "main", s.base); err != nil {
		t.Fatal(out)
	}
	if out, err := git(checkout, "run", "", "commit", "--quiet", "--allow-empty", "-m", "session work"); err != nil {
		t.Fatal(out)
	}
	own := core.GitWorkspaceRefPrefix(s.workspace) + "main"
	if out, err := git(checkout, "run", s.execution, "push", s.remote, "HEAD:"+own); err != nil {
		t.Fatalf("a session must create its own Workspace branch: %s", out)
	}
	if out, err := git(checkout, "run", s.execution, "push", s.remote, "HEAD:"+core.GitWorkspaceRefPrefix(s.workspace)+"attempt"); err != nil {
		t.Fatalf("a session may open more branches under its own prefix: %s", out)
	}
	project := s.project
	pushed := serverRef(t, f.git.Root, f.tid, project, own)

	// Another Workspace's branch, an upstream ref, a delivery ref or a ref outside the namespaces: refused.
	other := core.GitWorkspaceRefPrefix(uuid.NewString()) + "main"
	for _, ref := range []string{other, "refs/ora/upstreams/" + uuid.NewString() + "/heads/main", core.GitDeliveryRefPrefix + uuid.NewString(), "refs/heads/main"} {
		if out, err := git(checkout, "run", s.execution, "push", s.remote, "HEAD:"+ref); err == nil {
			t.Fatalf("a session wrote %s: %s", ref, out)
		}
	}
	// A non-fast-forward of its own branch is refused and leaves the branch where it was.
	if out, err := git(checkout, "run", s.execution, "push", s.remote, "+"+s.base+":"+own); err == nil {
		t.Fatalf("a non-fast-forward push was accepted: %s", out)
	}
	if got := serverRef(t, f.git.Root, f.tid, project, own); got != pushed {
		t.Fatalf("a refused push moved %s to %s", own, got)
	}
	if got := serverRef(t, f.git.Root, f.tid, project, other); got != "" {
		t.Fatalf("a refused push created %s", other)
	}

	// A certificate of an earlier generation of this very Workspace is refused, every other condition
	// still holding.
	if out, err := git(t.TempDir(), "stale", s.execution, "ls-remote", s.remote); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("a stale-generation certificate reached the repository: %v %s", err, out)
	}

	// The session ending ends its access on the next request.
	_, err = f.store.Pool.Exec(`UPDATE node_executions SET result='{"outcome":"agent_session_ended"}' WHERE execution_id=$1`, s.execution)
	must(t, err)
	if out, err := git(checkout, "run", s.execution, "push", s.remote, "HEAD:"+own); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("an ended session still pushed: %v %s", err, out)
	}
	if got := serverRef(t, f.git.Root, f.tid, project, own); got != pushed {
		t.Fatalf("an ended session moved %s", own)
	}
}
