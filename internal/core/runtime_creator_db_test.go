package core

// Which Operation created a Workspace is what a member reads to learn why a Project that never became
// usable is still unusable: the Project's own lifecycle has no terminal failure state, so the settled
// reason lives on the Operation and the Workspace is the only public handle on it.
//
// These tests drive the real creation paths (`createProject`, `createWorkspace`) against a real
// schema and then read the Workspace back through the same two member-facing reads the frontend uses.
// They pin three things at once: that a Workspace Cloud created itself names its creating Operation,
// that the durable `creator_operation_id` and `creator_evidence` columns are left exactly as migration
// 0018 defined them — resolution is a read, not a write, and the immutability trigger stays
// authoritative — and that a scope matching more than one Operation names none of them.

import (
	"context"
	"testing"
)

// creatorScene is one Project with its main Workspace, created through Cloud's own creation path.
type creatorScene struct {
	store     *Store
	tenant    string
	user      string
	space     string
	project   string
	workspace string
	operation string
}

// seedCreatorProject creates the tenant, its space and one Project in a single transaction, because
// the schema's integrity triggers are deferred and an active tenant requires an active administrator.
func seedCreatorProject(t *testing.T) creatorScene {
	t.Helper()
	store := dispatcherDB(t)
	s := creatorScene{store: store, tenant: newID(), user: newID(), space: newID()}
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'creator','active')`, s.user)
		t.exec(`INSERT INTO tenants(id,name,status) VALUES($1,'creator','active')`, s.tenant)
		t.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, s.tenant, s.user)
		t.exec(`INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'creator','creator-scene',$3)`, s.space, s.tenant, s.user)
		r := &PublicRequest{TenantID: s.tenant, Key: "creator-" + s.tenant, Body: Object{
			"name": "creator", "repositoryUrl": "https://git.example.invalid/creator.git", "defaultBranch": "main",
		}}
		created := createProject(t, r, s.user, requestHash("POST", "/projects", r.Body))
		s.project, s.workspace, s.operation = created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
		return Object{}
	}); err != nil {
		t.Fatalf("seed creator scene: %v", err)
	}
	return s
}

// transaction runs one statement block against the scene's database, as every read below does: the
// reads under test are transaction-shaped, and a raw pool query would not exercise them.
func (s creatorScene) transaction(t *testing.T, fn func(*transaction) Object) Object {
	t.Helper()
	out, err := s.store.transact(context.Background(), fn)
	if err != nil {
		t.Fatalf("scene transaction: %v", err)
	}
	return out
}

// document is the Workspace document a member reads by id.
func (s creatorScene) document(t *testing.T) Object {
	t.Helper()
	return s.transaction(t, func(t *transaction) Object {
		return workspaceDocument(t, s.tenant, s.user, s.workspace, false)
	})
}

// listed is the single Workspace of the scene's Project as the member-facing list reports it.
func (s creatorScene) listed(t *testing.T) Object {
	t.Helper()
	out := s.transaction(t, func(t *transaction) Object {
		return runtimeListing(t, &PublicRequest{TenantID: s.tenant}, s.user, s.project)
	})
	items, _ := out["items"].([]Object)
	if len(items) != 1 {
		t.Fatalf("listed workspaces: got %d items, want 1", len(items))
	}
	return items[0]
}

// durableCreator reads the columns migration 0018 owns, which resolution must never change.
func (s creatorScene) durableCreator(t *testing.T, wid string) (operation any, evidence string) {
	t.Helper()
	var operationID any
	evidence = s.transaction(t, func(t *transaction) Object {
		row := t.one("SELECT creator_operation_id,creator_evidence FROM workspaces WHERE id=$1", wid)
		operationID = row["creatorOperationId"]
		return row
	}).S("creatorEvidence")
	return operationID, evidence
}

// settleOperation makes an Operation stop being live, which is what frees its Project for the next
// one: the partial unique index allows a Project a single live Operation.
func (s creatorScene) settleOperation(t *testing.T, oid string) {
	t.Helper()
	s.transaction(t, func(t *transaction) Object {
		t.exec("UPDATE operations SET state='succeeded' WHERE id=$1", oid)
		return Object{}
	})
}

// insertOperation stages one extra Operation exactly scoped to a Workspace, which is how a scene
// produces the ambiguity and the kind mismatch the resolver has to answer for. The creation paths
// cannot produce either, and the durable columns cannot be rewritten afterwards.
func (s creatorScene) insertOperation(t *testing.T, oid, kind, wid string) {
	t.Helper()
	s.transaction(t, func(t *transaction) Object {
		t.exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash)
			VALUES($1,$2,$3,$4,$5,$6,'succeeded','sandbox','{}'::jsonb,$7,$8)`,
			oid, s.tenant, s.user, s.project, wid, kind, "extra-"+oid, requestHash("EXTRA", kind, Object{"id": oid}))
		return Object{}
	})
}

