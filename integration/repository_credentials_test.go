package integration

import (
	"context"
	"strings"
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

// A credentialed Project's Workspace clones its Git Server snapshot like any other: the external
// repository's credential reference is the Repo Executor's business (operation D1), so it never
// gates the clone and never reaches the clone input, the Controller snapshot or the Node.
func TestCredentialedProjectClonesItsSnapshotWithoutTheCredential(t *testing.T) {
	f := setup(t)
	ref, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment-controlled-reference")
	must(t, err)
	created := f.call("POST", f.path("/projects"), core.Object{"name": "Private", "repositoryUrl": "https://example.invalid/private.git", "defaultBranch": "main", "credentialRefId": ref.S("id")}, "private", 202)
	f.drain()
	wid := created.O("workspace").S("id")
	if w := f.ws(wid); w.S("observedState") != "ready" || w.S("baseCommitId") != f.commit {
		t.Fatalf("a credentialed Project must clone its snapshot: %v", w)
	}
	var input string
	must(t, f.store.Pool.QueryRow("SELECT input::text FROM clone_executions WHERE workspace_id=$1", wid).Scan(&input))
	for _, private := range []string{"example.invalid/private.git", "deployment-controlled-reference", ref.S("id")} {
		if strings.Contains(input, private) {
			t.Fatalf("the clone input carries %q: %s", private, input)
		}
	}
	if f.scalar("SELECT count(*) FROM projects p JOIN repository_bindings b ON b.project_id=p.id WHERE p.id=$1 AND p.owner_user_id=$2 AND b.credential_ref_id=$3", created.O("resource").S("id"), f.uid, ref.S("id")) != 1 {
		t.Fatal("cloning rewrote durable ownership or reference")
	}
}
