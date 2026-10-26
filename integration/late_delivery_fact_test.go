package integration

// IssueRun late-delivery decision D1-D3. The starting Run/Workspace is the shared storage
// fixture; session and delivery registration, D5, D8 and every terminal intake use production
// entrypoints. A formed terminal's gRPC request is cut before Cloud accepts it, then the original
// event is retained until after give-up. Cloud has neither its result nor its receipt.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

func lateDeliveryRegistered(t *testing.T, f *fixture) (liveThreadScene, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
	req := &controlpb.TakeOverNodeEventRequest{
		SubmissionId: "late-terminal", Epoch: 1, OperationId: scene.runID,
		ExecutionId: execution, Sequence: 1, Result: deliveredResult(scene, in, revisionFinalCommit),
		Event: []byte("original Node delivery terminal, formed before Cloud give-up"),
	}
	// Cancel the real RPC before transport can deliver it; the Node must retain this exact
	// terminal rather than replacing its identity after the connection is lost.
	ctx, cancel := context.WithCancel(asController("ctrl-a"))
	cancel()
	response, err := f.executions.TakeOverNodeEvent(ctx, req)
	if response != nil || status.Code(err) != codes.Canceled {
		t.Fatalf("cut-before-acceptance returned a terminal response: %v %v", response, err)
	}
	lateDeliveryNoFact(t, f, req)
	return scene, req
}

func lateDeliveryGiveUp(t *testing.T, f *fixture, scene liveThreadScene, done bool) {
	t.Helper()
	f.store.DeliveryGiveUpAfter = time.Nanosecond
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if f.runPhase(scene.runID) != "releasing" || f.runResult(scene.runID).S("deliveryState") != "failed" || f.runResult(scene.runID)["revisionId"] != nil {
		t.Fatal("production D5 did not abandon the outstanding delivery and declare release")
	}
	if done {
		f.store.DeliveryUnreachableAfter = time.Hour
		must(t, f.ageRunWorkspaceNodeBy(scene.runID, 2*time.Hour))
		must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
		status, _ := f.runStatusVersion(scene.runID)
		assertGiveUpSettlement(t, f, scene.runID, status)
	}
	if f.scalar("SELECT count(*) FROM agent_run_residuals WHERE run_id=$1", scene.runID) != 0 ||
		f.scalar("SELECT count(*) FROM runtime_force_stops WHERE workspace_id=$1", f.runWorkspaceID(scene.runID)) != 0 {
		t.Fatal("root D5/D8 fixture unexpectedly acquired a residual ledger or controlled intent")
	}
}