// TestWorkspaceDocumentNamesTheOperationThatCreatedIt is the product obligation: a Project that never
// became usable must be able to name the Operation that failed, on the list a member sees and on the
// document they open, without Cloud writing a link it cannot legally write.
func TestWorkspaceDocumentNamesTheOperationThatCreatedIt(t *testing.T) {
	s := seedCreatorProject(t)

	if got := s.document(t).S("creatorOperationId"); got != s.operation {
		t.Errorf("workspace document creating operation: got %q, want %q", got, s.operation)
	}
	if got := s.listed(t).S("creatorOperationId"); got != s.operation {
		t.Errorf("workspace list creating operation: got %q, want %q", got, s.operation)
	}
	// Reading the link is not evidence of it: migration 0018's columns stay empty on a Workspace
	// Cloud created, and this read must not be mistaken for the backfill that never ran on it.
	durable, evidence := s.durableCreator(t, s.workspace)
	if durable != nil {
		t.Errorf("durable creator operation: got %v, want nil", durable)
	}
	if evidence != "verified_request" {
		t.Errorf("durable creator evidence: got %q, want %q", evidence, "verified_request")
	}
}

// TestWorkspaceDocumentNamesTheCreatingOperationOfAnIsolatedWorkspace covers the other creation path:
// a Workspace created on an active Project is created by `create_workspace`, and resolving it against
// `create_project` would name an Operation that belongs to a different Workspace's creation.
func TestWorkspaceDocumentNamesTheCreatingOperationOfAnIsolatedWorkspace(t *testing.T) {
	s := seedCreatorProject(t)
	// An active Project is what a Workspace may be created on, and it may hold one live Operation.
	s.settleOperation(t, s.operation)
	created := s.transaction(t, func(t *transaction) Object {
		t.exec("UPDATE projects SET lifecycle='active' WHERE id=$1", s.project)
		r := &PublicRequest{TenantID: s.tenant, Key: "isolated-" + newID(), Body: Object{"title": "creator", "baseRef": "main"}}
		return createWorkspace(t, r, t.one("SELECT * FROM projects WHERE id=$1", s.project), s.user, requestHash("POST", "/workspaces", r.Body))
	})
	want := created.O("operation").S("id")
	if got := created.O("resource").S("creatorOperationId"); got != want {
		t.Errorf("create workspace response: got creating operation %q, want %q", got, want)
	}
	if got := s.transaction(t, func(t *transaction) Object {
		return workspaceDocument(t, s.tenant, s.user, created.O("resource").S("id"), false)
	}).S("creatorOperationId"); got != want {
		t.Errorf("isolated workspace document: got creating operation %q, want %q", got, want)
	}
}

// TestWorkspaceDocumentNamesNoOperationForAnAmbiguousScope pins the rule migration 0018's backfill
// used: two Operations that could both have created the Workspace are no evidence for either, because
// naming the wrong one would report a failure the member cannot act on against the wrong cause.
func TestWorkspaceDocumentNamesNoOperationForAnAmbiguousScope(t *testing.T) {
	s := seedCreatorProject(t)
	s.insertOperation(t, newID(), "create_project", s.workspace)

	if got := s.document(t).S("creatorOperationId"); got != "" {
		t.Errorf("ambiguous creating operation: got %q, want none", got)
	}
}

// TestWorkspaceDocumentIgnoresAnOperationOfAnotherKind keeps the scope exact: an Operation that
// created a different Workspace kind through the same Workspace row's project is not this
// Workspace's creator. The list a `start` operation leaves behind must not answer for the creation.
func TestWorkspaceDocumentIgnoresAnOperationOfAnotherKind(t *testing.T) {
	s := seedCreatorProject(t)
	s.insertOperation(t, newID(), "create_workspace", s.workspace)

	if got := s.document(t).S("creatorOperationId"); got != s.operation {
		t.Errorf("creating operation beside another kind: got %q, want %q", got, s.operation)
	}
}

// TestWorkspaceDocumentKeepsDurableCreatorEvidence makes the durable columns authoritative over the
// read: a Workspace that migration 0018 did link keeps that link even when the scope would otherwise
// be ambiguous, because the recorded evidence is proof and a re-derivation is only a fallback.
func TestWorkspaceDocumentKeepsDurableCreatorEvidence(t *testing.T) {
	s := seedCreatorProject(t)
	recorded := newID()
	wid := newID()
	// Staged in three statements, which is the only order the two tables allow a durable link in:
	// both foreign keys are immediate, so the Operation has to exist before the Workspace can name it
	// and can only be scoped to that Workspace afterwards. That is the ordering constraint this
	// resolution exists for, and it is why a Workspace Cloud creates is created in the other order.
	s.transaction(t, func(t *transaction) Object {
		t.exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash)
			VALUES($1,$2,$3,$4,'create_workspace','succeeded','sandbox','{}'::jsonb,$5,$6)`,
			recorded, s.tenant, s.user, s.project, "recorded-"+recorded, requestHash("RECORDED", "create_workspace", Object{"id": recorded}))
		t.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_operation_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$3,$4,'creation_operation',$5,'isolated','running','provisioning','main')`,
			wid, s.tenant, s.user, recorded, s.project)
		t.exec("UPDATE operations SET workspace_id=$2 WHERE id=$1", recorded, wid)
		t.exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'creator')`, newID(), wid)
		return Object{}
	})
	// A second Operation in the same scope would make the read ambiguous; the recorded link still wins.
	s.insertOperation(t, newID(), "create_workspace", wid)
	// The Project's main Workspace is the scene's, so the isolated one is read by its own id.
	document := s.transaction(t, func(t *transaction) Object {
		return workspaceDocument(t, s.tenant, s.user, wid, false)
	})
	if got := document.S("creatorOperationId"); got != recorded {
		t.Errorf("durable creating operation: got %q, want %q", got, recorded)
	}
}
