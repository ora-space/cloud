package integration

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestSessionStatisticsHTTPUnknownHistoryAuthorizationAndQueries(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	base := strings.TrimSuffix(sessionDashboardHistoryPath(scene.tenantID, scene.issueID, scene.runID), "history")
	statistics, calls := base+"statistics", base+"model-calls"
	// The simulator validates these successful payloads against the generated OpenAPI contract.
	out := f.call("GET", statistics, nil, "", 200)
	if out.O("counts").N("total") != 0 || out.O("usage").O("inputTokens")["value"] != nil || out.O("usage").O("inputTokens").S("availability") != "unavailable" || out["firstRecordedAt"] != nil {
		t.Fatalf("unrecorded history was reported as measured usage: %v", out)
	}
	if got := f.call("GET", calls+"?limit=1", nil, "", 200); len(threadItems(t, got)) != 0 || got.S("nextCursor") != "" {
		t.Fatalf("unrecorded calls=%v", got)
	}
	stranger, _ := f.addUser(t, "statistics-outsider", "Statistics outsider")
	unknown := f.user
	unknown.Subject = "statistics-unregistered"
	foreignTenant := f.moreTenant()
	otherIssue := f.moreIssue(scene)
	for _, suffix := range []string{"statistics", "model-calls"} {
		path := base + suffix
		for _, caller := range []core.Claims{stranger, unknown} {
			sessionDashboardFault(t, f, caller, path, 403, "membership_required")
		}
		for _, query := range []string{"historyKey=secret", "url=https%3A%2F%2Fexample.invalid", "limit=", "limit=1&limit=2", "unknown=a;b", "unknown=%zz", "before=" + scene.runID} {
			sessionDashboardFault(t, f, f.user, path+"?"+query, 400, "invalid_session_filter")
		}
		for _, invalid := range []string{
			strings.Replace(path, scene.tenantID, foreignTenant, 1),
			strings.Replace(path, scene.issueID, otherIssue, 1),
			strings.Replace(path, scene.runID, uuid.NewString(), 1),
		} {
			sessionDashboardFault(t, f, f.user, invalid, 404, "not_found")
		}
	}
	if f.scalar(`SELECT count(*) FROM user_identities WHERE subject=$1`, unknown.Subject) != 0 {
		t.Fatal("statistics GET provisioned identity")
	}
	for _, query := range []string{"limit=1", "after=" + uuid.NewString()} {
		sessionDashboardFault(t, f, f.user, statistics+"?"+query, 400, "invalid_session_filter")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=1.5", "after=not-a-uuid", "after=" + uuid.NewString()} {
		sessionDashboardFault(t, f, f.user, calls+"?"+query, 400, "invalid_pagination")
	}
	_, err := f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, scene.runID)
	must(t, err)
	for _, path := range []string{statistics, calls} {
		sessionDashboardFault(t, f, f.user, path, 404, "not_found")
	}
}

func TestMigration0035ModelAttemptsFreshAndUpgradeWithoutBackfill(t *testing.T) {
	verify := func(t *testing.T, pool *sql.DB) {
		t.Helper()
		if !tableExists(t, pool, "model_request_attempts") {
			t.Fatal("missing attempt ledger")
		}
		var rows, constraints, indexes int
		must(t, pool.QueryRow(`SELECT count(*) FROM model_request_attempts`).Scan(&rows))
		must(t, pool.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid='model_request_attempts'::regclass AND contype='f'`).Scan(&constraints))
		must(t, pool.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='model_attempt_run_order'`).Scan(&indexes))
		if rows != 0 || constraints != 3 || indexes != 1 {
			t.Fatalf("rows=%d scoped constraints=%d indexes=%d", rows, constraints, indexes)
		}
	}
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			pool, _ := testSchema(t, "test_ma35_")
			if upgrade {
				applyMigrationsUpTo(t, pool, migrationsBefore(t, "0035_model_attempt_statistics.sql"))
				if tableExists(t, pool, "model_request_attempts") {
					t.Fatal("pre-upgrade schema unexpectedly has attempt ledger")
				}
				_, err := pool.Exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Preserved upgrade identity','active')`, uuid.NewString())
				must(t, err)
			}
			migratedTwice(t, newStoreOnSchema(t, pool))
			verify(t, pool)
			if upgrade {
				var count int
				must(t, pool.QueryRow(`SELECT count(*) FROM users WHERE display_name='Preserved upgrade identity'`).Scan(&count))
				if count != 1 {
					t.Fatal("upgrade changed existing identity")
				}
			}
		})
	}
}
