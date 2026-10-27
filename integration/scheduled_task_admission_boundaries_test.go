package integration

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/wanglongan587/cloud/internal/core"
)

// scheduledTaskReplicas builds the scheduledTaskStore scene and returns it together with a second
// Store on its own connection pool over the same schema, which is what a second Cloud replica (or a
// restarted process) is to PostgreSQL: no shared Go state, only the database.
func scheduledTaskReplicas(t *testing.T) (*core.Store, *core.Store, skeletonSeed, string) {
	t.Helper()
	pool, config := testSchema(t, "test_scheduled_")
	store := newStoreOnSchema(t, pool)
	store.ScheduledTaskAcceptanceEnabled = true
	must(t, store.Migrate(context.Background()))
	s := seedAgentIssueRunSkeleton(t, pool)
	_, err := pool.Exec(`UPDATE issues SET project_ref=$1 WHERE id=$2`, s.projectID, s.issueID)
	must(t, err)
	agentID := seedScheduledTaskAgent(t, pool, s)
	seedScheduledTaskModelChain(t, store, s.userID)
	return store, scheduledTaskReplica(t, config), s, agentID
}

// scheduledTaskReplica opens one more independent Store on the schema the config points at. The pool
// is closed before the schema is dropped because cleanups run in reverse registration order.
func scheduledTaskReplica(t *testing.T, config *pgx.ConnConfig) *core.Store {
	t.Helper()
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = pool.Close() })
	replica := newStoreOnSchema(t, pool)
	replica.ScheduledTaskAcceptanceEnabled = true
	return replica
}

// scheduledTaskRunCount counts the IssueRuns the scheduled agent holds on the bound issue. The
// skeleton's own run uses a different executor, so this is exactly the runs admissions created.
func scheduledTaskRunCount(t *testing.T, pool *sql.DB, s skeletonSeed, agentID string) int {
	t.Helper()
	var count int
	must(t, pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE issue_id=$1 AND executor_type='agent' AND executor_id=$2`,
		s.issueID, agentID).Scan(&count))
	return count
}

// TestScheduledTaskReplicasAdmitADuePointOnce pins D2 invariant 2 across replicas: two independent
// Stores evaluating the same due point concurrently admit it exactly once. The occurrence primary
// key decides the winner; every loser reads the existing row and leaves, so no evaluation errors
// and no second IssueRun appears.
func TestScheduledTaskReplicasAdmitADuePointOnce(t *testing.T) {
	first, second, s, agentID := scheduledTaskReplicas(t)
	taskID := seedEnabledScheduledTask(t, first, s, agentID, "Concurrent replicas", 65)

	const passesPerReplica = 4
	start := make(chan struct{})
	errs := make(chan error, 2*passesPerReplica)
	var wg sync.WaitGroup
	for _, replica := range []*core.Store{first, second} {
		for range passesPerReplica {
			wg.Add(1)
			go func(store *core.Store) {
				defer wg.Done()
				<-start
				errs <- store.EvaluateScheduledTasksOnce(context.Background())
			}(replica)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent evaluation must not fail, got %v", err)
		}
	}

	if count, outcome := occurrenceCount(t, first, taskID); count != 1 || outcome != "admitted" {
		t.Fatalf("concurrent replicas must admit the due point once, got %d occurrence(s) outcome %q", count, outcome)
	}
	if runs := scheduledTaskRunCount(t, first.Pool, s, agentID); runs != 1 {
		t.Fatalf("concurrent replicas must create exactly one IssueRun, got %d", runs)
	}
}

// TestScheduledTaskRestartReplayKeepsTheAdmission pins D2 invariant 2 across a restart: a fresh
// process re-evaluating an already admitted due point returns the existing result, even after the
// admitted run has finished and the task is no longer busy.
func TestScheduledTaskRestartReplayKeepsTheAdmission(t *testing.T) {
	first, restarted, s, agentID := scheduledTaskReplicas(t)
	taskID, runID := admitScheduledRun(t, first, s, agentID, "Restart replay")
	before := readScheduledOccurrenceIdentity(t, first, taskID)
	_, err := first.Pool.Exec(`UPDATE issue_runs SET status='completed', phase='done' WHERE id=$1`, runID)
	must(t, err)

	must(t, restarted.EvaluateScheduledTasksOnce(context.Background()))

	if count, _ := occurrenceCount(t, first, taskID); count != 1 {
		t.Fatalf("a replay after restart must not add an occurrence, got %d", count)
	}
	if after := readScheduledOccurrenceIdentity(t, first, taskID); after != before {
		t.Fatalf("a replay after restart must return the existing result: before=%+v after=%+v", before, after)
	}
	if runs := scheduledTaskRunCount(t, first.Pool, s, agentID); runs != 1 {
		t.Fatalf("a replay after restart must not create another IssueRun, got %d", runs)
	}
}

// TestScheduledTaskEnableAPIReanchorsTheDisabledInterval pins D2 停用区间不评估、不补记 through the
// public endpoints rather than a direct enabled_at write: manual disable keeps the old anchor, so it
// is the enable endpoint's re-anchoring that keeps every due point swallowed by the disabled
// interval from being admitted or recorded as missed.
func TestScheduledTaskEnableAPIReanchorsTheDisabledInterval(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Disabled interval over the API", 5)
	creator := scheduledTaskCaller(t, store, s.tenantID)
	base := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID

	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "interval-disable", Path: base + "/disable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("disable = %d, want 200", status)
	}
	// Let the disabled interval swallow two hourly due points.
	_, err = store.Pool.Exec(`UPDATE scheduled_tasks SET enabled_at = enabled_at - interval '150 minutes' WHERE id=$1`, taskID)
	must(t, err)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	if count, _ := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("a disabled task must evaluate to no occurrence, got %d", count)
	}

	_, status, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "interval-enable", Path: base + "/enable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator, Body: core.Object{},
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("enable = %d, want 200", status)
	}
	var anchorAge float64
	must(t, store.Pool.QueryRow(`SELECT EXTRACT(EPOCH FROM now() - enabled_at) FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&anchorAge))
	if anchorAge < 0 || anchorAge > 60 {
		t.Fatalf("enable must re-anchor enabled_at at the database's now(), got an anchor %v seconds old", anchorAge)
	}

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	if count, outcome := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("re-enabling must not admit or back-fill the disabled interval, got %d %q", count, outcome)
	}
}

