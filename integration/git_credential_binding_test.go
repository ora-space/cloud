package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// A Project names at most one credential, and `projects.credential_ref_id` is that one place (D7). The
// tests below cover the two ends of it: what may be bound when the Project is created, and what the
// clone path does with the binding once it exists. Until a Node-side authentication channel exists the
// answer to the second is a refusal on every path, and these tests exist to keep it that way.

// projectBody is a create body for one Project, optionally bound to one credential.
func projectBody(repo, credentialRefID string) core.Object {
	body := core.Object{"name": "Private", "repositoryUrl": repo, "defaultBranch": "main"}
	if credentialRefID != "" {
		body["credentialRefId"] = credentialRefID
	}
	return body
}

// TestProjectBindingAcceptsOnlyACredentialThatCouldClone pins the bind-time half of the guard. The two
// conditions the clone path enforces are enforced here with the same reason codes, because a Project
// bound to a credential that can never clone is a Project whose only use is to fail — and the caller
// can still choose another credential at this point.
func TestProjectBindingAcceptsOnlyACredentialThatCouldClone(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	usable := f.createCredential(f.user, "usable", credentialScope)
	frozen := f.createCredential(f.user, "frozen", credentialScope)
	_, err := f.store.Pool.Exec("UPDATE credential_refs SET availability='frozen',unavailable_reason='associated_member_departed' WHERE id=$1", frozen.S("id"))
	must(t, err)
	revoked := f.createCredential(f.user, "revoked", credentialScope)
	f.call("DELETE", f.path("/git-credentials/"+revoked.S("id")), core.Object{"version": 1}, uuid.NewString(), 200)
	elsewhere := f.createCredential(f.user, "elsewhere", "https://example.invalid/other.git")
	other, _ := f.addUser(t, "git-credential-binding", "Binding")
	theirs := f.createCredential(other, "theirs", credentialScope)
	// A deployment-managed reference carries no scope. D7 keeps it bindable for compatibility; no
	// product credential can be in that state.
	managed, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment/managed-reference")
	must(t, err)

	bound := f.call("POST", f.path("/projects"), projectBody(credentialScope, usable.S("id")), uuid.NewString(), 202)
	if bound.O("resource").S("credentialRefId") != usable.S("id") {
		t.Fatalf("the created Project must name the credential it was bound to: %v", bound.O("resource"))
	}
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2", bound.O("resource").S("id"), usable.S("id")) != 1 {
		t.Fatal("the binding is not in the authoritative column")
	}

	cases := []struct {
		name   string
		repo   string
		id     string
		status int
		code   string
	}{
		{"a frozen credential", credentialScope, frozen.S("id"), 409, "repository_credential_frozen"},
		{"a revoked credential", credentialScope, revoked.S("id"), 409, "repository_credential_frozen"},
		{"a credential scoped to another repository", credentialScope, elsewhere.S("id"), 409, "repository_credential_scope_mismatch"},
		{"an ssh repository a scoped token can never authorize", "ssh://git@example.invalid/repo.git", usable.S("id"), 409, "repository_credential_scope_mismatch"},
		{"another member's credential", credentialScope, theirs.S("id"), 404, "credential_ref_not_found"},
		{"a credential that does not exist", credentialScope, uuid.NewString(), 404, "credential_ref_not_found"},
		{"an id that is not an id", credentialScope, "not-a-uuid", 400, "invalid_credential_ref"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.failAs(f.user, "POST", f.path("/projects"), projectBody(c.repo, c.id), uuid.NewString(), c.status, c.code)
		})
	}
	// The compatible case is the deployment reference, and it is asserted separately so a change that
	// made every unbound-scope credential unbindable would be visible here.
	legacy := f.call("POST", f.path("/projects"), projectBody(credentialScope, managed.S("id")), uuid.NewString(), 202)
	if legacy.O("resource").S("credentialRefId") != managed.S("id") {
		t.Fatalf("a deployment-managed reference stays bindable: %v", legacy.O("resource"))
	}
	// Exactly the two accepted creations exist. A refusal that had written a Project row first would
	// have to be undone, and this is the assertion that notices it was not.
	if f.scalar("SELECT count(*) FROM projects") != 2 {
		t.Fatalf("a refused binding left a Project behind: %d", f.scalar("SELECT count(*) FROM projects"))
	}
}

