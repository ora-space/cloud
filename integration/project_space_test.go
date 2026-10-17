package integration

import (
	"context"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestProjectSpaceScopingAndRoleGates verifies project access through the sole
// tenant membership and prevents access by an identity outside the tenant.
func TestProjectSpaceScopingAndRoleGates(t *testing.T) {
	f := setup(t)
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	space := f.fixtureSpace()
	sid := space.S("id")
	bob, _ := f.addUser(t, "bob", "Bob")
	carol := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "carol"}, Source: "corp", DisplayName: "Carol"}

	// Scenario 2: a member creates a project inside the space.
	_, status, e := f.client.Call(context.Background(), "POST", f.path("/spaces/"+sid+"/projects"), "gateway", gw, &bob, "bob-project", core.Object{"name": "Shared", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"})
	must(t, e)
	if status != 202 {
		t.Fatalf("member create project: want 202 got %d", status)
	}
	// Space projects are visible to members and isolated from non-members.
	list := f.call("GET", f.path("/spaces/"+sid+"/projects"), nil, "", 200)
	items := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("space project list: want 1 got %d", len(items))
	}
	pid := core.Object(items[0].(map[string]any)).S("id")
	// The project was created outside f.call, so stand in for its first sync here.
	f.syncProject(pid)
	if core.Object(items[0].(map[string]any)).S("spaceId") != sid {
		t.Fatal("project missing spaceId")
	}
	// Scenario 3/11: a non-member of this space cannot read the project.
	_, status, e = f.client.Call(context.Background(), "GET", f.path("/projects/"+pid), "gateway", gw, &carol, "", nil)
	must(t, e)
	if status != 403 {
		t.Fatalf("non-member read: want 403 got %d", status)
	}
	// Scenario 4: a member can update business content.
	p := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	_, status, e = f.client.Call(context.Background(), "PATCH", f.path("/projects/"+pid), "gateway", gw, &bob, "", core.Object{"name": "Shared Renamed", "version": p.N("version")})
	must(t, e)
	if status != 200 {
		t.Fatalf("member patch: want 200 got %d", status)
	}
	// Scenario 12: optimistic conflict returns 409.
	_, status, e = f.client.Call(context.Background(), "PATCH", f.path("/projects/"+pid), "gateway", gw, &bob, "", core.Object{"name": "Stale", "version": p.N("version")})
	must(t, e)
	if status != 409 {
		t.Fatalf("stale patch: want 409 got %d", status)
	}

	// Scenario 9: a member cannot delete the project.
	after := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	_, status, e = f.client.Call(context.Background(), "DELETE", f.path("/projects/"+pid), "gateway", gw, &bob, "bob-delete", core.Object{"version": after.N("version")})
	must(t, e)
	if status != 403 {
		t.Fatalf("member delete project: want 403 got %d", status)
	}

	// Scenario 10: an owner deletes through the existing lifecycle state machine.
	f.drain()
	active := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	if active.S("lifecycle") != "active" {
		t.Fatalf("project did not reach active: %s", active.S("lifecycle"))
	}
	f.acknowledgeSimulatorBindings()
	deleted := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": active.N("version")}, "owner-delete", 202)
	if deleted.O("resource").S("lifecycle") != "deleting" || deleted.O("operation").S("id") == "" {
		t.Fatal("owner delete did not enter lifecycle state machine")
	}
	f.drain()
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND deleted_at IS NULL", pid) != 0 {
		t.Fatal("delete lifecycle did not complete")
	}
}

