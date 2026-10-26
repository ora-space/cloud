package integration

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// lateDeliveryEnqueueLater creates the competing Run through the public production endpoint.
// A different active Agent is deliberate: the ordering guard belongs to the Issue, not an Agent.
func lateDeliveryEnqueueLater(t *testing.T, f *fixture, scene liveThreadScene) string {
	t.Helper()
	agent := uuid.NewString()
	f.seedSpaceAgent(t, scene.spaceID, scene.tenantID, agent, "official/late-order-"+agent[:8], "Later Agent", "active", "1.0.0")
	body := mustJSON(t, core.Object{"executorType": "agent", "executorId": agent})
	code, response, err := f.threadRequest("POST", "/api/v1/tenants/"+scene.tenantID+"/issues/"+scene.issueID+"/runs", body, "later-run-"+agent)
	must(t, err)
	if code != 200 {
		t.Fatalf("production creation of later Run: HTTP %d %v", code, response)
	}
	var run string
	must(t, f.store.Pool.QueryRow(`SELECT r.id::text FROM issue_runs r JOIN issue_activities a
 ON a.tenant_id=r.tenant_id AND a.issue_id=r.issue_id AND a.action='run.enqueued' AND a.details->>'runId'=r.id::text
 WHERE r.executor_id=$1 AND r.issue_id=$2`, agent, scene.issueID).Scan(&run))
	var later bool
	must(t, f.store.Pool.QueryRow(`SELECT a.seq>b.seq FROM issue_activities a JOIN issue_activities b ON a.issue_id=b.issue_id
 WHERE a.action='run.enqueued' AND a.details->>'runId'=$1 AND b.action='run.enqueued' AND b.details->>'runId'=$2`, run, scene.runID).Scan(&later))
	if !later {
		t.Fatal("later Run has no strictly later committed run.enqueued sequence")
	}
	return run
}

func lateDeliveryReceiptOnly(t *testing.T, f *fixture, req *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	lateDeliveryAccepted(t, f, req)
	if f.scalar("SELECT count(*) FROM revisions WHERE execution_id=$1", req.ExecutionId) != 0 ||
		f.scalar("SELECT count(*) FROM revision_verifications WHERE execution_id=$1", req.ExecutionId) != 0 {
		t.Fatal("receipt-only intake invented a Revision or verified delivery")
	}
}