// TestCredentialedCloneStaysRefusedOnEveryPath pins the fail-closed end. The refusal is a reason code
// carrying `repository_credential_executor_unavailable`, it happens before any clone execution is
// registered, and it is reached through the real control surface rather than by calling the guard.
//
// The uncredentialed case is drained first as a control: the same stack clones an ordinary Project, so
// the refusal below is about the credential rather than about a stack that cannot clone at all.
func TestCredentialedCloneStaysRefusedOnEveryPath(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	f.create("public")
	f.drain()

	credential := f.createCredential(f.user, "private", credentialScope)
	created := f.call("POST", f.path("/projects"), projectBody(credentialScope, credential.S("id")), uuid.NewString(), 202)
	pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
	oid := created.O("operation").S("id")

	// The Controller's own view of the Project carries no credential reference at all (D9): not the
	// binding, and not the store's reference. The field stays declared in the contract, and empty is
	// the truthful answer — resolving repository credentials is not something a Controller does.
	operations := controlpb.NewWorkspaceOperationServiceClient(f.controlConn)
	claimed, err := operations.ClaimOperation(asController(f.client.Subject), &controlpb.ClaimOperationRequest{Epoch: f.controller.Epoch})
	must(t, err)
	if claimed.GetSnapshot().GetProject().GetId() != pid {
		t.Fatalf("the Controller claimed another operation: %v", claimed.GetSnapshot().GetProject())
	}
	if claimed.GetSnapshot().GetProject().CredentialRef != nil {
		t.Fatal("the Controller projection carries a credential reference")
	}
	// The JSON projection a Controller reads carries the two declarations as explicit nulls rather than
	// omitting them, so an older reader finds a value instead of an unknown field.
	view := f.internal("/internal/v1/operations/"+oid+"/snapshot", core.Object{"epoch": claimed.GetSnapshot().GetOperation().GetControllerEpoch(), "version": claimed.GetSnapshot().GetOperation().GetVersion()}, 200).O("project")
	if view.S("credentialRefId") != credential.S("id") {
		t.Fatalf("the Controller cannot see which credential the Project uses: %v", view)
	}
	for _, field := range []string{"secretRef", "repositoryCredentialRefId"} {
		if value, ok := view[field]; !ok || value != nil {
			t.Fatalf("%s must be present and null in the Controller projection, got %v (present=%v)", field, value, ok)
		}
	}

	// The refusal itself. It arrives through the clone step's dispatch registration, which is the one
	// place a credentialed clone could be admitted, so the reason code is what the error carries.
	err = f.controller.Drain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repository_credential_executor_unavailable") {
		t.Fatalf("a credentialed clone must be refused with its reason code, got %v", err)
	}
	// Nothing was dispatched, so there is no execution a later recovery could pick up and run with an
	// identity that has no credential.
	if f.scalar("SELECT count(*) FROM clone_executions WHERE workspace_id=$1", wid) != 0 {
		t.Fatal("a credentialed Project fell back to an anonymous or static Git identity")
	}
	// The refusal did not rewrite the binding it refused.
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2 AND owner_user_id=$3", pid, credential.S("id"), f.uid) != 1 {
		t.Fatal("the refusal rewrote durable ownership or the binding")
	}
	// The control Project still cloned, so the step above reached the clone path rather than failing
	// somewhere earlier. Without this the test would pass on a stack that never got that far.
	if f.scalar("SELECT count(*) FROM clone_executions") != 1 {
		t.Fatalf("the control Project did not clone: %d executions", f.scalar("SELECT count(*) FROM clone_executions"))
	}
}

// TestCredentialBindingConflictIsRefusedRatherThanResolved pins D10. The retired 0023 column has no
// writer on any current path, so a non-empty value can only come from something written outside the
// product. When the two columns disagree, silently preferring either one would execute a clone against
// a credential nobody chose — so the disagreement is refused, and agreement is not itself a failure.
func TestCredentialBindingConflictIsRefusedRatherThanResolved(t *testing.T) {
	t.Run("a disagreement is refused", func(t *testing.T) {
		f := setup(t)
		f.store.Secrets = &recordingSecrets{}
		bound := f.createCredential(f.user, "bound", credentialScope)
		other := f.createCredential(f.user, "other", credentialScope)
		created := f.call("POST", f.path("/projects"), projectBody(credentialScope, bound.S("id")), uuid.NewString(), 202)
		pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
		// Direct SQL, because no path in the product writes this column: reproducing the stale row is
		// the only way to reach the state the guard exists for.
		_, err := f.store.Pool.Exec("UPDATE projects SET repository_credential_ref_id=$2 WHERE id=$1", pid, other.S("id"))
		must(t, err)

		err = f.controller.Drain(context.Background())
		if err == nil || !strings.Contains(err.Error(), "repository_credential_binding_conflict") {
			t.Fatalf("a dual binding must be refused with its reason code, got %v", err)
		}
		if f.scalar("SELECT count(*) FROM clone_executions WHERE workspace_id=$1", wid) != 0 {
			t.Fatal("the conflict was resolved by cloning with one of the two credentials")
		}
		if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2 AND repository_credential_ref_id=$3", pid, bound.S("id"), other.S("id")) != 1 {
			t.Fatal("the refusal rewrote either column instead of reporting the disagreement")
		}
	})

	t.Run("agreement is not the refusal", func(t *testing.T) {
		f := setup(t)
		f.store.Secrets = &recordingSecrets{}
		bound := f.createCredential(f.user, "bound", credentialScope)
		created := f.call("POST", f.path("/projects"), projectBody(credentialScope, bound.S("id")), uuid.NewString(), 202)
		pid := created.O("resource").S("id")
		_, err := f.store.Pool.Exec("UPDATE projects SET repository_credential_ref_id=$2 WHERE id=$1", pid, bound.S("id"))
		must(t, err)

		// Both columns naming the same credential is a consistent row. It is still refused, but for the
		// other reason — the missing executor — which is what makes the conflict code above attributable
		// to the disagreement rather than to any project that carries the legacy column at all.
		err = f.controller.Drain(context.Background())
		if err == nil || !strings.Contains(err.Error(), "repository_credential_executor_unavailable") || strings.Contains(err.Error(), "repository_credential_binding_conflict") {
			t.Fatalf("an agreeing pair must be refused for the missing executor, got %v", err)
		}
	})
}

// TestProjectWithoutACredentialKeepsCloningAnonymously is the negative control for the whole guard: a
// Project that names no credential is untouched by it, on both columns, so the refusals above are the
// credential's doing rather than a guard that refuses everything.
func TestProjectWithoutACredentialKeepsCloningAnonymously(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	created := f.create("anonymous")
	wid := created.O("workspace").S("id")
	f.drain()
	if f.scalar("SELECT count(*) FROM clone_executions WHERE workspace_id=$1 AND result IS NOT NULL", wid) != 1 {
		t.Fatal("an uncredentialed Project stopped cloning, so the credential guard is refusing too much")
	}
	if f.scalar("SELECT count(*) FROM projects WHERE credential_ref_id IS NOT NULL OR repository_credential_ref_id IS NOT NULL") != 0 {
		t.Fatal("the anonymous control Project acquired a binding")
	}
}
