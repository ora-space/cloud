package integration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// effectKinds lists the effect kinds one operation planned, in planning order.
func (f *fixture) effectKinds(operationID string) string {
	f.t.Helper()
	var kinds sql.NullString
	must(f.t, f.store.Pool.QueryRow("SELECT string_agg(kind,',' ORDER BY created_at,id) FROM external_effects WHERE operation_id=$1", operationID).Scan(&kinds))
	return kinds.String
}

// stepTo runs single simulator transitions until the operation reaches step.
func (f *fixture) stepTo(operationID, step string) core.Object {
	f.t.Helper()
	for range 10 {
		var current string
		must(f.t, f.store.Pool.QueryRow("SELECT step FROM operations WHERE id=$1", operationID).Scan(&current))
		if current == step {
			var version, epoch int64
			must(f.t, f.store.Pool.QueryRow("SELECT version,controller_epoch FROM operations WHERE id=$1", operationID).Scan(&version, &epoch))
			return core.Object{"version": version, "epoch": epoch}
		}
		_, e := f.controller.Step(context.Background())
		must(f.t, e)
	}
	f.t.Fatalf("operation %s never reached step %s", operationID, step)
	return nil
}

// Create goes sandbox -> node -> clone, start goes sandbox -> node, stop terminates; no operation
// plans a storage or worktree effect, and the database refuses to record one.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #workspace-opens-admission-only-after-node-and-clone-succeed.
func TestWorkspaceStepsPlanOnlyRetainedEffects(t *testing.T) {
	f := setup(t)
	created := f.create("steps-project")
	f.drain()
	pid := created.O("resource").S("id")
	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Steps", "baseRef": "main"}, "steps-isolated", 202)
	f.drain()
	iwid := isolated.O("resource").S("id")
	stop := f.call("POST", f.path("/workspaces/"+iwid+"/stop"), f.lifecycleBody(iwid, f.ws(iwid).N("version")), "steps-stop", 202)
	f.drain()
	start := f.call("POST", f.path("/workspaces/"+iwid+"/start"), f.lifecycleBody(iwid, f.ws(iwid).N("version")), "steps-start", 202)
	f.drain()
	cases := []struct {
		name, operation, kinds string
		clones                 int
	}{
		{"create_project", created.O("operation").S("id"), "sandbox_ensure", 1},
		{"create_workspace", isolated.O("operation").S("id"), "sandbox_ensure", 1},
		{"stop", stop.O("operation").S("id"), "sandbox_terminate", 0},
		{"start", start.O("operation").S("id"), "sandbox_ensure", 0},
	}
	for _, c := range cases {
		if got := f.effectKinds(c.operation); got != c.kinds {
			t.Errorf("%s planned %q, want %q", c.name, got, c.kinds)
		}
		if got := f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", c.operation); got != c.clones {
			t.Errorf("%s registered %d clone executions, want %d", c.name, got, c.clones)
		}
	}
	if f.ws(iwid).S("observedState") != "ready" || !f.ws(iwid).B("admissionOpen") {
		t.Fatal("start without a clone must reopen the Workspace on its kept data")
	}

	// A running operation cannot plan a retired effect kind.
	next := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Retired", "baseRef": "main"}, "steps-retired", 202)
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200).O("operation")
	for _, kind := range []string{"storage_ensure", "worktree_ensure", "worktree_delete", "storage_delete"} {
		out := f.internal("/internal/v1/operations/"+claimed.S("id")+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": kind, "workspaceId": next.O("resource").S("id")}, 409)
		if out.S("code") != "invalid_step" {
			t.Errorf("planning retired %s: %v", kind, out)
		}
	}
	// The database itself refuses new retired rows.
	for _, statement := range []string{
		fmt.Sprintf("INSERT INTO project_storage(project_id,observed_state) VALUES('%s','pending')", pid),
		fmt.Sprintf("INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch) VALUES(gen_random_uuid(),'%s','%s','%s','worktree_ensure','planned','{}',1)", claimed.S("id"), pid, iwid),
		fmt.Sprintf("UPDATE operations SET step='storage' WHERE id='%s'", claimed.S("id")),
	} {
		if _, e := f.store.Pool.Exec(statement); e == nil {
			t.Errorf("retired lifecycle row accepted: %s", statement)
		}
	}
	if kinds := f.scalar("SELECT count(DISTINCT kind) FROM external_effects WHERE kind NOT IN ('sandbox_ensure','sandbox_terminate')"); kinds != 0 {
		t.Fatalf("%d unexpected effect kinds planned", kinds)
	}
}