func lateDeliveryBusinessSnapshot(t *testing.T, f *fixture, scene liveThreadScene) string {
	t.Helper()
	var snapshot string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'run',to_jsonb(r),'workspace',to_jsonb(w),
 'operations',(SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY o.id),'[]') FROM operations o WHERE o.workspace_id=w.id),
 'runtime',(SELECT to_jsonb(c) FROM runtime_controls c WHERE c.workspace_id=w.id),
 'sandbox',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.id),'[]') FROM sandbox_instances s WHERE s.workspace_id=w.id),
 'work',(SELECT count(*) FROM execution_work WHERE run_id=r.id),
 'executions',(SELECT count(*) FROM node_executions WHERE workspace_id=w.id),
 'effects',(SELECT count(*) FROM external_effects WHERE workspace_id=w.id),
 'activities',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.seq),'[]') FROM issue_activities a WHERE a.issue_id=r.issue_id),
 'residuals',(SELECT count(*) FROM agent_run_residuals WHERE run_id=r.id),
 'intents',(SELECT count(*) FROM runtime_force_stops WHERE workspace_id=w.id))::text
 FROM issue_runs r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`, scene.runID).Scan(&snapshot))
	return snapshot
}

func lateDeliveryNoSettlement(f *fixture) {
	f.store.OnDeliverySettled = func(context.Context, *sql.Tx, string, string, core.Object) error {
		return errors.New("late fact invoked the business settlement hook")
	}
}

func lateDeliveryAccepted(t *testing.T, f *fixture, req *controlpb.TakeOverNodeEventRequest) *controlpb.TakeOverNodeEventResponse {
	t.Helper()
	response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
	must(t, err)
	if response.GetRecord().GetExecutionId() != req.ExecutionId || !proto.Equal(response.GetRecord().GetResult(), req.Result) {
		t.Fatalf("late acceptance did not return the exact original execution/result: %v", response)
	}
	if f.nodeSequence(req.ExecutionId) != int64(req.Sequence) ||
		f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1 AND sequence=$2", req.ExecutionId, req.Sequence) != 1 ||
		f.receiptEvent(req.ExecutionId, int64(req.Sequence)) != string(req.Event) ||
		f.scalar("SELECT count(*) FROM control_submissions WHERE submission_id=$1", req.SubmissionId) != 1 {
		t.Fatal("successful takeover preceded original result, exact receipt or durable submission")
	}
	return response
}

func lateDeliveryNoFact(t *testing.T, f *fixture, req *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	if f.nodeResult(req.ExecutionId) != nil || f.nodeSequence(req.ExecutionId) != 0 ||
		f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", req.ExecutionId) != 0 ||
		f.scalar("SELECT count(*) FROM revision_verifications WHERE execution_id=$1", req.ExecutionId) != 0 ||
		f.scalar("SELECT count(*) FROM revisions WHERE execution_id=$1", req.ExecutionId) != 0 ||
		f.scalar("SELECT count(*) FROM control_submissions WHERE submission_id=$1", req.SubmissionId) != 0 {
		t.Fatal("refused or rolled-back terminal left an ACKable fact, verification or submission")
	}
}

func TestLateDeliveryFactRootD5AndD8OverGRPC(t *testing.T) {
	for _, done := range []bool{false, true} {
		for _, outcome := range []string{"delivered", "unchanged", "failed"} {
			t.Run(fmt.Sprintf("done=%v/%s", done, outcome), func(t *testing.T) {
				f := setup(t)
				scene, req := lateDeliveryRegistered(t, f)
				in := f.deliveryInputOf(req.ExecutionId)
				switch outcome {
				case "unchanged":
					req.Result = unchangedResult(scene, in)
					f.push(in, in.S("baseCommit"))
				case "failed":
					req.Result = revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED)
				}
				lateDeliveryNoFact(t, f, req)
				lateDeliveryGiveUp(t, f, scene, done)
				lateDeliveryNoSettlement(f)
				before := lateDeliveryBusinessSnapshot(t, f, scene)
				accepted := lateDeliveryAccepted(t, f, req)
				if outcome == "failed" {
					if f.revisionRow(scene.runID) != nil || f.revisionVerification(req.ExecutionId) != nil {
						t.Fatal("late Node failure invented a verified Revision")
					}
				} else {
					row := f.revisionRow(scene.runID)
					if row == nil || row.RunID != scene.runID || row.WorkspaceID != f.runWorkspaceID(scene.runID) || row.TenantID != scene.tenantID ||
						row.RevisionRef != in.S("revisionRef") || row.DeliveryRef != in.S("deliveryRef") || row.HistoryKey != in.S("historyKey") ||
						row.BaseCommit != in.S("baseCommit") || f.revisionVerification(req.ExecutionId).S("outcome") != outcome {
						t.Fatalf("late success lost original identity or actual verification: %#v", row)
					}
				}
				reads, probes := len(f.refs.queried()), len(f.objects.probed())
				for _, submission := range []string{req.SubmissionId, "late-terminal-new-submission"} {
					replay := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
					replay.SubmissionId = submission
					if response := lateDeliveryAccepted(t, f, replay); !proto.Equal(response, accepted) {
						t.Fatal("replay changed the original complete takeover response")
					}
				}
				if len(f.refs.queried()) != reads || len(f.objects.probed()) != probes ||
					f.scalar("SELECT count(*) FROM revisions WHERE run_id=$1", scene.runID) > 1 {
					t.Fatal("same terminal reread external evidence or registered another Revision")
				}
				if done {
					must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
					must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
				}
				if after := lateDeliveryBusinessSnapshot(t, f, scene); after != before {
					t.Fatalf("late fact/replay changed business, failed delete or resource permissions:\n%s\n%s", before, after)
				}
			})
		}
	}
}

func TestLateDeliveryFactAtomicRollbackOverGRPC(t *testing.T) {
	for _, later := range []bool{false, true} {
		for _, table := range []string{"node_executions", "node_event_receipts", "control_submissions", "revision_verifications", "revisions"} {
			if later && (table == "revision_verifications" || table == "revisions") {
				continue // Receipt-only intake never writes these tables.
			}
			t.Run(fmt.Sprintf("later=%v/%s", later, table), func(t *testing.T) {
				f := setup(t)
				scene, req := lateDeliveryRegistered(t, f)
				lateDeliveryGiveUp(t, f, scene, later)
				if later {
					lateDeliveryEnqueueLater(t, f, scene)
				}
				lateDeliveryNoSettlement(f)
				before := lateDeliveryBusinessSnapshot(t, f, scene)
				clause := "INSERT"
				if table == "node_executions" {
					clause = "UPDATE OF result"
				}
				_, err := f.store.Pool.Exec(`CREATE FUNCTION reject_late_delivery_write() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected late delivery persistence failure'; END $$;
 CREATE TRIGGER reject_late_delivery_write BEFORE ` + clause + ` ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION reject_late_delivery_write()`)
				must(t, err)
				response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
				if err == nil || response != nil {
					t.Fatalf("%s failure returned an ACKable response: %v %v", table, response, err)
				}
				lateDeliveryNoFact(t, f, req)
				_, err = f.store.Pool.Exec("DROP TRIGGER reject_late_delivery_write ON " + table + "; DROP FUNCTION reject_late_delivery_write()")
				must(t, err)
				if later {
					lateDeliveryReceiptOnly(t, f, req)
				} else {
					lateDeliveryAccepted(t, f, req)
				}
				if after := lateDeliveryBusinessSnapshot(t, f, scene); after != before {
					t.Fatal("failed write or clean retry changed business state")
				}
			})
		}
	}
}

func TestLateDeliveryFactIdentitySequenceAndConflictFencesOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, true)
	before := lateDeliveryBusinessSnapshot(t, f, scene)
	for _, tc := range []struct {
		name   string
		change func(*controlpb.TakeOverNodeEventRequest)
		code   codes.Code
	}{
		{"gap", func(r *controlpb.TakeOverNodeEventRequest) { r.Sequence = 2 }, codes.Aborted},
		{"node", func(r *controlpb.TakeOverNodeEventRequest) { r.Result.Node.NodeId = "wrong-node" }, codes.Aborted},
		{"operation", func(r *controlpb.TakeOverNodeEventRequest) { r.OperationId = uuid.NewString() }, codes.NotFound},
		{"unknown-execution", func(r *controlpb.TakeOverNodeEventRequest) { r.ExecutionId = "not-registered" }, codes.NotFound},
		{"frozen-base", func(r *controlpb.TakeOverNodeEventRequest) {
			r.Result.GetRevisionDelivered().BaseCommit = strings.Repeat("2", 40)
		}, codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
			bad.SubmissionId = "late-invalid-" + tc.name
			tc.change(bad)
			response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), bad)
			if response != nil || status.Code(err) != tc.code {
				t.Fatalf("invalid original identity/sequence got %v %v; want %v", response, err, tc.code)
			}
		})
	}
	if len(f.refs.queried()) != 0 || len(f.objects.probed()) != 0 {
		t.Fatal("invalid original identity or sequence reached external verification")
	}
	lateDeliveryAccepted(t, f, req)
	if len(f.refs.queried()) != 1 || len(f.objects.probed()) != 1 ||
		lateDeliveryBusinessSnapshot(t, f, scene) != before {
		t.Fatal("invalid intake or acceptance changed business or repeated external verification")
	}
}

// All replay checks start from a terminal accepted through the real intake. The receipt-only
// variant is selected by a production-created later Run, never by deleting a verified Revision.
func lateDeliveryAcceptedState(t *testing.T, f *fixture, state string) (liveThreadScene, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	scene, req := lateDeliveryRegistered(t, f)
	if state == "revision_failed" {
		req.Result = revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED)
	}
	lateDeliveryGiveUp(t, f, scene, true)
	if state == "receipt-only" {
		lateDeliveryEnqueueLater(t, f, scene)
	}
	lateDeliveryNoSettlement(f)
	if state == "receipt-only" {
		lateDeliveryReceiptOnly(t, f, req)
	} else {
		lateDeliveryAccepted(t, f, req)
	}
	return scene, req
}

func lateDeliveryCanonicalSnapshot(t *testing.T, f *fixture, execution string) string {
	t.Helper()
	var snapshot string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'execution',to_jsonb(e),
 'receipts',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.sequence),'[]') FROM node_event_receipts r WHERE r.execution_id=e.execution_id),
 'verification',(SELECT to_jsonb(v) FROM revision_verifications v WHERE v.execution_id=e.execution_id),
 'revision',(SELECT to_jsonb(r) FROM revisions r WHERE r.execution_id=e.execution_id))::text
 FROM node_executions e WHERE e.execution_id=$1`, execution).Scan(&snapshot))
	return snapshot
}

