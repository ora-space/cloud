package core

import (
	"context"
	"testing"
	"time"
)

// A stale work row must neither block another tenant nor authorize a new execution. Terminal
// lifecycle state is the fixture precondition; declaration, claim and dispatch use production code.
func TestRetiredRunWorkCannotBlockOrRegister(t *testing.T) {
	for _, phase := range []string{"releasing", "done"} {
		t.Run(phase, func(t *testing.T) {
			store := dispatcherDB(t)
			old, claims := seedDispatchableWork(t, store)
			healthy := seedStartingRun(t, store, sessionStartInput())
			bindConnectedNode(t, store, healthy.ws)
			runSessionStartPass(t, store)
			if old.seed.seed.tenant == healthy.seed.tenant {
				t.Fatal("the successor must belong to another tenant")
			}
			if _, err := store.Pool.Exec(`UPDATE issue_runs SET phase=$2,status='failed',
				thread_state='ended',completed_at=now(),version=version+1 WHERE id=$1`, old.run, phase); err != nil {
				t.Fatalf("stage the terminal business state: %v", err)
			}
			var healthyWork Object
			for attempt := range 3 {
				picked, err := store.Control(context.Background(), &ControlRequest{
					Action: "clone_claim", Body: Object{"epoch": 1}, Service: claims,
				})
				if err != nil {
					t.Fatalf("claim %d: %v", attempt+1, err)
				}
				healthyWork = picked.O("work")
				if healthyWork.S("runId") != healthy.seed.run {
					t.Errorf("claim %d: terminal Run work still occupies the head; another tenant's live work was not offered", attempt+1)
				}
			}
			if _, err := dispatchAgentWork(t, store, claims, old.work, execOf(old.work, "late"), "", nil); err == nil || ErrorCode(err).Code != "dispatch_conflict" {
				t.Errorf("a pre-settlement claim must not register new work after %s: %v", phase, err)
			}
			if healthyWork.S("runId") == healthy.seed.run {
				if _, err := dispatchAgentWork(t, store, claims, healthyWork, execOf(healthyWork, "healthy"), "", nil); err != nil {
					t.Fatalf("the other tenant must still register its valid work: %v", err)
				}
			}
		})
	}
}

// The eligibility fence applies to IssueRun-owned work only. Workflow work carries workflow_run_id and
// leaves run_id NULL, so a retired IssueRun at the head of the queue must neither hide the workflow
// item from ClaimWork nor make its dispatch a conflict.
func TestWorkflowWorkStaysClaimableBesideRetiredIssueRunWork(t *testing.T) {
	store := dispatcherDB(t)

	// An IssueRun whose unregistered work is older than the workflow item, then closes its business
	// lifecycle: the claim must skip that row without skipping the workflow one behind it.
	retired := seedStartingRun(t, store, sessionStartInput())
	bindConnectedNode(t, store, retired.ws)
	runSessionStartPass(t, store)
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET phase='releasing',status='failed',
		thread_state='ended',completed_at=now(),version=version+1 WHERE id=$1`, retired.seed.run); err != nil {
		t.Fatalf("stage the terminal business state: %v", err)
	}

	// The workflow run's own Workspace and connected Node, as the execution wiring drives them.
	runID, _, _ := seedWorkflowIdentityScene(t, store, true)
	var wsID string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE workflow_runs SET status='running' WHERE id=$1", runID)
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("the project must be idle")
		}
		wsID = created.O("workspace").S("id")
		settleCreateWorkspaceOperation(t, tx, created.O("operation").S("id"))
		runWorkspaceSettled(tx, runID, "ready")
		tx.exec(`UPDATE workspaces SET observed_state='ready', admission_open=true, version=version+1 WHERE id=$1`, wsID)
		return Object{}
	}); err != nil {
		t.Fatalf("stage ready workspace: %v", err)
	}
	sandbox, nodeService := bindConnectedNode(t, store, wsID)
	input := Object{"kind": "agent_session"}
	target := Object{"workspaceId": wsID, "sandboxInstanceId": sandbox, "nodeId": nodeService}
	work, err := store.EnqueueExecutionWork(context.Background(), runID, "agent_session", input, target, time.Time{})
	if err != nil {
		t.Fatalf("enqueue workflow work: %v", err)
	}
	if work.S("workflowRunId") != runID || work.S("runId") != "" {
		t.Fatalf("work %v must register against the workflow run alone", work)
	}

	seedControllerLease(t, store)
	claims := controllerClaims()
	pick, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_claim", Body: Object{"epoch": 1}, Service: claims,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed := pick.O("work")
	if claimed.S("id") != work.S("id") {
		t.Fatalf("claim returned work %v; want the declared workflow item — the retired IssueRun's work must be skipped, not the workflow item", claimed)
	}
	if claimed.S("claimedRunId") != runID {
		t.Fatalf("claim projected run %q; want the workflow run %q", claimed.S("claimedRunId"), runID)
	}

	execution := "exec-wf-" + work.S("id")[:8]
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_dispatch", SubmissionID: "workflow-claim-dispatch", Service: claims,
		Body: Object{"epoch": int64(1), "operationId": claimed.S("claimedRunId"), "executionId": execution, "nodeId": nodeService, "input": input},
	}); err != nil {
		t.Fatalf("dispatch the claimed workflow work: %v", err)
	}
	var operation, workID string
	if err := store.Pool.QueryRow(`SELECT coalesce(operation_id::text,''),coalesce(work_id::text,'')
		FROM node_executions WHERE execution_id=$1`, execution).Scan(&operation, &workID); err != nil {
		t.Fatalf("read node execution: %v", err)
	}
	if operation != runID || workID != work.S("id") {
		t.Fatalf("node execution operation=%q work=%q; want the workflow run and the claimed item", operation, workID)
	}
}