// A failed clone parks the operation and is retried as a new execution; admission stays closed
// until a clone succeeds. An execution with no result blocks a second one, and the operation
// cannot finish on it: it is parked blocked, never retried automatically.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #workspace-opens-admission-only-after-node-and-clone-succeed.
func TestCloneFailureRetriesAsNewExecutionAndUnknownBlocks(t *testing.T) {
	f := setup(t)
	created := f.create("clone-fail")
	oid, wid := created.O("operation").S("id"), created.O("workspace").S("id")
	f.substrate.SetFault("clone", "fail")
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("expected clone failure")
	}
	f.substrate.SetFault("clone", "")
	op := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if op.S("state") != "retry_wait" || op.S("errorCode") != "clone_failed" || op.S("step") != "clone" {
		t.Fatal("clone failure was not parked", op)
	}
	if f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1 AND result->>'outcome'='clone_failed'", oid) != 1 {
		t.Fatal("failed execution was not kept")
	}
	if w := f.ws(wid); w.B("admissionOpen") || w.S("observedState") == "ready" {
		t.Fatal("admission opened without a successful clone", w)
	}
	if closed := f.internal("/internal/v1/access", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "epoch": f.controller.Epoch}, 409); closed.S("code") != "execution_closed" {
		t.Fatal("execution after a failed clone must be refused as execution_closed", closed)
	}
	f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": op.N("version")}, "clone-retry", 202)
	f.drain()
	if f.ws(wid).S("baseCommitId") != f.commit || f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 2 {
		t.Fatal("retry must succeed as a second execution")
	}

	// An execution whose outcome is unknown.
	unknown := f.call("POST", f.path("/projects/"+created.O("resource").S("id")+"/workspaces"), core.Object{"title": "Unknown", "baseRef": "main"}, "clone-unknown", 202)
	uoid, uwid := unknown.O("operation").S("id"), unknown.O("resource").S("id")
	at := f.stepTo(uoid, "clone")
	var node string
	must(t, f.store.Pool.QueryRow("SELECT n.node_id FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND n.ended_at IS NULL", uwid).Scan(&node))
	ctx := asController(f.client.Subject)
	dispatch := func(execution string) error {
		_, e := f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "dispatch-" + execution, Epoch: at.N("epoch"), OperationId: uoid, ExecutionId: execution, NodeId: node, Input: cloneSpec("https://example.invalid/repo.git", "main")})
		return e
	}
	must(t, dispatch("unknown-1"))
	expectStatus(t, dispatch("unknown-2"), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	f.internal("/internal/v1/operations/"+uoid+"/advance", core.Object{"epoch": at.N("epoch"), "version": at.N("version")}, 409)
	f.internal("/internal/v1/operations/"+uoid+"/defer", core.Object{"epoch": at.N("epoch"), "version": at.N("version"), "state": "blocked", "errorCode": "clone_result_unknown", "retrySeconds": 1}, 200)
	if claim := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200); claim["operation"] != nil {
		t.Fatal("an unknown clone outcome was picked up again", claim)
	}
	if f.ws(uwid).B("admissionOpen") {
		t.Fatal("admission opened on an unknown clone outcome")
	}
}

