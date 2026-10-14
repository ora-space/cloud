package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

func TestHealthReportsOptionalObjectStoreWithoutDisclosingConfiguration(t *testing.T) {
	f := setup(t)
	for _, configured := range []bool{false, true} {
		want := "unconfigured"
		if configured {
			f.store.ObjectStore = &objectstore.Config{Endpoint: "http://private.invalid", AccessKeyID: "private-access", SecretAccessKey: "private-secret"}
			want = "configured"
		}
		response, err := f.client.HTTP.Get(f.cloud.URL + "/healthz")
		must(t, err)
		var actual core.Object
		err = json.NewDecoder(response.Body).Decode(&actual)
		response.Body.Close()
		must(t, err)
		if response.StatusCode != http.StatusOK || len(actual) != 2 || actual.S("status") != "ok" || len(actual.O("dependencies")) != 1 || actual.O("dependencies").S("objectStore") != want {
			t.Fatal("health must report only readiness and optional storage configuration")
		}
	}
}

func TestRemainingPublicContractsAndMembershipRevocation(t *testing.T) {
	f := setup(t)
	health, e := f.client.HTTP.Get(f.cloud.URL + "/healthz")
	must(t, e)
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatal("health unavailable")
	}
	tenants := f.call("GET", "/api/v1/me/tenants?limit=10", nil, "", 200)
	if len(tenants["items"].([]any)) != 1 {
		t.Fatal("tenant membership listing", tenants)
	}
	f.call("GET", f.path("/members?limit=10"), nil, "", 200)
	p := f.create("project")
	f.drain()
	pid, wid := p.O("resource").S("id"), p.O("workspace").S("id")
	before := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	renamed := f.call("PATCH", f.path("/projects/"+pid), core.Object{"version": before.N("version"), "name": "Renamed"}, "", 200)
	if renamed.S("name") != "Renamed" || renamed.N("version") != before.N("version")+1 {
		t.Fatal("rename version", renamed)
	}
	f.call("PATCH", f.path("/projects/"+pid), core.Object{"version": before.N("version"), "name": "Stale"}, "", 409)
	list := f.call("GET", f.path("/projects/"+pid+"/workspaces"), nil, "", 200)
	if len(list["items"].([]any)) != 1 {
		t.Fatal(list)
	}
	f.internal("/internal/v1/access", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "read"}, 200)
	f.user.Subject = "member"
	member := f.call("GET", "/api/v1/me", nil, "", 200)
	f.user.Subject = "alice"
	f.addMemberID(member.S("id"), "member")
	f.user.Subject = "member"
	owned := f.create("member-project")
	f.drain()
	memberWorkspace := owned.O("workspace").S("id")
	f.call("GET", f.path("/resource-status"), nil, "", 403)
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+member.S("id")), core.Object{"role": "member", "status": "disabled", "version": 1}, "", 200)
	f.user.Subject = "member"
	f.call("GET", f.path("/workspaces/"+memberWorkspace), nil, "", 403)
	f.internal("/internal/v1/admissions", core.Object{"tenantId": f.tid, "workspaceId": memberWorkspace, "action": "execute", "ticketId": uuid.NewString(), "kind": "task", "epoch": f.controller.Epoch}, 403)
	if f.scalar("SELECT count(*) FROM workspaces WHERE id=$1 AND owner_user_id=$2 AND deleted_at IS NULL", memberWorkspace, member.S("id")) != 1 {
		t.Fatal("revocation destroyed ownership")
	}
}