// TestDifferentMemberCanCreateRuntimeInSharedProject keeps the project owner
// binding intact while recording the initiating member as the operation actor.
func TestDifferentMemberCanCreateRuntimeInSharedProject(t *testing.T) {
	f := setup(t)
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	bob, bobID := f.addUser(t, "bob", "Bob")
	created := f.call("POST", f.path("/projects"), core.Object{"name": "Shared", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"}, "shared-project", 202)
	pid := created.O("resource").S("id")
	f.drain()

	out, status, err := f.client.Call(context.Background(), "POST", f.path("/projects/"+pid+"/workspaces"), "gateway", gw, &bob, "bob-runtime", core.Object{"title": "Bob's task", "baseRef": "main"})
	must(t, err)
	if status != 202 {
		t.Fatalf("member create runtime in shared project: want 202, got %d (%v)", status, out)
	}
	resource := out.O("resource")
	if resource.S("ownerUserId") != f.uid || resource.S("creatorUserId") != bobID || out.O("operation").S("actorUserId") != bobID {
		t.Fatalf("runtime owner and operation actor diverged from their durable bindings: %v", out)
	}
	opid := out.O("operation").S("id")
	if f.call("GET", f.path("/operations/"+opid), nil, "", 200).S("id") != opid {
		t.Fatal("project owner could not inspect shared operation")
	}
	_, status, err = f.client.Call(context.Background(), "GET", f.path("/operations/"+opid), "gateway", gw, &bob, "", nil)
	must(t, err)
	if status != 200 {
		t.Fatalf("operation actor could not inspect shared operation: %d", status)
	}
	f.drain()
	list, status, err := f.client.Call(context.Background(), "GET", f.path("/projects/"+pid+"/workspaces"), "gateway", gw, &bob, "", nil)
	must(t, err)
	if status != 200 {
		t.Fatalf("member runtime list: want 200 got %d (%v)", status, list)
	}
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("shared project must expose main and isolated runtimes without legacy worktree rows: %v", list)
	}
	for _, item := range items {
		w := core.Object(item.(map[string]any))
		if w.S("observedState") != "ready" || w.S("ownerUserId") != f.uid {
			t.Fatalf("shared runtime lost Node clone readiness or project ownership: %v", w)
		}
		if w.S("id") == resource.S("id") {
			if !w.B("canUse") || w.S("baseCommitId") != f.commit {
				t.Fatalf("own runtime clone content missing: %v", w)
			}
		} else if w.B("canUse") || w["baseCommitId"] != nil || w["title"] != nil || w["requestedRef"] != nil {
			t.Fatalf("other runtime content leaked: %v", w)
		}
	}
	if f.scalar("SELECT count(*) FROM workspace_worktrees") != 0 || f.scalar("SELECT count(*) FROM project_storage") != 0 {
		t.Fatal("shared runtime must not reintroduce retired storage or worktree provisioning")
	}
}

// TestConcurrentMembersCannotStartTwoProjectOperations documents the current
// per-project lifecycle fence when members request separate runtimes together.
func TestConcurrentMembersCannotStartTwoProjectOperations(t *testing.T) {
	f := setup(t)
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	bob, _ := f.addUser(t, "bob", "Bob")
	created := f.call("POST", f.path("/projects"), core.Object{"name": "Shared", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"}, "shared-project", 202)
	pid := created.O("resource").S("id")
	f.drain()

	type outcome struct {
		status int
		body   core.Object
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	actors := []core.Claims{f.user, bob}
	keys := []string{"runtime-alice", "runtime-bob"}
	for i := range actors {
		go func(i int) {
			<-start
			body, status, err := f.client.Call(context.Background(), "POST", f.path("/projects/"+pid+"/workspaces"), "gateway", gw, &actors[i], keys[i], core.Object{"title": "Shared task", "baseRef": "main"})
			results <- outcome{status, body, err}
		}(i)
	}
	close(start)
	statuses := map[int]int{}
	for range actors {
		got := <-results
		must(t, got.err)
		statuses[got.status]++
		if got.status == 409 && got.body.S("code") != "operation_in_progress" {
			t.Fatalf("unexpected conflict: %v", got.body)
		}
	}
	if statuses[202] != 1 || statuses[409] != 1 || len(statuses) != 2 {
		t.Fatalf("concurrent runtime operations: want one 202 and one 409, got %v", statuses)
	}
	if got := f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND kind='isolated' AND deleted_at IS NULL", pid); got != 1 {
		t.Fatalf("concurrent runtime operations created %d isolated workspaces, want 1", got)
	}
}

// TestTenantMemberLastAdminRace requires one active administrator after
// simultaneous demotions of two administrators.
func TestTenantMemberLastAdminRace(t *testing.T) {
	f := setup(t)
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	bob, bobID := f.addUser(t, "bob", "Bob")
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	// Two admins; demoting both concurrently must leave exactly one.
	subjects := []core.Claims{f.user, bob}
	ids := []string{f.uid, bobID}
	statuses := make(chan int, 2)
	for i := range subjects {
		go func(i int) {
			rowVersion := float64(1)
			if i == 1 {
				rowVersion = 2
			}
			_, status, _ := f.client.Call(context.Background(), "PUT", f.path("/members/"+ids[i]), "gateway", gw, &subjects[i], "", core.Object{"role": "member", "status": "active", "version": rowVersion})
			statuses <- status
		}(i)
	}
	success, conflict := 0, 0
	for range 2 {
		switch <-statuses {
		case 200:
			success++
		case 409:
			conflict++
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("last admin race: success=%d conflict=%d", success, conflict)
	}
	if f.scalar("SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND role='admin' AND status='active'", f.tid) != 1 {
		t.Fatal("tenant lost its last administrator")
	}
}
