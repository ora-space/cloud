package integration

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// These DB tests are the minimal direct verification the review asked for (reviewer blocking item
// #8). They drive the shipped worker and PATCH writer the way production does: EvaluateScheduledTasksOnce
// and Store.Public, against a real PostgreSQL schema. NOT_VERIFIED locally: this environment has no
// network to fetch the integration test dependencies (pgx), so the suite only runs in CI.

const scheduledTaskModelID = "vendor/model-v1"

// seedScheduledTaskModelChain gives the creator a usable personal default backed by an enabled,
// credential-bound connection, so creatorModelReady and bindRunModel both resolve.
func seedScheduledTaskModelChain(t *testing.T, store *core.Store, userID string) {
	t.Helper()
	conn := uuid.NewString()
	cred := uuid.NewString()
	_, err := store.Pool.Exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id)
		VALUES($1,$2,repeat('a',64),'scheduled-task-test')`, cred, userID)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled)
		VALUES($1,$2,$3,'Personal Provider','openai-completions','https://models.example.invalid/v1','bearer',$4::jsonb,true)`,
		conn, userID, cred, `[{"id":"`+scheduledTaskModelID+`","name":"Model V1","contextWindow":128000,"maxTokens":8192}]`)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO personal_model_defaults(user_id,connection_id,model_id,version) VALUES($1,$2,$3,1)`, userID, conn, scheduledTaskModelID)
	must(t, err)
}

// seedEnabledScheduledTask inserts an enabled every_hours task due `enabledAgoMinutes` ago.
func seedEnabledScheduledTask(t *testing.T, store *core.Store, s skeletonSeed, agentID, name string, enabledAgoMinutes int) string {
	t.Helper()
	taskID := uuid.NewString()
	_, err := store.Pool.Exec(`INSERT INTO scheduled_tasks
		(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled,enabled_at,unattended_policy,unattended_consented_at)
		VALUES($1,$2,$3,$4,$5,$6,'Sync','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1,true,now() - $7 * interval '1 minute','full_auto_in_cloud_workspace',now())`,
		taskID, s.tenantID, s.issueID, s.userID, agentID, name, enabledAgoMinutes)
	must(t, err)
	return taskID
}

// scheduledTaskStore builds a migrated store with the issue bound to the project and the agent's
// model chain in place, ready for an admission.
func scheduledTaskStore(t *testing.T) (*core.Store, skeletonSeed, string) {
	t.Helper()
	pool, _ := testSchema(t, "test_scheduled_")
	store := newStoreOnSchema(t, pool)
	store.ScheduledTaskAcceptanceEnabled = true
	must(t, store.Migrate(context.Background()))
	s := seedAgentIssueRunSkeleton(t, pool)
	_, err := pool.Exec(`UPDATE issues SET project_ref=$1 WHERE id=$2`, s.projectID, s.issueID)
	must(t, err)
	agentID := seedScheduledTaskAgent(t, pool, s)
	seedScheduledTaskModelChain(t, store, s.userID)
	return store, s, agentID
}

