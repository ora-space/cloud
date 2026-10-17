package integration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// The 0017 upgrade keeps owner/operation/credential history and proves creator attribution.
// specs/test-cases/cloud/workspace/runtime-workspace-use-permissions.md#historical-creators-are-backfilled-only-from-unique-creation-evidence
func TestRuntimeUpgradeOnlyBackfillsUniqueCreationEvidenceAndKeepsUnknownReferences(t *testing.T) {
	pool, _ := testSchema(t, "test_runtime_upgrade_")
	entries, err := os.ReadDir(filepath.Join("..", "internal", "core", "migrations"))
	must(t, err)
	var versions []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".sql" && entry.Name() < "0018" {
			versions = append(versions, entry.Name())
		}
	}
	applyMigrationsUpTo(t, pool, versions)
	admin, member, tenant, space, project, ref := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := pool.Begin()
	must(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	exec := func(query string, args ...any) { t.Helper(); _, e := tx.Exec(query, args...); must(t, e) }
	exec("INSERT INTO users(id,display_name,status) VALUES($1,'Admin','active'),($2,'Creator','active')", admin, member)
	exec("INSERT INTO tenants(id,name,status) VALUES($1,'Upgrade','active')", tenant)
	exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active'),($1,$3,'member','active')", tenant, admin, member)
	exec("INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'Upgrade','upgrade-runtime',$3)", space, tenant, admin)
	exec("INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref) VALUES($1,$2,$3,'git','controlled-reference-only')", ref, tenant, admin)
	exec("INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle,credential_ref_id) VALUES($1,$2,$3,$4,'Project','https://example.invalid/repo.git','main','active',$5)", project, tenant, admin, space, ref)
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for i, wid := range ids {
		kind := "isolated"
		if i == 0 {
			kind = "main"
		}
		exec("INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref) VALUES($1,$2,$3,$4,$5,'running','ready','main')", wid, tenant, admin, project, kind)
		if kind == "isolated" {
			exec("INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'Historical task')", uuid.NewString(), wid)
		}
	}
	operation := uuid.NewString()
	exec("INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}','unique-creation-key','historical-input')", operation, tenant, member, project, ids[1])
	for range 2 {
		oid := uuid.NewString()
		exec("INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}','conflicting-creation-key','conflicting-evidence')", oid, tenant, member, project, ids[2])
	}
	must(t, tx.Commit())
	var originalOperations string
	must(t, pool.QueryRow("SELECT jsonb_agg(to_jsonb(o) ORDER BY id)::text FROM operations o").Scan(&originalOperations))
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))
	for i, wid := range ids {
		var owner, evidence, control string
		var creator, creatorOp sql.NullString
		must(t, pool.QueryRow("SELECT w.owner_user_id::text,w.creator_user_id::text,w.creator_operation_id::text,w.creator_evidence,c.state FROM workspaces w JOIN runtime_controls c ON c.workspace_id=w.id WHERE w.id=$1", wid).Scan(&owner, &creator, &creatorOp, &evidence, &control))
		if owner != admin || control != "reconciling" {
			t.Fatal("upgrade changed ownership or guessed idle", owner, control)
		}
		if i == 1 {
			if creator.String != member || creatorOp.String != operation || evidence != "creation_operation" {
				t.Fatal("unique evidence lost", creator, creatorOp, evidence)
			}
		} else if creator.Valid || creatorOp.Valid || evidence != "unknown" {
			t.Fatal("unproven creator guessed", creator, creatorOp, evidence)
		}
	}
	var afterOperations, attribution, availability, secret, secretState, oldRef, owner string
	must(t, pool.QueryRow("SELECT jsonb_agg(to_jsonb(o) ORDER BY id)::text FROM operations o").Scan(&afterOperations))
	if afterOperations != originalOperations {
		t.Fatal("migration rewrote operation history")
	}
	must(t, pool.QueryRow("SELECT c.attribution,c.availability,c.secret_ref,c.secret_state,b.credential_ref_id::text,p.owner_user_id::text FROM credential_refs c JOIN repository_bindings b ON b.credential_ref_id=c.id JOIN projects p ON p.id=b.project_id WHERE c.id=$1", ref).Scan(&attribution, &availability, &secret, &secretState, &oldRef, &owner))
	// The external line's switch keeps the historical row but can never redeem it: a
	// deployment-preset reference has no version in the Credential Store.
	if attribution != "unknown" || availability != "available" || secret != "controlled-reference-only" || secretState != "revoked" || oldRef != ref || owner != admin {
		t.Fatal("credential migration guessed attribution or rewrote bindings")
	}
	_, err = pool.Exec("UPDATE workspaces SET creator_user_id=$2 WHERE id=$1", ids[1], admin)
	if err == nil {
		t.Fatal("historical creator was mutable")
	}
}