// Cloud, not the Controller, owns the clone step's retry budget, and the budget ends the step.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #a-clone-step-is-attempted-at-most-three-times-before-its-operation-is-failed.
func TestCloneStepStopsAfterItsThirdFailure(t *testing.T) {
	f := setup(t)
	created := f.create("clone-budget")
	oid, wid := created.O("operation").S("id"), created.O("workspace").S("id")
	f.substrate.SetFault("clone", "fail")

	// The first two failures are parks, and each park is Cloud's own backoff: five seconds before the
	// second attempt and ten before the third. The delay the defer request carried is not what is
	// written, which is the point of the budget being Cloud's — the Controller proposes a constant of
	// its own and never counts attempts.
	for _, attempt := range []struct {
		number  int
		backoff float64
	}{{1, 5}, {2, 10}} {
		if e := f.controller.Drain(context.Background()); e == nil {
			t.Fatalf("attempt %d: the clone was expected to fail", attempt.number)
		}
		op := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
		if op.S("state") != "retry_wait" || op.S("errorCode") != "clone_failed" {
			t.Fatalf("attempt %d must be parked as a retry: %v", attempt.number, op)
		}
		var wait float64
		must(t, f.store.Pool.QueryRow("SELECT extract(epoch FROM retry_at-clock_timestamp()) FROM operations WHERE id=$1", oid).Scan(&wait))
		if wait < attempt.backoff-1 || wait > attempt.backoff+1 {
			t.Fatalf("attempt %d must be parked for %.0fs, got %.1fs", attempt.number, attempt.backoff, wait)
		}
		if got := f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid); got != attempt.number {
			t.Fatalf("attempt %d must have exactly its own execution record, got %d", attempt.number, got)
		}
		// The public retry entry makes the parked Operation due at once, so one test can drive three
		// attempts without waiting out two backoffs. It also shows what the budget owes it: a manual
		// retry does not reset the count, because the count is the persisted executions and this entry
		// deletes none of them.
		f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": op.N("version")}, fmt.Sprintf("clone-budget-%d", attempt.number), 202)
	}

	// The third failure is the last one: the step ends instead of parking another retry.
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("the third failing attempt was expected to end the clone step")
	}
	op := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if op.S("state") != "failed" || op.S("errorCode") != "clone_failed" {
		t.Fatalf("the third failure must settle the Operation: %v", op)
	}
	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND retry_at IS NULL", oid) != 1 {
		t.Fatal("an ended Operation must carry no retry time: claim would read it back as a pending retry")
	}
	if got := f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid); got != 3 {
		t.Fatalf("every attempt's execution record must be kept, got %d", got)
	}
	if f.ws(wid).B("admissionOpen") {
		t.Fatal("admission must stay closed once the clone step has ended")
	}
	// Terminal rather than parked: the public retry entry refuses it and no claim picks it up again.
	if refused := f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": op.N("version")}, "clone-budget-late", 409); refused.S("code") != "operation_not_retryable" {
		t.Fatalf("a failed Operation must not be retryable, got %v", refused)
	}
	if e := f.controller.Drain(context.Background()); e != nil {
		t.Fatal("an ended clone step must leave nothing for the Controller to work on")
	}
	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND state='failed'", oid) != 1 {
		t.Fatal("the failed Operation must stay failed")
	}
}

// Cleanup plans a Workspace's data deletion only once none of its sandboxes can still write.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #workspace-data-is-deleted-only-after-every-sandbox-is-terminated.
func TestWorkspaceDataDeleteWaitsForTermination(t *testing.T) {
	f := setup(t)
	created := f.create("data-delete")
	f.drain()
	isolated := f.call("POST", f.path("/projects/"+created.O("resource").S("id")+"/workspaces"), core.Object{"title": "Doomed", "baseRef": "main"}, "data-delete-isolated", 202)
	f.drain()
	iwid := isolated.O("resource").S("id")
	del := f.call("DELETE", f.path("/workspaces/"+iwid), f.lifecycleBody(iwid, f.ws(iwid).N("version")), "data-delete-delete", 202)
	// Test-only: move the operation past quiesce/terminate while the sandbox is still live.
	_, e := f.store.Pool.Exec("UPDATE operations SET step='cleanup' WHERE id=$1", del.O("operation").S("id"))
	must(t, e)
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200).O("operation")
	out := f.internal("/internal/v1/operations/"+claimed.S("id")+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "workspace_data_delete", "workspaceId": iwid}, 409)
	if out.S("code") != "termination_unconfirmed" || f.effectKinds(claimed.S("id")) != "" {
		t.Fatal("data deletion planned while a sandbox is live", out)
	}
}