func occurrenceCount(t *testing.T, store *core.Store, taskID string) (int, string) {
	t.Helper()
	var count int
	var outcome string
	err := store.Pool.QueryRow(`SELECT count(*), COALESCE(min(outcome),'') FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&count, &outcome)
	must(t, err)
	return count, outcome
}

// TestScheduledTaskAdmissionCreatesARun (FIX-1): the admission names the task UUID as the run's
// trigger evidence and creates an IssueRun.
func TestScheduledTaskAdmissionCreatesARun(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Admission", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	// A duplicate scan for the same due point must not create a second occurrence or IssueRun.
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	count, outcome := occurrenceCount(t, store, taskID)
	if count != 1 || outcome != "admitted" {
		t.Fatalf("want exactly one admitted occurrence, got %d with outcome %q", count, outcome)
	}
	var runID, ref string
	err := store.Pool.QueryRow(`SELECT r.id, r.trigger_evidence_ref_id::text FROM scheduled_task_occurrences o JOIN issue_runs r ON r.id=o.run_id WHERE o.task_id=$1`, taskID).Scan(&runID, &ref)
	must(t, err)
	if runID == "" || ref != taskID {
		t.Fatalf("admission must name the task UUID as trigger evidence, got run %q ref %q", runID, ref)
	}
}

// TestScheduledTaskHistoryReadsEmptyAndAdmitted verifies the public history path against PostgreSQL
// before and after admission. IssueRun has no session_id fact, so history must not manufacture one.
func TestScheduledTaskHistoryReadsEmptyAndAdmitted(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "History", 65)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/history"
	readHistory := func(key string) core.Object {
		out, status, err := store.Public(context.Background(), &core.PublicRequest{
			Method:          "GET",
			Key:             key,
			Path:            path,
			TenantID:        s.tenantID,
			ScheduledTaskID: taskID,
			Identity:        caller,
		})
		must(t, err)
		if status != 200 {
			t.Fatalf("history read status = %d, want 200", status)
		}
		return out
	}
	if entries, ok := readHistory("scheduled-history-empty")["entries"].([]core.Object); !ok || len(entries) != 0 {
		t.Fatalf("empty history entries = %v, want an empty list", entries)
	}

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	entries, ok := readHistory("scheduled-history-admitted")["entries"].([]core.Object)
	if !ok || len(entries) != 1 || entries[0].S("runId") == "" {
		t.Fatalf("admitted history entries = %v, want one entry with a run ID", entries)
	}
	if _, exists := entries[0]["sessionId"]; exists {
		t.Fatalf("history must not expose a session ID without an IssueRun fact: %v", entries[0])
	}
	// D6: an admitted run that has not reported yet shows a null verdict - "not reported" is a fact
	// the reader can see - and the artifact counts are zero rather than absent.
	if verdict, exists := entries[0]["verdict"]; !exists || verdict != nil {
		t.Fatalf("unreported verdict = %v (present %t), want an explicit null", verdict, exists)
	}
	if _, exists := entries[0]["verdictDetail"]; exists {
		t.Fatalf("a missing verdict must not carry a detail: %v", entries[0])
	}
	if entries[0].N("artifactCount") != 0 || entries[0].N("rejectedArtifactCount") != 0 {
		t.Fatalf("artifact counts = %v, want zero for a run without artifacts", entries[0])
	}
	if entries[0].B("artifactsExpired") {
		t.Fatal("a run that never registered artifacts must not look expired")
	}
	runID := entries[0].S("runId")
	_, err := store.Pool.Exec(`INSERT INTO scheduled_run_artifact_deliveries
		(issue_run_id,tenant_id,task_id,occurrence_id,scheduled_at,state,deadline_at,settled_at)
		SELECT run_id,tenant_id,task_id,id,scheduled_at,'delivered',now(),now()
		FROM scheduled_task_occurrences WHERE run_id=$1`, runID)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO scheduled_run_artifacts
		(tenant_id,issue_run_id,task_id,occurrence_id,scheduled_at,artifact_index,object_key,display_name,kind,size_bytes,sha256)
		SELECT tenant_id,run_id,task_id,id,scheduled_at,0,'test-artifact','result.txt','text',5,repeat('a',64)
		FROM scheduled_task_occurrences WHERE run_id=$1`, runID)
	must(t, err)
	available := readHistory("scheduled-history-available")["entries"].([]core.Object)[0]
	if available.B("artifactsExpired") || available.N("artifactCount") != 1 {
		t.Fatalf("registered artifacts must remain available: %v", available)
	}
	_, err = store.Pool.Exec("DELETE FROM scheduled_run_artifacts WHERE issue_run_id=$1", runID)
	must(t, err)
	for _, state := range []string{"delivered", "partial", "failed", "skipped"} {
		_, err = store.Pool.Exec("UPDATE scheduled_run_artifact_deliveries SET state=$1 WHERE issue_run_id=$2", state, runID)
		must(t, err)
		item := readHistory("scheduled-history-" + state)["entries"].([]core.Object)[0]
		wantExpired := state == "delivered" || state == "partial"
		if item.B("artifactsExpired") != wantExpired || item.N("artifactCount") != 0 {
			t.Fatalf("state %s history = %v; want expired=%t and zero registered files", state, item, wantExpired)
		}
	}
}

// TestScheduledTaskMissedRecoverySingleOccurrence (FIX-5): a long outage writes exactly one missed.
func TestScheduledTaskMissedRecoverySingleOccurrence(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Missed recovery", 150)
	// Two passes: the second must not write another missed point.
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	count, outcome := occurrenceCount(t, store, taskID)
	if count != 1 || outcome != "missed" {
		t.Fatalf("outage must leave one missed occurrence, got %d with outcome %q", count, outcome)
	}
}

