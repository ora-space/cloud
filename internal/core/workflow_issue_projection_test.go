package core

import (
	"context"
	"reflect"
	"testing"
)

type workflowProjectionScene struct {
	tenant, issue, workflow, workflowRun, issueRun string
}

// The scene stages a linked run only; scheduler invocation is deliberately outside this test's
// boundary. Every projection below consumes the real WorkflowRun row in its caller's transaction.
func seedWorkflowProjection(t *testing.T, store *Store) workflowProjectionScene {
	t.Helper()
	s := workflowProjectionScene{newID(), newID(), newID(), newID(), newID()}
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		user, snapshot := newID(), newID()
		tx.exec("INSERT INTO users(id,display_name,status) VALUES($1,'projection','active')", user)
		tx.exec("INSERT INTO tenants(id,name,status) VALUES($1,'projection','active')", s.tenant)
		tx.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", s.tenant, user)
		tx.exec("INSERT INTO issues(id,tenant_id,creator_user_id,title,number) VALUES($1,$2,$3,'projection',1)", s.issue, s.tenant, user)
		tx.exec("INSERT INTO workflows(id,tenant_id,name) VALUES($1,$2,'projection')", s.workflow, s.tenant)
		tx.exec("INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'projection','{}')", snapshot, s.tenant, s.workflow)
		tx.exec("INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name) VALUES($1,$2,$3,$4,'projection')", s.workflowRun, s.tenant, s.workflow, snapshot)
		tx.exec("INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,workflow_invocation_ref) VALUES($1,$2,$3,'workflow',$4,$5)", s.issueRun, s.tenant, s.issue, s.workflow, s.workflowRun)
		appendActivity(tx, s.tenant, s.issue, "user", user, "run.enqueued", runActivityDetails(s.issueRun, "workflow", s.workflow, nil))
		return Object{}
	})
	if err != nil {
		t.Fatalf("seed linked workflow: %v", err)
	}
	return s
}