func TestLateDeliveryFactAcceptedConflictMatrixOverGRPC(t *testing.T) {
	for _, state := range []string{"verified", "receipt-only", "revision_failed"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryAcceptedState(t, f, state)
			canonical, business := lateDeliveryCanonicalSnapshot(t, f, req.ExecutionId), lateDeliveryBusinessSnapshot(t, f, scene)
			reads, probes := len(f.refs.queried()), len(f.objects.probed())
			for _, tc := range []struct {
				name   string
				change func(*controlpb.TakeOverNodeEventRequest)
			}{
				{"event", func(r *controlpb.TakeOverNodeEventRequest) { r.Event = []byte("changed original event") }},
				{"result", func(r *controlpb.TakeOverNodeEventRequest) {
					if state == "revision_failed" {
						r.Result.GetRevisionFailed().Reason = controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_PUSH_FAILED
					} else {
						r.Result.GetRevisionDelivered().FinalCommit = strings.Repeat("2", 40)
					}
				}},
				{"extra-sequence", func(r *controlpb.TakeOverNodeEventRequest) { r.Sequence = 2 }},
				{"submission-content", func(r *controlpb.TakeOverNodeEventRequest) {
					r.SubmissionId = req.SubmissionId
					r.Event = []byte("different same submission")
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bad := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
					bad.SubmissionId = "late-conflict-" + tc.name
					tc.change(bad)
					response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), bad)
					if response != nil {
						t.Fatalf("conflicting accepted %s terminal became ACKable: %v", state, response)
					}
					expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
					if bad.SubmissionId != req.SubmissionId && f.scalar("SELECT count(*) FROM control_submissions WHERE submission_id=$1", bad.SubmissionId) != 0 {
						t.Fatal("refused conflict persisted a successful submission")
					}
				})
			}
			if lateDeliveryCanonicalSnapshot(t, f, req.ExecutionId) != canonical || lateDeliveryBusinessSnapshot(t, f, scene) != business ||
				len(f.refs.queried()) != reads || len(f.objects.probed()) != probes {
				t.Fatal("accepted-terminal conflict rewrote canonical facts/business or repeated external verification")
			}
		})
	}
}

