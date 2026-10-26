package integration

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func workflowModelReadPath(tenant, workflow, run, suffix string) string {
	return "/api/v1/tenants/" + tenant + "/workflows/" + workflow + "/runs/" + run + "/" + suffix
}

// seedWorkflowAttempts writes a content-free attempt ledger for a live WorkflowRun. The rows are
// inserted directly because this test is about the read path; registration is driven through the
// gateway and grants by model_attempt_workflow_db_test.go and model_attempt_gateway_test.go. Real
// PostgreSQL constraints still apply, so the supporting Workspace, node execution and grant are
// the same shapes those paths persist.
func seedWorkflowAttempts(t *testing.T, f *fixture, runID string, attempts []core.Object) []string {
	t.Helper()
	var tenant, project string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id, project_id FROM workflow_runs WHERE id=$1`, runID).Scan(&tenant, &project))
	var binding string
	err := f.store.Pool.QueryRow(`SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 ORDER BY slot_name LIMIT 1`, runID).Scan(&binding)
	if err == sql.ErrNoRows {
		// The development simulator settles the run without freezing executor bindings, so a test
		// run created through the fixture has none. Freeze the same row the production gate would
		// from the personal connection seedPersonalModel installed.
		binding = uuid.NewString()
		var inserted int64
		result, execErr := f.store.Pool.Exec(`INSERT INTO run_model_bindings(id,workflow_run_id,slot_name,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
			SELECT $1,$2,'default',c.user_id,c.id,c.version,c.credential_id,c.name,c.protocol,c.base_url,c.auth_mode,c.models->0
			FROM personal_model_connections c
			WHERE c.user_id=$3 AND c.enabled AND c.deleted_at IS NULL
			ORDER BY c.created_at, c.id LIMIT 1`, binding, runID, f.uid)
		must(t, execErr)
		inserted, execErr = result.RowsAffected()
		must(t, execErr)
		if inserted != 1 {
			t.Fatalf("no personal model connection to freeze for run %s", runID)
		}
	} else if err != nil {
		t.Fatalf("read frozen binding: %v", err)
	}
	workspace, grant := uuid.NewString(), uuid.NewString()
	execution := "exec-" + runID[:8]
	// A live isolated Workspace must own exactly one task identity, and the guard is checked at
	// commit, so the pair is persisted in one transaction.
	tx, err := f.store.Pool.Begin()
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref)
		VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','running','ready',1,'HEAD')`,
		workspace, tenant, f.uid, project)
	must(t, err)
	_, err = tx.Exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'statistics attempt ledger')`, uuid.NewString(), workspace)
	must(t, err)
	must(t, tx.Commit())
	_, err = f.store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
		VALUES($1,'agent_session',$2,$3,'node-statistics','operation-statistics','{}'::jsonb,1)`,
		execution, uuid.NewString(), workspace)
	must(t, err)
	digest := sha256.Sum256([]byte(grant))
	_, err = f.store.Pool.Exec(`INSERT INTO model_access_grants(id,binding_id,execution_id,workspace_id,runtime_generation,token_digest)
		VALUES($1,$2,$3,$4,1,$5)`, grant, binding, execution, workspace, hex.EncodeToString(digest[:]))
	must(t, err)
	ids := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		id, outcome, started := uuid.NewString(), attempt.S("outcome"), attempt.S("startedAt")
		var finished, observation any
		var duration, firstOutput, input, output, total any
		availability, terminal := "unavailable", false
		if outcome != "started" {
			finished, observation = started, `{"synthetic":true}`
			duration, firstOutput = int64(100), int64(20)
			terminal = outcome == "succeeded"
			if attempt.O("usage") != nil {
				availability = attempt.O("usage").S("availability")
				input = int64(4)
				if availability == "complete" {
					output, total = int64(5), int64(15)
					input = int64(10)
				}
			}
		}
		_, err := f.store.Pool.Exec(`INSERT INTO model_request_attempts(
			id,tenant_id,workflow_run_id,binding_id,grant_id,execution_id,model_id,protocol,streamed,
			started_at,finished_at,outcome,error_code,http_status,duration_ms,first_output_ms,terminal_observed,
			input_tokens,output_tokens,total_tokens,input_semantics,usage_availability,observation)
			VALUES($1,$2,$3,$4,$5,$6,'vendor/model-v1','openai-completions',true,
			$7::timestamptz,$8::timestamptz,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb)`,
			id, tenant, runID, binding, grant, execution, started, finished, outcome, "synthetic_error", 200,
			duration, firstOutput, terminal, input, output, total, "includes_cache", availability, observation)
		must(t, err)
		ids = append(ids, id)
	}
	return ids
}

