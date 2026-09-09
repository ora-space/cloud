package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

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
	f.call("PUT", f.path("/members/"+member.S("id")), core.Object{"role": "member", "status": "active", "version": 0}, "", 200)
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
	f.internal(prefix+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "sandbox_ensure", "workspaceId": p.O("workspace").S("id")}, 409)
	effect := f.internal(prefix+"/effects", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version"), "kind": "storage_ensure"}, 200)
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
	f.internal(prefix+"/effects/"+effect.O("effect").S("id")+"/result", core.Object{"epoch": f.controller.Epoch, "version": effect.O("operation").N("version"), "state": "succeeded", "externalId": "forged", "result": core.Object{"layoutVersion": 1}}, 409)
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
	f.call("PUT", f.path("/members/"+bob.S("id")), core.Object{"role": "member", "status": "active", "version": 0}, "", 200)
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
	f.internal("/internal/v1/admissions", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "ticketId": ticket, "kind": "task", "epoch": f.controller.Epoch}, 200)
	project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "busy-delete", 409)
	if !f.ws(main).B("admissionOpen") || !f.ws(wid).B("admissionOpen") {
		t.Fatal("busy cascade partially closed admission")
	}
	f.finishTicket(ticket, n)
	deleting := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "delete", 202)
	for _, id := range []string{main, wid} {
		f.internal("/internal/v1/admissions", core.Object{"tenantId": f.tid, "workspaceId": id, "action": "execute", "ticketId": uuid.NewString(), "kind": "task", "epoch": f.controller.Epoch}, 409)
	}
	f.substrate.SetFault("worktree_delete", "fail")
	if err := f.controller.Drain(context.Background()); err == nil {
		t.Fatal("expected maintenance failure")
	}
	if f.scalar("SELECT count(*) FROM external_effects WHERE project_id=$1 AND kind='storage_delete'", pid) != 0 {
		t.Fatal("storage deletion scheduled before maintenance completed")
	}
	if f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 2 {
		t.Fatal("failed cascade lost resources")
	}
	f.substrate.SetFault("worktree_delete", "")
	operation := f.call("GET", f.path("/operations/"+deleting.O("operation").S("id")), nil, "", 200)
	if operation.S("state") != "retry_wait" || operation.S("errorCode") != "git_cleanup_failed" {
		t.Fatal("cleanup failure was not deferred", operation)
	}
	f.call("POST", f.path("/operations/"+operation.S("id")+"/retry"), core.Object{"version": operation.N("version")}, "cleanup-retry", 202)
	f.drain()
	if f.scalar("SELECT count(*) FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", pid) != 0 || f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 0 {
		t.Fatal("cascade left a live resource")
	}
	if f.scalar("SELECT count(*) FROM project_storage WHERE project_id=$1 AND observed_state='deleted'", pid) != 1 {
		t.Fatal("storage not confirmed deleted")
	}
}