func TestLateDeliveryFactReplayLeaseAndOriginalResourceFencesOverGRPC(t *testing.T) {
	for _, state := range []string{"verified", "receipt-only", "revision_failed"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryAcceptedState(t, f, state)
			canonical, business := lateDeliveryCanonicalSnapshot(t, f, req.ExecutionId), lateDeliveryBusinessSnapshot(t, f, scene)
			reads, probes := len(f.refs.queried()), len(f.objects.probed())
			for _, sameSubmission := range []bool{false, true} {
				for _, fence := range []string{"epoch", "holder", "expired", "node", "operation", "execution"} {
					t.Run(fmt.Sprintf("same-submission=%v/%s", sameSubmission, fence), func(t *testing.T) {
						replay := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
						if !sameSubmission {
							replay.SubmissionId = "late-fenced-replay-" + fence
						}
						ctx := asController("ctrl-a")
						code, detail := codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT
						switch fence {
						case "epoch":
							replay.Epoch++
						case "holder":
							ctx = asController("wrong-controller")
						case "expired":
							_, err := f.store.Pool.Exec("UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE name='global'")
							must(t, err)
							defer func() {
								_, err := f.store.Pool.Exec("UPDATE controller_leases SET expires_at=clock_timestamp()+interval '30 minutes' WHERE name='global'")
								must(t, err)
							}()
						case "node":
							replay.Result.Node.NodeId = "wrong-original-node"
						case "operation":
							replay.OperationId = uuid.NewString()
						case "execution":
							replay.ExecutionId = "not-the-original-execution"
						}
						if fence == "epoch" || fence == "holder" || fence == "expired" {
							code, detail = codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER
						} else if !sameSubmission && (fence == "operation" || fence == "execution") {
							code, detail = codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND
						}
						response, err := f.executions.TakeOverNodeEvent(ctx, replay)
						if response != nil {
							t.Fatalf("replay bypassed %s fence: %v", fence, response)
						}
						expectStatus(t, err, code, detail)
						if !sameSubmission && f.scalar("SELECT count(*) FROM control_submissions WHERE submission_id=$1", replay.SubmissionId) != 0 {
							t.Fatal("fenced replay persisted a successful submission")
						}
					})
				}
			}
			if lateDeliveryCanonicalSnapshot(t, f, req.ExecutionId) != canonical || lateDeliveryBusinessSnapshot(t, f, scene) != business ||
				len(f.refs.queried()) != reads || len(f.objects.probed()) != probes {
				t.Fatal("fenced replay changed canonical facts/business or repeated external evidence or settlement")
			}
		})
	}
}