func lateDeliveryResidualAuthority(t *testing.T, f *fixture, scene liveThreadScene) string {
	t.Helper()
	var snapshot string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'responsibility',responsibility,'state',reclaim_state,'id',reclaim_id,'mode',reclaim_mode,
 'operation',reclaim_operation_id,'authorization',reclaim_authorization,'targets',reclaim_targets,
 'cleared',cleared_at,'disposition',disposition,
 'intents',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.id),'[]') FROM runtime_force_stops s WHERE s.workspace_id=r.workspace_id))::text
 FROM agent_run_residuals r WHERE run_id=$1`, scene.runID).Scan(&snapshot))
	return snapshot
}

func TestLateDeliveryFactRunCreationOrderOverGRPC(t *testing.T) {
	// Every path has both sides of the predicate. Root paths contain no ledger; residual paths
	// explicitly stage an existing ledger and are never presented as no-ledger evidence.
	for _, path := range []string{"root-D5", "root-D8", "reconciling", "reclaiming", "cleared", "controlled"} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/later=%v", path, later), func(t *testing.T) {
				f := setup(t)
				scene, req := lateDeliveryRegistered(t, f)
				switch path {
				case "root-D5", "root-D8":
					lateDeliveryGiveUp(t, f, scene, path == "root-D8")
				case "reconciling":
					seedControlledTerminalIntake(t, f, scene)
				case "controlled":
					seedControlledTerminalIntake(t, f, scene)
					controlledTerminate(t, f, scene.tenantID, scene.runID, f.runWorkspaceID(scene.runID))
				default:
					seedResidualTerminalIntake(t, f, scene, path)
				}
				if later {
					next := lateDeliveryEnqueueLater(t, f, scene)
					// Wall-clock columns intentionally tell the opposite story from creation seq.
					_, err := f.store.Pool.Exec(`UPDATE issue_runs SET created_at=(SELECT created_at-interval '1 hour' FROM issue_runs WHERE id=$2) WHERE id=$1`, next, scene.runID)
					must(t, err)
				}
				before := lateDeliveryBusinessSnapshot(t, f, scene)
				authority := ""
				if path != "root-D5" && path != "root-D8" {
					authority = lateDeliveryResidualAuthority(t, f, scene)
				}
				lateDeliveryNoSettlement(f)
				if later {
					lateDeliveryReceiptOnly(t, f, req)
				} else {
					lateDeliveryAccepted(t, f, req)
					if f.revisionRow(scene.runID) == nil || f.revisionVerification(req.ExecutionId).S("outcome") != "delivered" {
						t.Fatal("no-later-Run intake failed to rescue the original verified Revision")
					}
				}
				replay := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
				replay.SubmissionId = "late-order-replay"
				reads, probes := len(f.refs.queried()), len(f.objects.probed())
				lateDeliveryAccepted(t, f, replay)
				if len(f.refs.queried()) != reads || len(f.objects.probed()) != probes || lateDeliveryBusinessSnapshot(t, f, scene) != before {
					t.Fatal("ordering decision replay reread external evidence or changed business")
				}
				if authority != "" && lateDeliveryResidualAuthority(t, f, scene) != authority {
					t.Fatal("late intake changed existing residual responsibility, authorization, disposal state or controlled intent")
				}
				if authority != "" {
					conflict := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
					conflict.SubmissionId = "late-order-residual-conflict"
					conflict.Result.GetRevisionDelivered().FinalCommit = resumedFinalCommit
					response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), conflict)
					if response != nil || status.Convert(err).Message() != "residual_fact_conflict" ||
						f.scalar("SELECT count(*) FROM agent_run_residuals WHERE run_id=$1 AND reclaim_conflicted_at IS NOT NULL", scene.runID) != 1 {
						t.Fatalf("residual canonical conflict lost its committed audit: %v %v", response, err)
					}
					if lateDeliveryResidualAuthority(t, f, scene) != authority || len(f.refs.queried()) != reads || len(f.objects.probed()) != probes {
						t.Fatal("residual conflict changed authority or reread external evidence")
					}
				}
				if later && (f.revisionRow(scene.runID) != nil || f.revisionVerification(req.ExecutionId) != nil) {
					t.Fatal("receipt-only replay was promoted into a registration opportunity")
				}
			})
		}
	}
}

func TestLateDeliveryFactLaterRunStatesAndMissingOrderOverGRPC(t *testing.T) {
	for _, state := range []string{"queued", "running", "terminal", "soft-deleted", "original-missing", "original-ambiguous", "later-missing"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryRegistered(t, f)
			lateDeliveryGiveUp(t, f, scene, false)
			later := ""
			if state != "original-missing" && state != "original-ambiguous" {
				later = lateDeliveryEnqueueLater(t, f, scene)
			}
			switch state {
			case "running":
				_, err := f.store.Pool.Exec("UPDATE issue_runs SET status='running',phase='running' WHERE id=$1", later)
				must(t, err)
			case "terminal", "soft-deleted":
				_, err := f.store.Pool.Exec(`UPDATE issue_runs SET status='cancelled',phase='done',completed_at=now(),
 deleted_at=CASE WHEN $2='soft-deleted' THEN now() ELSE NULL END WHERE id=$1`, later, state)
				must(t, err)
			case "original-missing", "later-missing":
				id := scene.runID
				if state == "later-missing" {
					id = later
				}
				_, err := f.store.Pool.Exec("DELETE FROM issue_activities WHERE action='run.enqueued' AND details->>'runId'=$1", id)
				must(t, err)
			case "original-ambiguous":
				_, err := f.store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
 SELECT gen_random_uuid(),a.tenant_id,a.issue_id,(SELECT max(seq)+5 FROM issue_activities WHERE issue_id=a.issue_id),
 a.actor_type,a.actor_id,a.action,a.details FROM issue_activities a WHERE a.action='run.enqueued' AND a.details->>'runId'=$1`, scene.runID)
				must(t, err)
			}
			before := lateDeliveryBusinessSnapshot(t, f, scene)
			lateDeliveryReceiptOnly(t, f, req)
			if before != lateDeliveryBusinessSnapshot(t, f, scene) {
				t.Fatal("receipt-only fallback changed already-settled business")
			}
		})
	}
}

