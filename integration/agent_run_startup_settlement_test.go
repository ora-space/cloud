package integration

import (
	"context"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// Real gRPC takeover still receipts late facts after business abandonment, without a new delivery.
func TestStartupAbandonmentReceiptsLateFactsWithoutResurrectingRun(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.endThread(scene.threadScene, "end-before-first-record", 202, "")
	_, err := f.store.Pool.Exec("UPDATE issue_runs SET startup_ending_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1", scene.runID)
	must(t, err)
	must(t, f.store.StartQueuedAgentSessionsOnce(context.Background()))
	phase, status, state := f.threadRunState(scene.runID)
	if phase != "done" || status != "failed" || state != "ended" {
		t.Fatalf("confirmation did not bound waiting: %s/%s/%s", phase, status, state)
	}
	f.postThread(scene.threadScene, "late-input", threadBody("must not dispatch"), 409, "thread_closed")
	f.runningThread(t, scene)
	f.sessionEndOK(t, scene, "late-terminal-fact", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED)
	f.sessionEndOK(t, scene, "late-terminal-fact", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED)
	phase, status, state = f.threadRunState(scene.runID)
	if phase != "done" || status != "failed" || state != "ended" {
		t.Fatalf("late fact resurrected run: %s/%s/%s", phase, status, state)
	}
	if f.scalar("SELECT count(*) FROM agent_run_residuals WHERE run_id=$1 AND session_ended_at IS NOT NULL AND cleared_at IS NULL", scene.runID) != 1 ||
		f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", scene.executionID) != 2 ||
		f.scalar("SELECT count(*) FROM execution_work WHERE run_id=$1 AND kind='deliver_revision'", scene.runID) != 0 ||
		f.scalar("SELECT count(*) FROM operations WHERE kind='delete_workspace'") != 0 {
		t.Fatal("late facts must update their original responsibility, never fabricate delivery or cleanup")
	}
}

// Ending before any registration keeps HTTP request idempotency while the production loop
// settles the run, and ownership checks remain identical to the existing Thread endpoints.
func TestStartupEndPreservesHTTPIdempotencyAndClosesFurtherInput(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	seed := seedOwnershipChain(t, f.store.Pool)
	_, err := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status)
		VALUES($1,$2,'admin','active')`, seed.tenantID, f.uid)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issues SET project_ref=$2 WHERE id=$1`, seed.issueID, seed.projectID)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
		VALUES(gen_random_uuid(),$1,$2,1,'user',$3,'run.enqueued',jsonb_build_object('runId',$4::text))`,
		seed.tenantID, seed.issueID, seed.userID, seed.runID)
	must(t, err)
	created, err := f.store.CreateRunWorkspace(context.Background(), seed.runID)
	must(t, err)
	workspace := created.O("workspace").S("id")
	_, err = f.store.Pool.Exec(`UPDATE operations SET state='succeeded',step='done' WHERE id=$1`, created.O("operation").S("id"))
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_id=$2,phase='starting',status='dispatched',thread_state='pending'
		WHERE id=$1`, seed.runID, workspace)
	must(t, err)
	scene := threadScene{seed: seed, tenantID: seed.tenantID, issueID: seed.issueID, runID: seed.runID}
	f.seedThreadEntry(scene.runID, "system", "user_turn", `{"content":"frozen prompt"}`, nil, nil)
	_, err = f.store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status)
		VALUES($1,$2,$3,'official/startup-test','Startup','active')`, scene.seed.agentID, scene.seed.spaceID, scene.tenantID)
	must(t, err)
	f.endThread(scene, "startup-end", 202, "")
	must(t, f.store.StartQueuedAgentSessionsOnce(context.Background()))
	phase, status, thread := f.threadRunState(scene.runID)
	if phase != "releasing" || status != "cancelled" || thread != "ended" {
		t.Fatalf("startup end did not settle through HTTP and reconciliation: %s/%s/%s", phase, status, thread)
	}
	f.endThread(scene, "startup-end", 202, "")
	f.endThread(scene, "different-end", 409, "thread_closed")
	f.postThread(scene, "after-end", threadBody("must remain unsent"), 409, "thread_closed")
	if got := len(threadItems(t, f.getThread(scene, "", 200))); got != 1 {
		t.Fatalf("startup ending altered its frozen prompt: %d entries", got)
	}
}

// A recovery classification is visible before terminal takeover without inventing Node history.
func TestThreadReadShowsUnconfirmedStartupWhileWaitingForNode(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	_, err := f.store.Pool.Exec(`UPDATE issue_runs SET thread_state='ending',failure_reason='agent_start_failed',
		result='{"startup":{"state":"awaiting_node","policyCancellation":true}}' WHERE id=$1`, scene.runID)
	must(t, err)
	view := f.getThread(scene, "", 200)
	if view.S("threadState") != "ending" || view.S("failureCode") != "agent_start_failed" || len(threadItems(t, view)) != 0 {
		t.Fatalf("unconfirmed startup is not a safe visible recovery state: %v", view)
	}
}

// A late first record may prove the run executed, but must not reopen an accepted end request.
// The real terminal takeover and delivery still own the outcome and the cleanup declaration.
func TestStartupTimeoutReconcilesLateNodeEvidenceWithoutReopening(t *testing.T) {
	for _, tc := range []struct {
		name, status, failure string
		userEnd, lateRecord   bool
		withDelivery          bool
		reason                controlpb.AgentSessionEndReason
	}{
		{"policy cancellation without a record", "failed", "agent_start_failed", false, false, false, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED},
		{"late record and delivery retain policy failure", "failed", "agent_start_failed", false, true, true, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED},
		{"user ending before expiry retains completion", "completed", "", true, true, false, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			scene := seedLiveThreadScene(t, f)
			if !tc.withDelivery {
				f.store.ObjectStore = nil
			}
			scene.start(t, f)
			turn := f.postThread(scene.threadScene, "queued-startup-turn", threadBody("queued before expiry"), 201, "").O("resource").S("turnId")
			if tc.userEnd {
				f.endThread(scene.threadScene, "explicit-startup-end", 202, "")
			}
			f.store.AgentStartTimeout = time.Minute
			_, err := f.store.Pool.Exec(`UPDATE issue_runs SET startup_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, scene.runID)
			must(t, err)
			must(t, f.store.StartQueuedAgentSessionsOnce(context.Background()))
			must(t, f.store.StartQueuedAgentSessionsOnce(context.Background()))
			if view := f.getThread(scene.threadScene, "", 200); view.S("threadState") != "ending" || view.S("failureCode") != "agent_start_failed" {
				t.Fatalf("unknown execution must retain a visible ending state: %v", view)
			}
			if f.scalar(`SELECT count(*) FROM operations WHERE kind='delete_workspace'`) != 0 || f.turnStatus(scene.runID, turn) != "queued" {
				t.Fatal("unconfirmed execution discarded input or declared cleanup")
			}
			f.postThread(scene.threadScene, "after-expiry", threadBody("must remain unsent"), 409, "thread_closed")
			sequence := uint64(1)
			if tc.lateRecord {
				f.runningThread(t, scene)
				sequence++
				if _, _, state := f.threadRunState(scene.runID); state != "ending" {
					t.Fatalf("late first record reopened an ending Thread: %s", state)
				}
				f.postThread(scene.threadScene, "after-late-record", threadBody("must remain unsent"), 409, "thread_closed")
			}
			f.sessionEndOK(t, scene, "late-startup-terminal", sequence, tc.reason)
			if tc.withDelivery {
				if phase, _, state := f.threadRunState(scene.runID); phase != "delivering" || state != "ended" || f.scalar(`SELECT count(*) FROM operations WHERE kind='delete_workspace'`) != 0 {
					t.Fatalf("terminal takeover skipped its delivery responsibility: %s/%s", phase, state)
				}
				execution := f.deliverRevision(t, scene)
				_, err = f.deliveryTerminal(scene, execution, "startup-delivery-failed", 1, revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED), "delivery-failed")
				must(t, err)
				if phase, _, _ := f.threadRunState(scene.runID); phase != "delivering" {
					t.Fatal("failed delivery skipped the existing retry policy")
				}
				f.store.DeliveryGiveUpAfter = time.Nanosecond
				must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
			}
			phase, status, state := f.threadRunState(scene.runID)
			var failure, startupState string
			must(t, f.store.Pool.QueryRow(`SELECT failure_reason,result->'startup'->>'state' FROM issue_runs WHERE id=$1`, scene.runID).Scan(&failure, &startupState))
			if phase != "releasing" || status != tc.status || state != "ended" || failure != tc.failure || startupState != "settled" {
				t.Fatalf("late terminal lost startup/user outcome: %s/%s/%s failure=%s startup=%s", phase, status, state, failure, startupState)
			}
			if f.turnStatus(scene.runID, turn) != "discarded" || f.scalar(`SELECT count(*) FROM operations WHERE kind='delete_workspace'`) != 1 {
				t.Fatal("terminal settlement did not own discarded input and exactly one cleanup")
			}
			before := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID)
			f.sessionEndOK(t, scene, "late-startup-terminal", sequence, tc.reason)
			if f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID) != before {
				t.Fatal("replayed late terminal settled the run twice")
			}
		})
	}
}