// TestScheduledTaskReadsProjectTheNextDuePoint pins the read-side nextDueAt projection through the
// public endpoints: it follows the enable anchor on the worker's every_hours grid, is absent while the
// task is disabled, and moves to the new anchor after enable. Nothing is persisted for it.
func TestScheduledTaskReadsProjectTheNextDuePoint(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Next due projection", 150)
	creator := scheduledTaskCaller(t, store, s.tenantID)
	list := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"
	base := list + "/" + taskID
	call := func(method, key, path, id string) core.Object {
		t.Helper()
		out, status, err := store.Public(context.Background(), &core.PublicRequest{
			Method: method, Key: key, Path: path,
			TenantID: s.tenantID, ScheduledTaskID: id, Identity: creator, Body: core.Object{},
		})
		must(t, err)
		if status != 200 {
			t.Fatalf("%s %s = %d, want 200", method, path, status)
		}
		return out
	}
	// The hourly grid anchored 150 minutes ago has its next point 30 minutes after the anchor's
	// third hour, computed here from the stored anchor rather than the test's clock.
	expected := func() time.Time {
		t.Helper()
		var want time.Time
		must(t, store.Pool.QueryRow(`SELECT enabled_at + (floor(EXTRACT(EPOCH FROM now() - enabled_at) / 3600) + 1) * interval '1 hour'
			FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&want))
		return want
	}
	nextDue := func(task core.Object) any {
		t.Helper()
		// Management writes answer with the definition under "resource"; reads return it bare.
		if resource, ok := task["resource"].(core.Object); ok {
			task = resource
		}
		value, ok := task["nextDueAt"]
		if !ok {
			t.Fatalf("task read has no nextDueAt key: %v", task)
		}
		return value
	}
	assertAt := func(label string, got any, want time.Time) {
		t.Helper()
		text, _ := got.(string)
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !at.Equal(want) {
			t.Fatalf("%s nextDueAt = %v, want %v", label, got, want.UTC())
		}
	}

	want := expected()
	assertAt("GET", nextDue(call("GET", "next-get", base, taskID)), want)
	listed, _ := call("GET", "next-list", list, "")["tasks"].([]core.Object)
	if len(listed) != 1 {
		t.Fatalf("list = %v, want the one task", listed)
	}
	assertAt("list", nextDue(listed[0]), want)

	if got := nextDue(call("POST", "next-disable", base+"/disable", taskID)); got != nil {
		t.Fatalf("disabled nextDueAt = %v, want none", got)
	}
	if got := nextDue(call("GET", "next-get-disabled", base, taskID)); got != nil {
		t.Fatalf("disabled read nextDueAt = %v, want none", got)
	}

	enabled := call("POST", "next-enable", base+"/enable", taskID)
	var anchor time.Time
	must(t, store.Pool.QueryRow(`SELECT enabled_at FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&anchor))
	assertAt("re-enabled", nextDue(enabled), anchor.Add(time.Hour))
	if count, _ := occurrenceCount(t, store, taskID); count != 0 {
		t.Fatalf("reads must not write occurrences, got %d", count)
	}
}

// TestScheduledTaskAdmissionSnapshotsTheSpacePluginVersion pins D1/invariant 6: the Agent plugin
// version is not frozen on the definition but read from the Space at each admission. After the Space
// selects a new version, the next admission snapshots the new value and the already admitted run
// keeps the version it was admitted with.
func TestScheduledTaskAdmissionSnapshotsTheSpacePluginVersion(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	_, err := store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,selected_release)
		VALUES($1,$2,'official','ora-space.opencode','installed','1.0.0','installed','{"id":"official/ora-space.opencode","version":"1.0.0"}'::jsonb)`,
		s.spaceID, s.tenantID)
	must(t, err)
	taskID, firstRun := admitScheduledRun(t, store, s, agentID, "Plugin snapshot")
	pluginVersion := func(runID string) string {
		t.Helper()
		var version string
		must(t, store.Pool.QueryRow(`SELECT COALESCE(input->>'agentPluginVersion','') FROM issue_runs WHERE id=$1`, runID).Scan(&version))
		return version
	}
	if got := pluginVersion(firstRun); got != "1.0.0" {
		t.Fatalf("admission must snapshot the Space's selected version, got %q", got)
	}

	// Finish the first run so the task is not busy, select a new version in the Space, and move the
	// task one hour back so the next hourly point is due now.
	_, err = store.Pool.Exec(`UPDATE issue_runs SET status='completed', phase='done' WHERE id=$1`, firstRun)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE space_plugins SET desired_version='1.1.0',
		selected_release='{"id":"official/ora-space.opencode","version":"1.1.0"}'::jsonb
		WHERE space_id=$1 AND source_namespace='official' AND identifier='ora-space.opencode'`, s.spaceID)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE scheduled_task_occurrences SET scheduled_at = scheduled_at - interval '1 hour' WHERE task_id=$1`, taskID)
	must(t, err)
	_, err = store.Pool.Exec(`UPDATE scheduled_tasks SET enabled_at = enabled_at - interval '1 hour' WHERE id=$1`, taskID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var secondRun string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences
		WHERE task_id=$1 AND run_id IS NOT NULL AND run_id <> $2`, taskID, firstRun).Scan(&secondRun))
	if got := pluginVersion(secondRun); got != "1.1.0" {
		t.Fatalf("the next admission must snapshot the newly selected version, got %q", got)
	}
	if got := pluginVersion(firstRun); got != "1.0.0" {
		t.Fatalf("an admitted run must keep the version it was admitted with, got %q", got)
	}
}