func TestOperationPreconditionsAndScheduledRetry(t *testing.T) {
	f := setup(t)
	p := f.create("create")
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200).O("operation")
	oid := claimed.S("id")
	prefix := "/internal/v1/operations/" + oid
	f.internal(prefix+"/advance", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version")}, 409)
	f.internal(prefix+"/advance", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "state": "succeeded"}, 400)
	f.internal(prefix+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "sandbox_terminate", "workspaceId": p.O("workspace").S("id")}, 409)
	effect := f.internal(prefix+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "sandbox_ensure", "workspaceId": p.O("workspace").S("id")}, 200)
	f.internal(prefix+"/advance", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version")}, 409)
	deferred := f.internal(prefix+"/defer", core.Object{"epoch": f.controller.Epoch, "version": effect.O("operation").N("version"), "state": "retry_wait", "errorCode": "substrate_timeout", "retrySeconds": 30}, 200)
	none := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200)
	if none["operation"] != nil {
		t.Fatal("retry claimed before due")
	}
	_, e := f.store.Pool.Exec("UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", oid)
	must(t, e)
	f.drain()
	done := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if done.S("state") != "succeeded" || done.N("version") <= deferred.N("version") {
		t.Fatal("scheduled retry failed", done)
	}
	f.internal(prefix+"/effects/"+effect.O("effect").S("id")+"/result", core.Object{"epoch": f.controller.Epoch, "version": effect.O("operation").N("version"), "state": "succeeded", "externalId": "forged", "result": core.Object{"sandboxInstanceId": effect.O("effect").S("id"), "nodeId": "forged"}}, 409)
	node := f.node(p.O("workspace").S("id"))
	out, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/register", "node", node, nil, "", core.Object{"protocolVersion": 1})
	must(t, e)
	if status != 200 {
		t.Fatal(out)
	}
	_, status, e = f.client.Call(context.Background(), "POST", "/internal/v1/nodes/status", "node", node, nil, "", core.Object{"version": out.N("version"), "connectionState": "connected"})
	must(t, e)
	if status != 400 {
		t.Fatal("missing initialized accepted", status)
	}
	different := node
	different.Subject = uuid.NewString()
	_, status, e = f.client.Call(context.Background(), "POST", "/internal/v1/nodes/register", "node", different, nil, "", core.Object{"protocolVersion": 1})
	must(t, e)
	if status != 409 {
		t.Fatal("live node overwritten", status)
	}
	_, status, e = f.client.Call(context.Background(), "POST", "/internal/v1/nodes/register", "node", node, nil, "", core.Object{"protocolVersion": 2})
	must(t, e)
	if status != 400 {
		t.Fatal("unsupported protocol accepted", status)
	}
	f.internal("/internal/v1/controller-lease/release", core.Object{"epoch": f.controller.Epoch}, 200)
	f.internal("/internal/v1/controller-lease/renew", core.Object{"epoch": f.controller.Epoch}, 409)
}

func TestDatabaseEffectAndTicketScopes(t *testing.T) {
	f := setup(t)
	a := f.create("a")
	b := f.create("b")
	f.drain()
	aw, bw := a.O("workspace").S("id"), b.O("workspace").S("id")
	node := f.node(aw)
	if _, e := f.store.Pool.Exec("INSERT INTO execution_tickets(id,tenant_id,workspace_id,node_instance_id,actor_user_id,admission_epoch,kind,state) VALUES($1,$2,$3,$4,$5,0,'task','active')", uuid.NewString(), f.tid, bw, node.Subject, f.uid); e == nil {
		t.Fatal("ticket accepted other workspace node")
	}
	if _, e := f.store.Pool.Exec("INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch) VALUES($1,$2,$3,$4,'worktree_delete','planned','{}',1)", uuid.NewString(), a.O("operation").S("id"), b.O("resource").S("id"), bw); e == nil {
		t.Fatal("effect operation project mismatch accepted")
	}
	f.user.Subject = "ticket-bob"
	bob := f.call("GET", "/api/v1/me", nil, "", 200)
	f.user.Subject = "alice"
	f.addMemberID(bob.S("id"), "member")
	if _, e := f.store.Pool.Exec("INSERT INTO execution_tickets(id,tenant_id,workspace_id,node_instance_id,actor_user_id,admission_epoch,kind,state) VALUES($1,$2,$3,$4,$5,0,'task','active')", uuid.NewString(), f.tid, aw, node.Subject, bob.S("id")); e == nil {
		t.Fatal("ticket accepted an actor who does not own the workspace")
	}
	// The verified gateway key cannot elevate itself by placing controller in role.
	nowClaims := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}, Role: "controller"}
	token, err := f.client.Credentials.Token("gateway", nowClaims)
	must(t, err)
	parsed, _, e := jwt.NewParser().ParseUnverified(token, &core.Claims{})
	must(t, e)
	claims, ok := parsed.Claims.(*core.Claims)
	if !ok {
		t.Fatal("claims type")
	}
	claims.Role = "controller"
	forged := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	forged.Header["kid"] = "gateway"
	raw, e := forged.SignedString(f.client.Credentials.Private["gateway"])
	must(t, e)
	req, e := http.NewRequest("POST", f.cloud.URL+"/internal/v1/controller-lease/acquire", nil)
	must(t, e)
	req.Header.Set("Authorization", "Bearer "+raw)
	res, e := f.client.HTTP.Do(req)
	must(t, e)
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("key purpose failed to pin service role", res.StatusCode)
	}
}