// TestWorkflowModelStatisticsHTTP exercises the WorkflowRun read scope over real HTTP and
// PostgreSQL: empty, partial and complete ledgers; row isolation between runs; membership-only
// visibility; cursor pagination bound to one run; and workflow/run deletion.
func TestWorkflowModelStatisticsHTTP(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	pid := runProject(t, f, "statistics-project")
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Statistics source", "graph": runGraph(),
	}, "statistics-wf", 200).O("resource")
	wfid := wf.S("id")
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "statistics-publish", 200)
	run := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": pid}, "statistics-run", 200).O("resource")
	rid := run.S("id")
	statistics := workflowModelReadPath(f.tid, wfid, rid, "statistics")
	calls := workflowModelReadPath(f.tid, wfid, rid, "model-calls")

	// No recorded attempt is "not measured", not zero usage.
	empty := f.call("GET", statistics, nil, "", 200)
	if empty.O("counts").N("total") != 0 || empty["firstRecordedAt"] != nil ||
		empty.O("usage").O("inputTokens")["value"] != nil || empty.O("usage").O("inputTokens").S("availability") != "unavailable" {
		t.Fatalf("empty WorkflowRun ledger reported measurements: %v", empty)
	}
	if got := threadItems(t, f.call("GET", calls, nil, "", 200)); len(got) != 0 {
		t.Fatalf("empty WorkflowRun calls=%v", got)
	}

	// The creator's frozen binding is the owner: the ledger rows are registered under
	// workflow_run_id and must be visible through the WorkflowRun read.
	ids := seedWorkflowAttempts(t, f, rid, []core.Object{
		{"outcome": "started", "startedAt": "2026-10-01T10:00:00Z"},
		{"outcome": "succeeded", "startedAt": "2026-10-01T10:00:01Z", "usage": core.Object{"availability": "complete"}},
		{"outcome": "failed", "startedAt": "2026-10-01T10:00:02Z", "usage": core.Object{"availability": "partial"}},
	})

	out := f.call("GET", statistics, nil, "", 200)
	counts := out.O("counts")
	if counts.N("total") != 3 || counts.N("started") != 1 || counts.N("succeeded") != 1 || counts.N("failed") != 1 {
		t.Fatalf("counts=%v", counts)
	}
	if out["firstRecordedAt"] == nil || out["lastRecordedAt"] == nil {
		t.Fatalf("recorded range missing: %v", out)
	}
	input := out.O("usage").O("inputTokens")
	if input.N("value") != 14 || input.N("knownCount") != 2 || input.N("totalCount") != 3 || input.S("availability") != "partial" {
		t.Fatalf("input coverage=%v", input)
	}
	output := out.O("usage").O("outputTokens")
	if output.N("value") != 5 || output.N("knownCount") != 1 || output.N("totalCount") != 3 {
		t.Fatalf("output coverage=%v", output)
	}
	timing := out.O("timing").O("meanDurationMs")
	if timing.N("value") != 100 || timing.N("knownCount") != 2 || timing.N("totalCount") != 3 || timing.S("availability") != "partial" {
		t.Fatalf("timing coverage=%v", timing)
	}

	page := f.call("GET", calls+"?limit=2", nil, "", 200)
	first := threadItems(t, page)
	if len(first) != 2 || first[0].S("id") != ids[0] || first[1].S("id") != ids[1] || page.S("nextCursor") != ids[1] {
		t.Fatalf("first page=%v", page)
	}
	if first[0].O("usage").S("availability") != "unavailable" || first[1].O("usage").S("availability") != "complete" {
		t.Fatalf("attempt usage evidence=%v", first)
	}
	rest := threadItems(t, f.call("GET", calls+"?after="+page.S("nextCursor"), nil, "", 200))
	if len(rest) != 1 || rest[0].S("id") != ids[2] || rest[0].S("outcome") != "failed" || rest[0].O("usage").S("availability") != "partial" {
		t.Fatalf("second page=%v", rest)
	}
	for _, private := range []string{"bindingId", "grantId", "executionId", "tenantId", "workflowRunId", "runId"} {
		if _, ok := first[0][private]; ok {
			t.Fatalf("private field exposed: %s", private)
		}
	}

	// A cursor from another WorkflowRun is not an anchor in this one.
	other := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": pid}, "statistics-run-2", 200).O("resource")
	otherIDs := seedWorkflowAttempts(t, f, other.S("id"), []core.Object{
		{"outcome": "succeeded", "startedAt": "2026-10-01T10:00:03Z", "usage": core.Object{"availability": "complete"}},
	})
	otherStatistics := workflowModelReadPath(f.tid, wfid, other.S("id"), "statistics")
	if got := f.call("GET", otherStatistics, nil, "", 200); got.O("counts").N("total") != 1 {
		t.Fatalf("second run saw another run's attempts: %v", got)
	}
	if got := f.call("GET", statistics, nil, "", 200); got.O("counts").N("total") != 3 {
		t.Fatalf("first run saw another run's attempts: %v", got)
	}
	sessionDashboardFault(t, f, f.user, calls+"?after="+otherIDs[0], 400, "invalid_pagination")

	// Any active tenant member who can read the run can read its statistics; the creator is not
	// privileged, and deleting the membership removes access.
	member, _ := f.addUser(t, "statistics-member", "Statistics member")
	if status, got, err := f.threadRequestAs(member, "GET", statistics, "", ""); err != nil || status != 200 || got.O("counts").N("total") != 3 {
		t.Fatalf("member statistics: status=%d body=%v err=%v", status, got, err)
	}
	outsider, outsiderID := f.addUser(t, "statistics-outsider", "Statistics outsider")
	_, err := f.store.Pool.Exec(`DELETE FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2`, f.tid, outsiderID)
	must(t, err)
	sessionDashboardFault(t, f, outsider, statistics, 403, "membership_required")
	unknown := f.user
	unknown.Subject = "statistics-unregistered"
	sessionDashboardFault(t, f, unknown, statistics, 403, "membership_required")

	// Tenant, workflow, run and soft-deletion scopes are 404s after membership succeeds.
	sessionDashboardFault(t, f, f.user, strings.Replace(statistics, f.tid, f.moreTenant(), 1), 404, "not_found")
	sessionDashboardFault(t, f, f.user, workflowModelReadPath(f.tid, uuid.NewString(), rid, "statistics"), 404, "not_found")
	sessionDashboardFault(t, f, f.user, workflowModelReadPath(f.tid, wfid, uuid.NewString(), "model-calls"), 404, "not_found")
	otherWorkflow := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Statistics other source", "graph": runGraph(),
	}, "statistics-wf-2", 200).O("resource").S("id")
	sessionDashboardFault(t, f, f.user, workflowModelReadPath(f.tid, otherWorkflow, other.S("id"), "statistics"), 404, "not_found")
	_, err = f.store.Pool.Exec(`UPDATE workflow_runs SET deleted_at=now() WHERE id=$1`, other.S("id"))
	must(t, err)
	sessionDashboardFault(t, f, f.user, otherStatistics, 404, "not_found")

	// Query handling matches the IssueRun routes: statistics accepts none, model-calls only a
	// singleton limit/after.
	for _, query := range []string{"limit=1", "after=" + ids[0], "unknown=value"} {
		sessionDashboardFault(t, f, f.user, statistics+"?"+query, 400, "invalid_session_filter")
	}
	for _, query := range []string{"before=" + ids[0], "unknown=value", "limit="} {
		sessionDashboardFault(t, f, f.user, calls+"?"+query, 400, "invalid_session_filter")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "after=not-a-uuid"} {
		sessionDashboardFault(t, f, f.user, calls+"?"+query, 400, "invalid_pagination")
	}

	// Archiving the workflow hides its whole run history, including statistics.
	version := f.call("GET", f.path("/workflows/"+wfid), nil, "", 200).N("version")
	f.call("DELETE", f.path("/workflows/"+wfid), core.Object{"version": version}, "statistics-delete", 200)
	sessionDashboardFault(t, f, f.user, statistics, 404, "not_found")
	sessionDashboardFault(t, f, f.user, calls, 404, "not_found")
}