// TestScheduledTaskAutoDisableDoesNotRecover pins D1/D5: once a task auto-disabled because its creator
// lost membership or its bound issue was deleted, restoring the condition does not re-enable it.
// Only the creator's explicit enable may do that, so the next pass leaves the task disabled with its
// reason and admits nothing.
func TestScheduledTaskAutoDisableDoesNotRecover(t *testing.T) {
	cases := []struct {
		name, reason, invalidate, heal string
		issueSide                      bool
	}{
		{
			name: "creator inactive", reason: "creator_inactive",
			invalidate: `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`,
			heal:       `UPDATE tenant_memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2`,
		},
		{
			name: "issue deleted", reason: "issue_deleted", issueSide: true,
			invalidate: `UPDATE issues SET deleted_at=now() WHERE tenant_id=$1 AND id=$2`,
			heal:       `UPDATE issues SET deleted_at=NULL WHERE tenant_id=$1 AND id=$2`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			// A second administrator lets the creator's membership be disabled past the last-admin guard.
			_, err := store.Pool.Exec(`
				WITH backup AS (
					INSERT INTO users(id, display_name, status)
					VALUES(gen_random_uuid(), 'Scheduled-task backup administrator', 'active')
					RETURNING id
				)
				INSERT INTO tenant_memberships(tenant_id, user_id, role, status)
				SELECT $1, id, 'admin', 'active' FROM backup`, s.tenantID)
			must(t, err)
			taskID := seedEnabledScheduledTask(t, store, s, agentID, "No recovery "+tc.name, 65)
			subject := s.userID
			if tc.issueSide {
				subject = s.issueID
			}
			_, err = store.Pool.Exec(tc.invalidate, s.tenantID, subject)
			must(t, err)
			must(t, store.EvaluateScheduledTasksOnce(context.Background()))
			if state := readScheduledTaskState(t, store, taskID); state.enabled || state.disabledReason != tc.reason {
				t.Fatalf("precondition: task must auto-disable with %q, got %+v", tc.reason, state)
			}

			_, err = store.Pool.Exec(tc.heal, s.tenantID, subject)
			must(t, err)
			must(t, store.EvaluateScheduledTasksOnce(context.Background()))

			state := readScheduledTaskState(t, store, taskID)
			if state.enabled || state.hasEnabledAt || state.disabledReason != tc.reason {
				t.Fatalf("restoring the condition must not re-enable the task, got %+v", state)
			}
			if count, _ := occurrenceCount(t, store, taskID); count != 0 {
				t.Fatalf("an auto-disabled task must stay out of admission after the condition heals, got %d", count)
			}
		})
	}
}