// A Project whose provisioning failed is the Project a member most wants to delete, and it is the one
// the delete used to refuse. Nothing about a failed clone promotes the Project out of `provisioning` —
// only a successful step does — so the condition the delete read, `lifecycle='active'`, was
// permanently false for exactly this Project, and the refusal said `resource_unavailable`, which is
// not a state any member can act on.
//
// Two halves of that are asserted here, because they are two different guarantees and only one of
// them is about this release. The clone step's own end settles the runtime hold it took, so a
// Workspace is not left reserved for an Operation that is over. And the deletion takes over a hold
// whose Operation is no longer live regardless, because a database migrated from a release that ended
// the step without settling it still carries exactly that hold — the state the second half writes
// directly rather than waits to inherit.
//
// Evidence for specs test-cases/cloud/project/space-scoping.md
// #deleting-a-project-supersedes-what-is-still-running-on-it.
func TestProjectDeleteAcceptsAProjectWhoseProvisioningFailed(t *testing.T) {
	f := setup(t)
	created := f.create("failed-delete")
	pid, oid, wid := created.O("resource").S("id"), created.O("operation").S("id"), created.O("workspace").S("id")
	// The clone budget is what ends a provisioning step that cannot succeed, and it is Cloud's, so the
	// failure is driven the way it happens in a deployment rather than written into a column.
	f.substrate.SetFault("clone", "fail")
	for range 3 {
		if e := f.controller.Drain(context.Background()); e == nil {
			t.Fatal("the clone was expected to fail")
		}
		op := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
		if op.S("state") == "failed" {
			break
		}
		f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": op.N("version")}, uuid.NewString(), 202)
	}
	f.substrate.SetFault("clone", "")

	ended := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if ended.S("state") != "failed" || ended.S("errorCode") != "clone_failed" {
		t.Fatalf("this test is about a failed provisioning, got %v", ended)
	}
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='provisioning'", pid) != 1 {
		t.Fatal("a failed provisioning must leave the Project in provisioning, not promote it")
	}
	if f.scalar("SELECT count(*) FROM runtime_controls WHERE workspace_id=$1 AND state='maintenance' AND maintenance_operation_id=$2", wid, oid) != 0 {
		t.Fatal("the ended Operation must not still be holding the Workspace: a hold owned by an Operation that is over starves every later Operation on that Workspace")
	}
	// The pre-fix hold, written the way a deployment that ran the older binary carries it: the control
	// is still `maintenance`, still names an Operation, and that Operation has already ended. The
	// deletion below is what has to survive it.
	if _, err := f.store.Pool.Exec("UPDATE runtime_controls SET state='maintenance',maintenance_operation_id=$2,maintenance_run_id=NULL,version=version+1 WHERE workspace_id=$1", wid, oid); err != nil {
		t.Fatal(err)
	}
	attempts := f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid)

	project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	deleting := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "failed-delete-request", 202)
	// A deletion already in flight is the one live request the delete still refuses, and it says which.
	if again := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": deleting.O("resource").N("version")}, "failed-delete-again", 409); again.S("code") != "operation_in_progress" {
		t.Fatalf("a second deletion must be refused as the duplicate it is, got %v", again)
	}
	f.acknowledgeSimulatorBindings()
	must(t, f.controller.Drain(context.Background()))

	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='deleted'", pid) != 1 {
		t.Fatal("the deletion must have completed")
	}
	if f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 0 || f.scalar("SELECT count(*) FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", pid) != 0 {
		t.Fatal("the deletion must have cleaned up rather than only hidden the Project")
	}
	// The failed attempt is history: it is kept, and the Operation that produced it is not rewritten
	// into a success by the deletion that superseded it.
	if got := f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid); got != attempts || attempts == 0 {
		t.Fatalf("every attempt's execution record must be kept, got %d of %d", got, attempts)
	}
	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND state='failed' AND error_code='clone_failed'", oid) != 1 {
		t.Fatal("the failed Operation must stay failed and keep its own reason")
	}
}