func TestLateDeliveryFactResumeKeepsLaterRevisionOverGRPC(t *testing.T) {
	f := setup(t)
	original, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, original, true)
	// A follow-up workspace is a storage fixture, then its session/end/delivery are production.
	// Its run.enqueued event is inserted as ordering fixture evidence before any execution.
	later := seedFollowUpRun(t, f, original, false)
	_, err := f.store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
 SELECT gen_random_uuid(),tenant_id,id,(SELECT max(seq)+7 FROM issue_activities WHERE issue_id=$1),
 'user',creator_user_id,'run.enqueued',jsonb_build_object('runId',$2::text) FROM issues WHERE id=$1`, original.issueID, later.runID)
	must(t, err)
	later.start(t, f)
	f.runningThread(t, later)
	f.sessionEndOK(t, later, "later-session-end", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	newExecution := f.deliverRevision(t, later)
	in := f.deliveryInputOf(newExecution)
	f.push(in, resumedFinalCommit)
	_, err = f.deliveryTerminal(later, newExecution, "later-revision", 1, deliveredResult(later, in, resumedFinalCommit), "later result")
	must(t, err)
	newRevision := f.revisionRow(later.runID)
	if newRevision == nil {
		t.Fatal("later Run did not produce its real verified Revision")
	}
	lateDeliveryReceiptOnly(t, f, req)
	// Starting the next session executes the actual candidate selector and freezes priorRevision.
	next := seedFollowUpRun(t, f, later, false)
	next.start(t, f)
	prior := f.sessionInput(next.executionID).O("priorRevision")
	if prior.S("revisionId") != newRevision.ID || prior.S("finalCommit") != resumedFinalCommit {
		t.Fatalf("late original fact displaced the later resume candidate: %v", prior)
	}
}

func TestLateDeliveryFactNoLaterRunResumesRescuedRevisionOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, true)
	// A larger creation sequence in another tenant/Issue cannot block this Issue. Equal
	// sequences would not catch an ordering query that accidentally dropped those scope fences.
	unrelated := seedLiveThreadScene(t, f)
	_, err := f.store.Pool.Exec(`UPDATE issue_activities SET seq=(
 SELECT seq+10 FROM issue_activities WHERE action='run.enqueued' AND details->>'runId'=$2)
 WHERE action='run.enqueued' AND details->>'runId'=$1`, unrelated.runID, scene.runID)
	must(t, err)
	var unrelatedIsLater bool
	must(t, f.store.Pool.QueryRow(`SELECT a.seq>b.seq FROM issue_activities a CROSS JOIN issue_activities b
 WHERE a.action='run.enqueued' AND a.details->>'runId'=$1 AND b.action='run.enqueued' AND b.details->>'runId'=$2`, unrelated.runID, scene.runID).Scan(&unrelatedIsLater))
	if unrelated.tenantID == scene.tenantID || unrelated.issueID == scene.issueID || !unrelatedIsLater {
		t.Fatal("unrelated Run must belong to another tenant/Issue and have a larger creation sequence")
	}
	_, err = f.store.Pool.Exec("UPDATE issue_runs SET phase='done',status='cancelled',completed_at=now() WHERE id=$1", unrelated.runID)
	must(t, err)
	lateDeliveryAccepted(t, f, req)
	row := f.revisionRow(scene.runID)
	if row == nil {
		t.Fatal("unrelated Issue prevented original Revision rescue")
	}
	next := seedFollowUpRun(t, f, scene, false)
	// The storage fixture must include its real creation record before session startup and
	// the replay below; seedFollowUpRun itself only creates the Run and runtime resources.
	_, err = f.store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
 SELECT gen_random_uuid(),tenant_id,id,(SELECT max(seq)+7 FROM issue_activities WHERE issue_id=$1),
 'user',creator_user_id,'run.enqueued',jsonb_build_object('runId',$2::text) FROM issues WHERE id=$1`, scene.issueID, next.runID)
	must(t, err)
	next.start(t, f)
	if prior := f.sessionInput(next.executionID).O("priorRevision"); prior.S("revisionId") != row.ID {
		t.Fatalf("rescued original Revision was not the next session's frozen candidate: %v", prior)
	}
	// Already-registered late Revision remains stable when a later Run has since been created.
	replay := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
	replay.SubmissionId = "late-after-next-created"
	lateDeliveryAccepted(t, f, replay)
	if f.revisionRow(scene.runID).ID != row.ID {
		t.Fatal("a later Run caused replay to delete or replace an already-registered Revision")
	}
}