// TestScheduledTaskSingleFailureDoesNotBlock (FIX-4): a task whose agent is gone must not starve a
// healthy task in the same pass.
func TestScheduledTaskSingleFailureDoesNotBlock(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	badAgentID := seedScheduledTaskAgentWithPlugin(t, store.Pool, s, "official/ora-space.opencode-retired")
	_, err := store.Pool.Exec(`UPDATE space_agents SET status='retired' WHERE id=$1`, badAgentID)
	must(t, err)
	badTask := seedEnabledScheduledTask(t, store, s, badAgentID, "Retired agent", 65)
	goodTask := seedEnabledScheduledTask(t, store, s, agentID, "Healthy agent", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	badCount, badOutcome := occurrenceCount(t, store, badTask)
	goodCount, goodOutcome := occurrenceCount(t, store, goodTask)
	if badCount != 1 || badOutcome != "failed" {
		t.Fatalf("bad task must record one failed occurrence, got %d %q", badCount, badOutcome)
	}
	if goodCount != 1 || goodOutcome != "admitted" {
		t.Fatalf("healthy task must admit despite the bad task, got %d %q", goodCount, goodOutcome)
	}
}

// TestScheduledTaskCorruptDefinitionDoesNotBlock keeps an invalid persisted definition from
// starving a later healthy task. The corrupt row remains untouched and is still returned to the
// sync loop as an error; only its own short transaction is rolled back.
func TestScheduledTaskCorruptDefinitionDoesNotBlock(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	badTask := seedEnabledScheduledTask(t, store, s, agentID, "Corrupt timezone", 65)
	goodTask := seedEnabledScheduledTask(t, store, s, agentID, "Healthy after corruption", 65)
	_, err := store.Pool.Exec(`UPDATE scheduled_tasks
		SET timezone='Not/AZone', updated_at=now() - interval '1 minute'
		WHERE id=$1`, badTask)
	must(t, err)

	if err := store.EvaluateScheduledTasksOnce(context.Background()); err == nil {
		t.Fatal("corrupt definition must remain visible to the sync loop")
	}
	badCount, _ := occurrenceCount(t, store, badTask)
	goodCount, goodOutcome := occurrenceCount(t, store, goodTask)
	if badCount != 0 {
		t.Fatalf("corrupt definition must not create an occurrence, got %d", badCount)
	}
	if goodCount != 1 || goodOutcome != "admitted" {
		t.Fatalf("healthy task must admit despite corrupt peer, got %d %q", goodCount, goodOutcome)
	}
}

// TestScheduledTaskBusyRunSkipsTheNextOccurrence keeps a run that is still releasing from being
// treated as terminal merely because its business result is already completed.
func TestScheduledTaskBusyRunSkipsTheNextOccurrence(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Busy run", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var runID string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&runID))
	_, err := store.Pool.Exec(`UPDATE issue_runs SET phase='releasing', status='completed' WHERE id=$1`, runID)
	must(t, err)
	// Move the admitted ledger point back one interval so the worker evaluates the current due point
	// while the prior run remains in its required release phase.
	_, err = store.Pool.Exec(`UPDATE scheduled_task_occurrences SET scheduled_at=scheduled_at - interval '1 hour' WHERE task_id=$1`, taskID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var admitted, skipped, runs int
	must(t, store.Pool.QueryRow(`SELECT count(*) FILTER (WHERE outcome='admitted'), count(*) FILTER (WHERE outcome='skipped_busy'), count(DISTINCT run_id) FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&admitted, &skipped, &runs))
	if admitted != 1 || skipped != 1 || runs != 1 {
		t.Fatalf("releasing run must leave one admitted run and one skipped occurrence, got admitted=%d skipped=%d runs=%d", admitted, skipped, runs)
	}
}

// TestScheduledTaskModelBindingFailureIsDurable verifies the final model-binding gate cannot roll
// back an admitted ledger row and later let the worker relabel it as missed.
func TestScheduledTaskModelBindingFailureIsDurable(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Invalid default", 65)
	// creatorModelReady still sees an enabled credential-backed connection; only the authoritative
	// binding policy detects that the configured default model is no longer offered.
	_, err := store.Pool.Exec(`UPDATE personal_model_connections
		SET models=jsonb_set(models, '{0,id}', '"vendor/model-v2"'::jsonb)
		WHERE user_id=$1`, s.userID)
	must(t, err)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var count, runs int
	var outcome, reason string
	must(t, store.Pool.QueryRow(`SELECT count(*), COALESCE(min(outcome),''), COALESCE(min(failure_reason),''), count(run_id) FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&count, &outcome, &reason, &runs))
	if count != 1 || outcome != "failed" || reason != "model_unavailable" || runs != 0 {
		t.Fatalf("binding rejection must remain one model-unavailable failure without a run, got count=%d outcome=%q reason=%q runs=%d", count, outcome, reason, runs)
	}
}

// TestScheduledTaskNonModelBindingFailureIsTargetUnavailable pins the D5 mapping for a target-side
// binding gap: a Claude agent that passes save-time eligibility (active agent, usable creator model)
// but has no declaration is recorded as failed/target_unavailable, not model_unavailable, so the
// creator is pointed at the agent's configuration rather than their own model connection.
func TestScheduledTaskNonModelBindingFailureIsTargetUnavailable(t *testing.T) {
	store, s, _ := scheduledTaskStore(t)
	claudeAgentID := seedScheduledTaskAgentWithPlugin(t, store.Pool, s, "official/ora-space.claude")
	taskID := seedEnabledScheduledTask(t, store, s, claudeAgentID, "Undeclared Claude", 65)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var count, runs int
	var outcome, reason string
	must(t, store.Pool.QueryRow(`SELECT count(*), COALESCE(min(outcome),''), COALESCE(min(failure_reason),''), count(run_id) FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&count, &outcome, &reason, &runs))
	if count != 1 || outcome != "failed" || reason != "target_unavailable" || runs != 0 {
		t.Fatalf("undeclared Claude agent must leave one failed target_unavailable occurrence without a run, got count=%d outcome=%q reason=%q runs=%d", count, outcome, reason, runs)
	}
}

// TestScheduledTaskEvaluationProcessesMoreThanOnePage prevents a fixed first 100-task page from
// starving later enabled definitions forever.
func TestScheduledTaskEvaluationProcessesMoreThanOnePage(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	const taskCount = 101 // worker page size is 100; retain an explicit overflow case here.
	for i := 0; i < taskCount; i++ {
		seedEnabledScheduledTask(t, store, s, agentID, "Paged "+uuid.NewString(), 65)
	}
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var count int
	must(t, store.Pool.QueryRow(`SELECT count(*) FROM scheduled_task_occurrences`).Scan(&count))
	if count != taskCount {
		t.Fatalf("evaluation must visit every enabled task, got %d", count)
	}
}

// TestScheduledTaskHistoryRetentionKeepsTheOldestActiveRun makes the active occurrence older than
// every disposable record. A later due point must still prune the oldest *terminal-or-unbound*
// entry, keep exactly 100 rows, and leave the active IssueRun linked as an audit fact.
func TestScheduledTaskHistoryRetentionKeepsTheOldestActiveRun(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "History retention", 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var activeRunID string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&activeRunID))
	_, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences
		SET scheduled_at=now() - interval '101 hours'
		WHERE task_id=$1 AND run_id=$2`, taskID, activeRunID)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO scheduled_task_occurrences(tenant_id,task_id,scheduled_at,outcome,task_revision,observed_at)
		SELECT $1,$2,now() - interval '100 hours' + n * interval '1 hour','missed',1,now()
		FROM generate_series(0,98) AS n`, s.tenantID, taskID)
	must(t, err)

	var oldestDisposable time.Time
	must(t, store.Pool.QueryRow(`SELECT min(scheduled_at) FROM scheduled_task_occurrences
		WHERE task_id=$1 AND run_id IS NULL`, taskID).Scan(&oldestDisposable))
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))

	var occurrences, activeLinks int
	must(t, store.Pool.QueryRow(`SELECT count(*), count(*) FILTER (WHERE run_id=$2)
		FROM scheduled_task_occurrences WHERE task_id=$1`, taskID, activeRunID).Scan(&occurrences, &activeLinks))
	if occurrences != 100 || activeLinks != 1 {
		t.Fatalf("retention must keep 100 occurrences including the active run, got occurrences=%d activeLinks=%d", occurrences, activeLinks)
	}
	var pruned bool
	must(t, store.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM scheduled_task_occurrences WHERE task_id=$1 AND scheduled_at=$2)`, taskID, oldestDisposable).Scan(&pruned))
	if pruned {
		t.Fatalf("retention must prune the oldest disposable occurrence %s", oldestDisposable)
	}
	var status string
	must(t, store.Pool.QueryRow(`SELECT status FROM issue_runs WHERE id=$1`, activeRunID).Scan(&status))
	if status == "completed" || status == "failed" || status == "cancelled" {
		t.Fatalf("retention must not delete or terminalize the active IssueRun, got status=%q", status)
	}
}

// TestScheduledTaskUpdateAppliesPatch (FIX-3): the PATCH writer's WHERE clause binds the right
// tenant, creator and version so a legal rename succeeds and bumps the version.
func TestScheduledTaskUpdateAppliesPatch(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Patch", 65)
	caller := scheduledTaskCaller(t, store, s.tenantID)

	out, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method:          "PATCH",
		Key:             "patch-1",
		Path:            "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID,
		TenantID:        s.tenantID,
		ScheduledTaskID: taskID,
		Body:            core.Object{"name": "Renamed", "version": int64(1)},
		Identity:        caller,
	})
	must(t, err)
	if status != 200 || out.O("resource").S("name") != "Renamed" {
		t.Fatalf("legal rename must succeed, got %d %v", status, out)
	}
	var version int
	must(t, store.Pool.QueryRow(`SELECT version FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&version))
	if version != 2 {
		t.Fatalf("rename must bump version, got %d", version)
	}
}

// TestScheduledTaskWritesRequireAcceptance keeps the rollout gate at the actual public boundary:
// before end-to-end acceptance, no caller can save a new definition or enable an existing one even
// if it bypasses the Web route. The worker carries the matching gate separately.
func TestScheduledTaskWritesRequireAcceptance(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"
	listed, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "GET", Path: path, TenantID: s.tenantID, Identity: caller,
	})
	if err != nil || status != 200 || !listed.B("acceptanceEnabled") {
		t.Fatalf("enabled list = %#v, status %d, err %v; want acceptanceEnabled=true", listed, status, err)
	}

	store.ScheduledTaskAcceptanceEnabled = false
	listed, status, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "GET", Path: path, TenantID: s.tenantID, Identity: caller,
	})
	if err != nil || status != 200 || listed.B("acceptanceEnabled") {
		t.Fatalf("gated list = %#v, status %d, err %v; want acceptanceEnabled=false", listed, status, err)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method:   "POST",
		Key:      "scheduled-gated-create",
		Path:     path,
		TenantID: s.tenantID,
		Identity: caller,
		Body: core.Object{
			"issueId":  s.issueID,
			"agentId":  agentID,
			"name":     "Gated create",
			"prompt":   "Sync",
			"schedule": core.Object{"kind": "daily", "hour": 9, "minute": 0},
			"timezone": "UTC",
			"approval": core.Object{"policy": "full_auto_in_cloud_workspace"},
			"enabled":  false,
		},
	})
	if fault := core.ErrorCode(err); fault.Code != "scheduled_task_acceptance_pending" || fault.Status != 409 {
		t.Fatalf("create must be gated before acceptance, got %v", err)
	}

	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Gated enable", 65)
	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method:          "POST",
		Key:             "scheduled-gated-enable",
		Path:            path + "/" + taskID + "/enable",
		TenantID:        s.tenantID,
		ScheduledTaskID: taskID,
		Identity:        caller,
		Body:            core.Object{},
	})
	if fault := core.ErrorCode(err); fault.Code != "scheduled_task_acceptance_pending" || fault.Status != 409 {
		t.Fatalf("enable must be gated before acceptance, got %v", err)
	}
}

// scheduledTaskCaller binds the seed's creator as a verified identity for the public PATCH path.
func scheduledTaskCaller(t *testing.T, store *core.Store, tenantID string) *core.Claims {
	t.Helper()
	var userID string
	must(t, store.Pool.QueryRow(`SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 AND role='admin' AND status='active' ORDER BY user_id LIMIT 1`, tenantID).Scan(&userID))
	subject := "scheduled-" + userID[:8]
	_, err := store.Pool.Exec(`INSERT INTO user_identities(user_id,source,subject) VALUES($1,'test-scheduled',$2)`, userID, subject)
	must(t, err)
	return &core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subject}, Source: "test-scheduled", Caller: "gateway", DisplayName: "Scheduled Caller"}
}