// scheduledRunFacts is the IssueRun state a consent revocation must leave exactly as it was.
type scheduledRunFacts struct {
	status, phase, cancelRequested, input string
	version                               int64
}

func readScheduledRunFacts(t *testing.T, store *core.Store, runID string) scheduledRunFacts {
	t.Helper()
	var facts scheduledRunFacts
	must(t, store.Pool.QueryRow(`SELECT status, COALESCE(phase,''), COALESCE(cancel_requested_at::text,''), input::text, version
		FROM issue_runs WHERE id=$1`, runID).Scan(&facts.status, &facts.phase, &facts.cancelRequested, &facts.input, &facts.version))
	return facts
}

// TestScheduledTaskRevokeConsentKeepsExistingRunFacts pins D5: revoking the unattended consent only
// disables the task for future admissions. The live run it already admitted is neither cancelled nor
// rewritten, and its occurrence keeps its admission identity.
func TestScheduledTaskRevokeConsentKeepsExistingRunFacts(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID, runID := admitScheduledRun(t, store, s, agentID, "Revoke keeps run facts")
	runBefore := readScheduledRunFacts(t, store, runID)
	occurrenceBefore := readScheduledOccurrenceIdentity(t, store, taskID)

	creator := scheduledTaskCaller(t, store, s.tenantID)
	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "revoke-live-run", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/revoke-consent",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("revoke-consent = %d, want 200", status)
	}
	if state := readScheduledTaskState(t, store, taskID); state.enabled || state.disabledReason != "consent_revoked" {
		t.Fatalf("precondition: revoking consent must disable the task, got %+v", state)
	}

	if runAfter := readScheduledRunFacts(t, store, runID); runAfter != runBefore {
		t.Fatalf("revoking consent must not rewrite the admitted run: before=%+v after=%+v", runBefore, runAfter)
	}
	if occurrenceAfter := readScheduledOccurrenceIdentity(t, store, taskID); occurrenceAfter != occurrenceBefore {
		t.Fatalf("revoking consent must keep the occurrence: before=%+v after=%+v", occurrenceBefore, occurrenceAfter)
	}
}
