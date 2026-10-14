package integration

// This file pins the upgrade path of the credential-continuity migration on the state it was written
// for: a database that already holds references whose owners are no longer present. `0023` adds the
// attribution column with a default of `unknown` and then backfills a freeze over every row whose owner
// is not an active member. The window that closes is real rather than theoretical — a member who left
// before the upgrade never fired the departure trigger, because the trigger did not exist yet — and
// without the backfill exactly those references would stay usable, which is the state the decision
// forbids.
//
// What is asserted is the selectivity, not the freeze alone: an upgrade that froze every reference would
// satisfy "the departed owner's reference is frozen" while disabling every credential in the
// deployment. The rows are seeded before the migration runs, so they are the ones a pre-0023 database
// would already hold rather than rows written into the shape the migration itself created.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMigration0023FreezesReferencesWhoseOwnersWereAlreadyGone drives the backfill against the three
// owner states it has to tell apart.
//
// The three are not variations of one case. A disabled *membership* is the ordinary departure. An active
// membership whose *user* is soft-deleted is the same absence reaching the reference through a different
// table, and the backfill's predicate reads both, because its `NOT EXISTS` joins `users` as well as
// `tenant_memberships` — so a reference owned by a deleted user whose membership row is still `active`
// is frozen too. The third row is the control: nothing about it changes, and it is what makes the first
// two evidence about the predicate rather than about the migration writing to every row it can see.
//
// The reason column is asserted together with the availability, because a row that arrives frozen with
// no reason at all leaves an operator unable to tell a deliberate freeze from a corrupt one. What the
// backfill writes for the soft-deleted user is worth reading twice: the same
// `associated_member_departed`. The migration names the event it can name — the access ended with the
// person — rather than distinguishing the two tables its predicate read, and this test pins that as the
// intended behavior instead of asserting a distinction the migration does not make.
func TestMigration0023FreezesReferencesWhoseOwnersWereAlreadyGone(t *testing.T) {
	pool, _ := testSchema(t, "test_cred23_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0023_repository_credential_continuity.sql"))
	if columnExists(t, pool, "credential_refs", "attribution") {
		t.Fatal("the upgrade path must start from a database without the continuity columns")
	}

	// The tenant, its members and their references are written in one transaction because 0002's
	// `tenant_admin` constraint trigger is deferrable: an active tenant with no active administrator is
	// refused at commit rather than at the statement that creates it. The upgrade path has to start from
	// a tenant that was valid when it was written, so the seed is committed as the deployment would have
	// committed it.
	tenant := uuid.NewString()
	tx, e := pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		_, err := tx.Exec(query, args...)
		must(t, err)
	}
	exec(`INSERT INTO tenants(id,name,status) VALUES($1,'upgrade path tenant','active')`, tenant)

	// seed writes one owner in the given state together with its membership and one reference, in the
	// pre-0023 shape: no attribution, no availability, no scope. A soft-deleted user is inserted already
	// deleted rather than updated into it, because the `users` trigger fires on UPDATE — an upgrade that
	// only the trigger protected would leave this row behind, which is the case being pinned. The present
	// member is also the tenant's active administrator, so the deferred guard above is satisfied by the
	// same rows the test needs rather than by an extra fixture.
	seed := func(name, membershipStatus string, userDeleted bool) string {
		user, reference := uuid.NewString(), uuid.NewString()
		var deletedAt any
		if userDeleted {
			deletedAt = time.Now()
		}
		exec(`INSERT INTO users(id,display_name,status,deleted_at) VALUES($1,$2,'active',$3)`, user, name, deletedAt)
		exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin',$3)`, tenant, user, membershipStatus)
		exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref) VALUES($1,$2,$3,'git',$4)`,
			reference, tenant, user, "credential-ref/"+reference)
		return reference
	}
	departed := seed("departed member", "disabled", false)
	deletedUser := seed("deleted user", "active", true)
	present := seed("present member", "active", false)
	must(t, tx.Commit())

	migratedTwice(t, newStoreOnSchema(t, pool))

	// Every existing reference arrives at `unknown`: the column is added with that default, and the
	// backfill's attribution filter therefore cannot exclude any of the three rows. Without this the
	// selectivity below could be explained by the filter rather than by the membership predicate.
	var unknown int
	must(t, pool.QueryRow(`SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND attribution='unknown'`, tenant).Scan(&unknown))
	if unknown != 3 {
		t.Fatalf("existing references must arrive at 'unknown' attribution, got %d of 3", unknown)
	}

	state := func(reference string) (string, string, int64) {
		t.Helper()
		var availability, reason string
		var version int64
		must(t, pool.QueryRow(`SELECT availability,coalesce(unavailable_reason,''),version FROM credential_refs WHERE id=$1`, reference).
			Scan(&availability, &reason, &version))
		return availability, reason, version
	}

	// A version of 2 is part of the assertion, not decoration: it is the migration's own signal that the
	// row moved, so a reference that reports `frozen` without having advanced cannot pass as frozen by
	// the backfill.
	for _, frozen := range []struct{ name, reference string }{
		{"a member whose membership was already disabled", departed},
		{"a member whose user was already soft-deleted", deletedUser},
	} {
		availability, reason, version := state(frozen.reference)
		if availability != "frozen" || reason != "associated_member_departed" {
			t.Fatalf("%s: the upgrade left the reference %s/%q, want frozen/associated_member_departed", frozen.name, availability, reason)
		}
		if version != 2 {
			t.Fatalf("%s: the backfill froze the reference without advancing its version (%d, want 2)", frozen.name, version)
		}
	}

	availability, reason, version := state(present)
	if availability != "available" || reason != "" || version != 1 {
		t.Fatalf("a reference owned by a present member was touched by the backfill: %s/%q/version %d, want available/(none)/1", availability, reason, version)
	}
}
