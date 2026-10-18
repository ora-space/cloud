package integration

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// These tests prove Cloud authority only. A real scoped Git credential provider remains unavailable.
func TestCredentialDepartureFreezesPersonalAndUnknownButRetainsTeamHistory(t *testing.T) {
	f := setup(t)
	_, bob := f.addUser(t, "credential-bob", "Bob")
	refs := map[string]string{}
	for _, kind := range []string{"unknown", "personal", "team"} {
		ref, err := f.store.ConfigureCredential(context.Background(), f.tid, bob, "deployment-reference/"+kind)
		must(t, err)
		refs[kind] = ref.S("id")
		if kind != "unknown" {
			_, err = f.store.Pool.Exec("UPDATE credential_refs SET attribution=$2,attribution_basis='controlled deployment fixture' WHERE id=$1", ref.S("id"), kind)
			must(t, err)
		}
	}
	f.call("PUT", f.path("/members/"+bob), core.Object{"role": "member", "status": "disabled", "version": 1}, "", 200)
	for kind, id := range refs {
		var status, owner, secret string
		must(t, f.store.Pool.QueryRow("SELECT availability,owner_user_id::text,secret_ref FROM credential_refs WHERE id=$1", id).Scan(&status, &owner, &secret))
		want := "frozen"
		if kind == "team" {
			want = "available"
		}
		if status != want || owner != bob || secret != "deployment-reference/"+kind {
			t.Fatalf("%s: %s %s %s", kind, status, owner, secret)
		}
	}
	// The controlled rejoin fixture changes membership, never the frozen credential decision.
	_, err := f.store.Pool.Exec("UPDATE tenant_memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2", f.tid, bob)
	must(t, err)
	if f.scalar("SELECT count(*) FROM credential_refs WHERE owner_user_id=$1 AND availability='frozen'", bob) != 2 {
		t.Fatal("rejoining resurrected credential authority")
	}
}

func TestCredentialedCloneRefusesMissingExecutorAndAnonymousFallback(t *testing.T) {
	f := setup(t)
	ref, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment-controlled-reference")
	must(t, err)
	created := f.call("POST", f.path("/projects"), core.Object{"name": "Private", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main", "credentialRefId": ref.S("id")}, "private", 202)
	if err = f.controller.Drain(context.Background()); err == nil {
		t.Fatal("credentialed Git must reject an unavailable scoped executor")
	}
	wid := created.O("workspace").S("id")
	if f.scalar("SELECT count(*) FROM clone_executions WHERE workspace_id=$1", wid) != 0 {
		t.Fatal("credentialed project fell back to anonymous/static Git")
	}
	if f.scalar("SELECT count(*) FROM projects p JOIN repository_bindings b ON b.project_id=p.id WHERE p.id=$1 AND p.owner_user_id=$2 AND b.credential_ref_id=$3", created.O("resource").S("id"), f.uid, ref.S("id")) != 1 {
		t.Fatal("refusal rewrote durable ownership or reference")
	}
}

func TestProductionStoreRefusesUnscopedCloneAdmission(t *testing.T) {
	f := setup(t)
	production := newStoreOnSchema(t, f.store.Pool)
	_, err := production.EnqueueClone(context.Background(), f.tid, f.uid, "unscoped", "https://example.invalid/repo.git", "main")
	if err == nil || core.ErrorCode(err).Code != "runtime_scope_required" {
		t.Fatalf("production legacy admission: %v", err)
	}
	if f.scalar("SELECT count(*) FROM clone_requests") != 0 {
		t.Fatal("refused legacy request was persisted")
	}
}
