package core

import (
	"context"
	"strings"
	"testing"
)

// DB tests for the scheduled-run business verdict (specs decisions/cloud/scheduled-task/
// 20261022-run-verdict-and-artifacts.md D1/D2). The value only ever arrives through the executor's
// terminal session result, so these tests drive the shipped settlement and the persistence helper
// against a real isolated PostgreSQL schema rather than waiting on a Node.

// stageScheduledOccurrence makes a seeded run an admitted scheduled occurrence, which is the one
// authoritative origin fact that lets it carry a business verdict. The definition row is derived
// from the run's own tenant and issue so the ledger's foreign keys stay real.
func stageScheduledOccurrence(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var taskID string
	if err := store.Pool.QueryRow(`
		WITH run AS (SELECT tenant_id, issue_id FROM issue_runs WHERE id=$1)
		INSERT INTO scheduled_tasks
			(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled)
		SELECT gen_random_uuid(), run.tenant_id, run.issue_id,
		       (SELECT id FROM users ORDER BY id LIMIT 1),
		       (SELECT id FROM space_agents ORDER BY id LIMIT 1),
		       'verdict-test','review the change','{"kind":"daily","hour":0,"minute":0}'::jsonb,'UTC',1,1,false
		FROM run
		RETURNING id`, runID).Scan(&taskID); err != nil {
		t.Fatalf("stage scheduled definition: %v", err)
	}
	if _, err := store.Pool.Exec(`
		INSERT INTO scheduled_task_occurrences(tenant_id,task_id,scheduled_at,outcome,task_revision,observed_at,run_id)
		SELECT tenant_id,$2,now(),'admitted',1,now(),$1 FROM issue_runs WHERE id=$1`, runID, taskID); err != nil {
		t.Fatalf("stage scheduled occurrence: %v", err)
	}
	return taskID
}

// scheduledResultObject is the structured value an executor reports for one concluded run.
func scheduledResultObject(verdict, detail string) Object {
	result := Object{"verdict": verdict}
	if detail != "" {
		result["verdictDetail"] = detail
	}
	return result
}

// recordedResult reads the stored verdict of one run, or nil when it reported none.
func recordedResult(t *testing.T, store *Store, runID string) Object {
	t.Helper()
	var verdict, detail string
	var occurrence *string
	var scheduledAt string
	err := store.Pool.QueryRow(`
		SELECT verdict, COALESCE(verdict_detail,''), occurrence_id::text, scheduled_at::text
		FROM scheduled_run_results WHERE issue_run_id=$1`, runID).Scan(&verdict, &detail, &occurrence, &scheduledAt)
	if err != nil {
		return nil
	}
	return Object{"verdict": verdict, "verdictDetail": detail, "occurrenceId": occurrence, "scheduledAt": scheduledAt}
}

// The terminal session result of a scheduled run writes its verdict in the same transaction as the
// Thread's terminal state.
//
// The store carries no business hooks here: the delivery work item this same transaction declares is
// not what this test is about, and with object storage unconfigured the hook is what settles it. The
// verdict write must not depend on that settlement, which is exactly what the assertion shows.
func TestSessionEndedWritesTheScheduledVerdict(t *testing.T) {
	store := &Store{Pool: coreFixturePool(t, func(cleanup func() error) {
		t.Cleanup(func() {
			if err := cleanup(); err != nil {
				t.Error(err)
			}
		})
	})}
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	ended := Object{
		"outcome":         "agent_session_ended",
		"reason":          "idle_timeout",
		"scheduledResult": scheduledResultObject("fail", "two critical defects"),
	}
	var settleErr error
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		settleErr = store.settleSessionEnded(tx, scene.run, scene.execution, ended)
		return Object{}
	}); err != nil {
		t.Fatalf("settle transaction: %v", err)
	}
	if settleErr != nil {
		t.Fatalf("settle the scheduled session end: %v", settleErr)
	}
	recorded := recordedResult(t, store, scene.run)
	if recorded == nil {
		t.Fatal("the reported verdict was not written")
	}
	// A delivering run and a failing business result coexist: the verdict never derives from, or
	// rewrites, the execution-layer fact.
	if recorded.S("verdict") != "fail" || recorded.S("verdictDetail") != "two critical defects" {
		t.Fatalf("stored verdict = %+v, want fail with its detail", recorded)
	}
	var phase string
	if err := store.Pool.QueryRow(`SELECT phase FROM issue_runs WHERE id=$1`, scene.run).Scan(&phase); err != nil || phase != "delivering" {
		t.Fatalf("run phase = %q (err %v), want delivering", phase, err)
	}
}

