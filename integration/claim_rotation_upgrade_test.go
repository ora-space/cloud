package integration

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Fresh databases and a populated 0050 database must gain claim bookkeeping without rewriting
// historical operation values. Repeated migration must preserve a recorded claim opportunity.
func TestMigration0051ClaimRotationAppliesFreshAndUpgrades(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade_from_0050"
		}
		t.Run(name, func(t *testing.T) {
			pool, _ := testSchema(t, "test_claim51_")
			store := newStoreOnSchema(t, pool)
			if upgrade {
				applyMigrationsUpTo(t, pool, migrationsBefore(t, "0051_operation_last_claimed_at.sql"))
			} else {
				migratedTwice(t, store)
			}
			s := seedAgentIssueRunSkeleton(t, pool)
			id := uuid.NewString()
			_, err := pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash)
				VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}','claim-upgrade','claim-upgrade')`, id, s.tenantID, s.userID, s.projectID, s.runWorkspaceID)
			must(t, err)
			var before, after string
			must(t, pool.QueryRow(`SELECT (to_jsonb(o)-'last_claimed_at')::text FROM operations o WHERE id=$1`, id).Scan(&before))
			migratedTwice(t, store)
			must(t, pool.QueryRow(`SELECT (to_jsonb(o)-'last_claimed_at')::text FROM operations o WHERE id=$1`, id).Scan(&after))
			var claimed sql.NullTime
			must(t, pool.QueryRow(`SELECT last_claimed_at FROM operations WHERE id=$1`, id).Scan(&claimed))
			if before != after || claimed.Valid || !columnNullable(t, pool, "operations", "last_claimed_at") {
				t.Fatal("claim migration rewrote history or backfilled a claim opportunity")
			}
			var index string
			must(t, pool.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='operations_claim_rotation'`).Scan(&index))
			if !strings.Contains(index, "COALESCE(last_claimed_at, created_at), created_at, id") || !strings.Contains(index, "WHERE") {
				t.Fatalf("missing partial rotation index: %s", index)
			}
			_, err = pool.Exec(`UPDATE operations SET last_claimed_at='2026-01-01T00:00:00Z' WHERE id=$1`, id)
			must(t, err)
			migratedTwice(t, store)
			must(t, pool.QueryRow(`SELECT last_claimed_at FROM operations WHERE id=$1`, id).Scan(&claimed))
			if !claimed.Valid || claimed.Time.UTC().Format("2006-01-02T15:04:05Z") != "2026-01-01T00:00:00Z" {
				t.Fatal("repeated migration rewrote the recorded claim time")
			}
		})
	}
}