// A Project whose only Operation has not been claimed yet is deletable too, and deleting it has to end
// that Operation rather than leave it queued: a Project that was deleted must not then be provisioned.
//
// The end is recorded as `failed` with this action's own reason code and no retry time, which is how
// the administrative force stop already records a cancellation the member asked for workspace by
// workspace. The Operation is not deleted and its request and actor are not rewritten — the reason
// code is what tells the two kinds of cancellation apart in the audit.
//
// Evidence for specs test-cases/cloud/project/space-scoping.md
// #deleting-a-project-supersedes-what-is-still-running-on-it.
func TestProjectDeleteSupersedesWaitingWorkAndDeletesTheProject(t *testing.T) {
	f := setup(t)
	created := f.create("queued-delete")
	pid, oid := created.O("resource").S("id"), created.O("operation").S("id")
	// Never drained: no Controller has taken the Operation, which is the `queued` half of the reported
	// state and the one a member cannot clear from the UI.
	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND state='queued'", oid) != 1 {
		t.Fatal("the provisioning must still be waiting for this test to mean anything")
	}
	project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "queued-delete-request", 202)

	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND state='failed' AND error_code='superseded_by_project_deletion' AND retry_at IS NULL", oid) != 1 {
		t.Fatal("the waiting provisioning must be ended by the deletion, with no retry time left to claim it again")
	}
	f.drain()
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='deleted'", pid) != 1 {
		t.Fatal("the Project must be deleted")
	}
	if f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 0 {
		t.Fatal("the Project's Workspace must have been deleted with it")
	}
	// Work that was never claimed must not have run on the way out.
	if f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 0 {
		t.Fatal("a superseded provisioning must not have been carried out")
	}
}

func TestProjectDeleteClosesEveryWorkspaceAndWaitsForCleanup(t *testing.T) {
	f := setup(t)
	p := f.create("create")
	f.drain()
	pid, main := p.O("resource").S("id"), p.O("workspace").S("id")
	side := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Side", "baseRef": "main"}, "side", 202)
	f.drain()
	wid := side.O("resource").S("id")
	n := f.node(wid)
	ticket := uuid.NewString()
	f.internal("/internal/v1/admissions", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "ticketId": ticket, "kind": "task", "epoch": f.controller.Epoch, "sessionId": f.controlSession(wid)}, 200)
	project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "busy-delete", 409)
	if !f.ws(main).B("admissionOpen") || !f.ws(wid).B("admissionOpen") {
		t.Fatal("busy cascade partially closed admission")
	}
	f.finishTicket(ticket, n)
	f.releaseRuntimeControl(wid)
	deleting := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "delete", 202)
	for _, id := range []string{main, wid} {
		f.internal("/internal/v1/admissions", core.Object{"tenantId": f.tid, "workspaceId": id, "action": "execute", "ticketId": uuid.NewString(), "kind": "task", "epoch": f.controller.Epoch}, 409)
	}
	f.substrate.SetFault("workspace_data_delete", "fail")
	f.acknowledgeSimulatorBindings()
	if err := f.controller.Drain(context.Background()); err == nil {
		t.Fatal("expected data deletion failure")
	}
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='deleted'", pid) != 0 {
		t.Fatal("project deleted before every Workspace's data was deleted")
	}
	if f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 2 {
		t.Fatal("failed cascade lost resources")
	}
	f.substrate.SetFault("workspace_data_delete", "")
	operation := f.call("GET", f.path("/operations/"+deleting.O("operation").S("id")), nil, "", 200)
	if operation.S("state") != "retry_wait" || operation.S("errorCode") != "substrate_timeout" {
		t.Fatal("cleanup failure was not deferred", operation)
	}
	f.call("POST", f.path("/operations/"+operation.S("id")+"/retry"), core.Object{"version": operation.N("version")}, "cleanup-retry", 202)
	f.drain()
	if f.scalar("SELECT count(*) FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", pid) != 0 || f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 0 {
		t.Fatal("cascade left a live resource")
	}
	if f.scalar("SELECT count(*) FROM external_effects WHERE operation_id=$1 AND kind='workspace_data_delete' AND state='succeeded'", deleting.O("operation").S("id")) != 2 || f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='deleted'", pid) != 1 {
		t.Fatal("project deletion did not delete every Workspace's data before completing")
	}
	for _, id := range []string{main, wid} {
		if _, e := os.Stat(f.substrate.WorkspaceData(id)); !os.IsNotExist(e) {
			t.Fatal("workspace data survived project deletion", id, e)
		}
	}
}