// A verdict is written at most once: a second report for the same run cannot overwrite the first.
func TestScheduledRunResultIsWrittenOncePerRun(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if e := recordScheduledRunResult(tx, scene.run, Object{"reason": "idle_timeout", "scheduledResult": scheduledResultObject("fail", "first")}); e != nil {
			t.Fatalf("write the first verdict: %v", e)
		}
		if e := recordScheduledRunResult(tx, scene.run, Object{"reason": "idle_timeout", "scheduledResult": scheduledResultObject("pass", "second")}); e != nil {
			t.Fatalf("repeat the verdict report: %v", e)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	recorded := recordedResult(t, store, scene.run)
	if recorded == nil || recorded.S("verdict") != "fail" || recorded.S("verdictDetail") != "first" {
		t.Fatalf("stored verdict = %+v, want the first report to win", recorded)
	}
	var count int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM scheduled_run_results WHERE issue_run_id=$1`, scene.run).Scan(&count); err != nil || count != 1 {
		t.Fatalf("result rows = %d (err %v), want exactly one", count, err)
	}
}

// Only an admitted scheduled run that Cloud ended as asked may carry a verdict. An unattributable
// report is refused, and an end reason that never completed a turn stores nothing.
func TestScheduledRunResultRefusesUnattributableReports(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)
	longDetail := strings.Repeat("x", MaxScheduledRunVerdictDetailRunes+1)
	report := func(t *testing.T, reason string, result Object) error {
		t.Helper()
		var writeErr error
		if _, err := store.transact(context.Background(), func(tx *transaction) Object {
			writeErr = recordScheduledRunResult(tx, scene.run, Object{"reason": reason, "scheduledResult": result})
			return Object{}
		}); err != nil {
			t.Fatalf("transaction: %v", err)
		}
		return writeErr
	}

	// A manual run has no authoritative task identity, so a reported verdict cannot be attributed.
	if err := report(t, "idle_timeout", scheduledResultObject("idle", "")); err == nil {
		t.Fatal("a verdict on a manual run must be refused")
	}
	if recordedResult(t, store, scene.run) != nil {
		t.Fatal("a refused report must store nothing")
	}

	stageScheduledOccurrence(t, store, scene.run)
	for name, tc := range map[string]struct {
		reason    string
		result    Object
		wantError bool
	}{
		"cancelled end":    {reason: "cancelled", result: scheduledResultObject("idle", "")},
		"failed end":       {reason: "agent_failed", result: scheduledResultObject("idle", "")},
		"unknown verdict":  {reason: "idle_timeout", result: scheduledResultObject("banana", ""), wantError: true},
		"over-long detail": {reason: "idle_timeout", result: scheduledResultObject("fail", longDetail), wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			err := report(t, tc.reason, tc.result)
			if (err != nil) != tc.wantError {
				t.Fatalf("report error = %v, want an error: %v", err, tc.wantError)
			}
			if recordedResult(t, store, scene.run) != nil {
				t.Fatal("nothing may be stored for this report")
			}
		})
	}
}

// The frozen task identity survives the 100-entry history trim: clearing the occurrence reference
// keeps task_id, scheduled_at and the run.
func TestScheduledRunResultSurvivesOccurrenceTrim(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)
	taskID := stageScheduledOccurrence(t, store, scene.run)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if e := recordScheduledRunResult(tx, scene.run, Object{"reason": "idle_timeout", "scheduledResult": scheduledResultObject("blocked", "environment offline")}); e != nil {
			t.Fatalf("write the verdict: %v", e)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if _, err := store.Pool.Exec(`DELETE FROM scheduled_task_occurrences WHERE run_id=$1`, scene.run); err != nil {
		t.Fatalf("trim the occurrence: %v", err)
	}
	var storedTask, storedAt string
	var occurrence *string
	if err := store.Pool.QueryRow(`SELECT task_id::text, scheduled_at::text, occurrence_id::text
		FROM scheduled_run_results WHERE issue_run_id=$1`, scene.run).Scan(&storedTask, &storedAt, &occurrence); err != nil {
		t.Fatalf("the verdict must survive the trim: %v", err)
	}
	if storedTask != taskID || occurrence != nil || storedAt == "" {
		t.Fatalf("after trim task=%q occurrence=%v scheduledAt=%q; want the frozen task and no occurrence reference", storedTask, occurrence, storedAt)
	}
}