func projectWorkflowState(t *testing.T, store *Store, s workflowProjectionScene, state, summary string) Object {
	t.Helper()
	out, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE workflow_runs SET status=$2,
			started_at=CASE WHEN $2='pending' THEN NULL ELSE '2026-10-15 10:00:00+00'::timestamptz END,
			finished_at=CASE WHEN $2 IN ('succeeded','failed','cancelled') THEN '2026-10-15 10:01:00+00'::timestamptz ELSE NULL END,
			error=CASE WHEN $2 IN ('failed','cancelled') THEN 'execution reason' ELSE '' END
			WHERE id=$1 AND tenant_id=$3`, s.workflowRun, state, s.tenant)
		return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, summary)
	})
	if err != nil {
		t.Fatalf("project %s: %v", state, err)
	}
	return out
}

func workflowProjectionActivities(t *testing.T, store *Store, s workflowProjectionScene) []Object {
	t.Helper()
	out, err := store.transact(context.Background(), func(tx *transaction) Object {
		return Object{"items": tx.list("SELECT * FROM issue_activities WHERE tenant_id=$1 AND issue_id=$2 ORDER BY seq", s.tenant, s.issue)}
	})
	if err != nil {
		t.Fatalf("activities: %v", err)
	}
	return out["items"].([]Object)
}

func TestWorkflowIssueProjectionStatesAndActivities(t *testing.T) {
	store := dispatcherDB(t)
	for _, tc := range []struct{ state, status, action string }{
		{"succeeded", "completed", "run.completed"},
		{"failed", "failed", "run.failed"},
		{"cancelled", "cancelled", "run.cancelled"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			s := seedWorkflowProjection(t, store)
			pending := projectWorkflowState(t, store, s, "pending", "")
			if pending.S("status") != "queued" || pending.N("version") != 1 {
				t.Fatalf("pending must stay queued without a write: %v", pending)
			}
			running := projectWorkflowState(t, store, s, "running", "")
			if running.S("status") != "running" || running.S("startedAt") == "" || running.S("completedAt") != "" {
				t.Fatalf("running projection: %v", running)
			}
			repeatedRunning, err := store.transact(context.Background(), func(tx *transaction) Object {
				return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "")
			})
			if err != nil || !reflect.DeepEqual(running, repeatedRunning) || len(workflowProjectionActivities(t, store, s)) != 2 {
				t.Fatalf("running repeat changed row or activities: err=%v before=%v after=%v", err, running, repeatedRunning)
			}
			out := projectWorkflowState(t, store, s, tc.state, "  execution output  ")
			if out.S("status") != tc.status {
				t.Fatalf("status = %s, want %s", out.S("status"), tc.status)
			}
			var timestampsMatch bool
			if err := store.Pool.QueryRow(`SELECT i.started_at=w.started_at AND i.completed_at=w.finished_at
				FROM issue_runs i JOIN workflow_runs w ON w.id=i.workflow_invocation_ref WHERE i.id=$1`, s.issueRun).Scan(&timestampsMatch); err != nil || !timestampsMatch {
				t.Fatalf("timestamps must mirror source: match=%v err=%v", timestampsMatch, err)
			}
			wantReason := "execution reason"
			if tc.state == "succeeded" {
				wantReason = ""
			}
			if out.S("failureReason") != wantReason || out.S("error") != wantReason {
				t.Fatalf("terminal reason: %v", out)
			}
			activities := workflowProjectionActivities(t, store, s)
			var actions []string
			for index, a := range activities {
				actions = append(actions, a.S("action"))
				if index == 0 {
					continue
				}
				if a.S("actorType") != "system" || a["actorId"] != nil || a.O("details").S("workflowInvocationRef") != s.workflowRun || a.O("details").S("runId") != s.issueRun {
					t.Fatalf("system activity provenance: %v", a)
				}
			}
			if !reflect.DeepEqual(actions, []string{"run.enqueued", "run.started", "run.message", tc.action}) || activities[2].O("details").S("message") != "execution output" {
				t.Fatalf("activity sequence or summary: %v", activities)
			}
			if tc.state == "failed" && activities[3].O("details").S("failureReason") != wantReason {
				t.Fatalf("failed activity reason: %v", activities[3])
			}
			var comments int
			if err := store.Pool.QueryRow("SELECT count(*) FROM issue_comments WHERE issue_id=$1", s.issue).Scan(&comments); err != nil || comments != 0 {
				t.Fatalf("workflow output must not author any comment: count=%d err=%v", comments, err)
			}
			repeated, err := store.transact(context.Background(), func(tx *transaction) Object {
				return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "a replay must not replace the first summary")
			})
			if err != nil || !reflect.DeepEqual(out, repeated) || !reflect.DeepEqual(activities, workflowProjectionActivities(t, store, s)) {
				t.Fatalf("repeat changed row or activities: err=%v before=%v after=%v", err, out, repeated)
			}
		})
	}
}

func TestWorkflowIssueProjectionDirectTerminalIncludesStart(t *testing.T) {
	store := dispatcherDB(t)
	for _, initial := range []string{"queued", "dispatched"} {
		for _, tc := range []struct{ state, action string }{
			{"succeeded", "run.completed"},
			{"failed", "run.failed"},
			{"cancelled", "run.cancelled"},
		} {
			t.Run(initial+"_to_"+tc.state, func(t *testing.T) {
				s := seedWorkflowProjection(t, store)
				if _, err := store.Pool.Exec("UPDATE issue_runs SET status=$2 WHERE id=$1", s.issueRun, initial); err != nil {
					t.Fatal(err)
				}
				out := projectWorkflowState(t, store, s, tc.state, "direct terminal output")
				activities := workflowProjectionActivities(t, store, s)
				var actions []string
				for _, activity := range activities {
					actions = append(actions, activity.S("action"))
				}
				if !reflect.DeepEqual(actions, []string{"run.enqueued", "run.started", "run.message", tc.action}) {
					t.Fatalf("direct terminal must retain lifecycle ordering: %v", actions)
				}
				if activities[1].S("actorType") != "system" || activities[1]["actorId"] != nil || activities[1].O("details").S("workflowInvocationRef") != s.workflowRun {
					t.Fatalf("supplemented start provenance: %v", activities[1])
				}
				repeated, err := store.transact(context.Background(), func(tx *transaction) Object {
					return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "direct terminal output")
				})
				if err != nil || !reflect.DeepEqual(out, repeated) || !reflect.DeepEqual(activities, workflowProjectionActivities(t, store, s)) {
					t.Fatalf("direct terminal repeat changed row or activities: err=%v", err)
				}
			})
		}
	}
}

func TestWorkflowIssueProjectionAwaitingInputPreservesIssueRunAndCommits(t *testing.T) {
	store := dispatcherDB(t)
	for _, initial := range []string{"pending", "running"} {
		t.Run(initial, func(t *testing.T) {
			s := seedWorkflowProjection(t, store)
			before := projectWorkflowState(t, store, s, initial, "")
			activities := workflowProjectionActivities(t, store, s)
			out, err := store.transact(context.Background(), func(tx *transaction) Object {
				tx.exec("UPDATE workflow_runs SET status='awaiting_input',version=version+1,input=$2 WHERE id=$1", s.workflowRun, jsonText(Object{"commitWitness": true}))
				return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "no output activity while waiting")
			})
			if err != nil || !reflect.DeepEqual(before, out) || !reflect.DeepEqual(activities, workflowProjectionActivities(t, store, s)) {
				t.Fatalf("awaiting_input must preserve IssueRun and activity history: err=%v before=%v after=%v", err, before, out)
			}
			var status string
			var committed bool
			if err := store.Pool.QueryRow("SELECT status,input->>'commitWitness'='true' FROM workflow_runs WHERE id=$1", s.workflowRun).Scan(&status, &committed); err != nil || status != "awaiting_input" || !committed {
				t.Fatalf("caller's workflow update did not commit: status=%s witness=%v err=%v", status, committed, err)
			}
		})
	}
}

func TestWorkflowIssueProjectionCancellationBeforeStart(t *testing.T) {
	store := dispatcherDB(t)
	s := seedWorkflowProjection(t, store)
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE workflow_runs SET status='cancelled',finished_at=now(),error='cancelled before start' WHERE id=$1", s.workflowRun)
		return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	activities := workflowProjectionActivities(t, store, s)
	if len(activities) != 2 || activities[1].S("action") != "run.cancelled" {
		t.Fatalf("cancellation must not fabricate a start or output: %v", activities)
	}
	var complete, unstarted bool
	if err := store.Pool.QueryRow("SELECT completed_at IS NOT NULL,started_at IS NULL FROM issue_runs WHERE id=$1", s.issueRun).Scan(&complete, &unstarted); err != nil || !complete || !unstarted {
		t.Fatalf("cancelled timestamps: complete=%v unstarted=%v err=%v", complete, unstarted, err)
	}
}

func TestWorkflowIssueProjectionActivityFailureRollsBack(t *testing.T) {
	store := dispatcherDB(t)
	s := seedWorkflowProjection(t, store)
	if _, err := store.Pool.Exec("ALTER TABLE issue_activities ADD CONSTRAINT reject_projection_failure CHECK(action <> 'run.failed')"); err != nil {
		t.Fatal(err)
	}
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE workflow_runs SET status='failed',finished_at=now(),error='failure' WHERE id=$1", s.workflowRun)
		return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "must roll back too")
	})
	if err == nil {
		t.Fatal("activity failure must abort caller transaction")
	}
	var workflowState, issueState string
	var version int
	if err := store.Pool.QueryRow(`SELECT w.status,i.status,i.version FROM workflow_runs w
		JOIN issue_runs i ON i.workflow_invocation_ref=w.id WHERE w.id=$1`, s.workflowRun).Scan(&workflowState, &issueState, &version); err != nil {
		t.Fatal(err)
	}
	if workflowState != "pending" || issueState != "queued" || version != 1 || len(workflowProjectionActivities(t, store, s)) != 1 {
		t.Fatalf("partial transition escaped rollback: workflow=%s issue=%s version=%d", workflowState, issueState, version)
	}
}

func TestWorkflowIssueProjectionScopeAndInvalidStates(t *testing.T) {
	store := dispatcherDB(t)
	s := seedWorkflowProjection(t, store)
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		return projectWorkflowRunToIssueRun(tx, newID(), s.workflowRun, "")
	})
	if ErrorCode(err).Code != "not_found" {
		t.Fatalf("foreign tenant: %v", err)
	}
	for _, state := range []string{"running", "failed"} {
		_, err := store.transact(context.Background(), func(tx *transaction) Object {
			tx.exec("UPDATE workflow_runs SET status=$2 WHERE id=$1", s.workflowRun, state)
			return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "")
		})
		if err == nil {
			t.Fatalf("state %s without required timestamps must fail", state)
		}
	}
	projectWorkflowState(t, store, s, "succeeded", "")
	_, err = store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE workflow_runs SET status='failed',error='different outcome' WHERE id=$1", s.workflowRun)
		return projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "")
	})
	if ErrorCode(err).Code != "workflow_run_projection_conflict" {
		t.Fatalf("terminal rewrite: %v", err)
	}
	// A direct WorkflowRun remains valid without an IssueRun and must not emit issue activities.
	_, err = store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("DELETE FROM issue_runs WHERE id=$1", s.issueRun)
		require(projectWorkflowRunToIssueRun(tx, s.tenant, s.workflowRun, "") == nil, 500, "internal_error")
		return Object{}
	})
	if err != nil {
		t.Fatalf("unlinked direct run: %v", err)
	}
}
