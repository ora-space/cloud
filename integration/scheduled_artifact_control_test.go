package integration

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// scheduledArtifactScene crosses the shipped gRPC takeover, claim and dispatch boundaries. Only the
// admission precondition is staged; no artifact work or execution rows are inserted by the test.
func scheduledArtifactScene(t *testing.T, f *fixture) (liveThreadScene, string) {
	t.Helper()
	return scheduledArtifactSceneForManifest(t, f, &controlpb.ScheduledArtifactManifest{
		Items: []*controlpb.ScheduledArtifactItem{{
			Index: 3, DisplayName: "report.md", Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_MARKDOWN,
			SizeBytes: revisionHistorySize, Sha256: revisionHistorySHA,
		}},
		Rejections: []*controlpb.ScheduledArtifactRejection{{DisplayName: "notes.pdf", Reason: controlpb.ScheduledArtifactRejectionReason_SCHEDULED_ARTIFACT_REJECTION_REASON_UNSUPPORTED_TYPE}},
	})
}

func scheduledArtifactSceneForManifest(t *testing.T, f *fixture, manifest *controlpb.ScheduledArtifactManifest) (liveThreadScene, string) {
	t.Helper()
	scene := seedLiveThreadScene(t, f)
	var taskID string
	must(t, f.store.Pool.QueryRow(`INSERT INTO scheduled_tasks
		(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled)
		SELECT gen_random_uuid(),tenant_id,issue_id,$2,executor_id,'artifact-test','review',
		'{"kind":"daily","hour":0,"minute":0}'::jsonb,'UTC',1,1,false FROM issue_runs WHERE id=$1
		RETURNING id`, scene.runID, f.uid).Scan(&taskID))
	_, err := f.store.Pool.Exec(`INSERT INTO scheduled_task_occurrences
		(tenant_id,task_id,scheduled_at,outcome,task_revision,observed_at,run_id)
		VALUES($1,$2,now(),'admitted',1,now(),$3)`, scene.tenantID, taskID, scene.runID)
	must(t, err)
	scene.start(t, f)
	ended := sessionEndResult(scene, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_IDLE_TIMEOUT, "")
	ended.GetAgentSessionEnded().ArtifactManifest = manifest
	_, err = f.executions.TakeOverNodeEvent(asController("ctrl-a"), &controlpb.TakeOverNodeEventRequest{
		Epoch: 1, SubmissionId: "declare-artifacts", OperationId: scene.runID, ExecutionId: scene.executionID,
		Sequence: 1, Event: []byte("declare-artifacts"), Result: ended,
	})
	must(t, err)
	queried, err := f.executions.GetDispatch(asController("ctrl-a"), &controlpb.GetDispatchRequest{ExecutionId: scene.executionID})
	must(t, err)
	if !proto.Equal(queried.GetRecord().GetResult(), ended) {
		t.Fatalf("session recovery got %v; want %v", queried.GetRecord().GetResult(), ended)
	}
	var uploadExecution string
	// Revision and artifact deliveries are independently dispatchable while the other is pending.
	for i := 0; i < 2; i++ {
		claimed, err := f.executions.ClaimWork(asController("ctrl-a"), &controlpb.ClaimWorkRequest{Epoch: 1})
		must(t, err)
		item := claimed.GetItem()
		if item == nil || item.GetOperationId() != scene.runID {
			t.Fatal("session end must release both delivery work items")
		}
		execution := "scheduled-revision"
		if item.GetInput().GetDeliverScheduledArtifacts() != nil {
			execution = "scheduled-upload"
			uploadExecution = execution
		}
		_, err = f.executions.RecordDispatch(asController("ctrl-a"), &controlpb.RecordDispatchRequest{
			Epoch: 1, SubmissionId: execution, OperationId: scene.runID, ExecutionId: execution,
			NodeId: item.GetTarget().GetNodeId(), Input: item.GetInput(),
		})
		must(t, err)
	}
	if uploadExecution == "" {
		t.Fatal("artifact upload work was not dispatched")
	}
	return scene, uploadExecution
}

func scheduledGrantRequest(execution string) *controlpb.GrantScheduledArtifactUploadRequest {
	return &controlpb.GrantScheduledArtifactUploadRequest{
		Epoch: 1, ExecutionId: execution,
		Artifacts: []*controlpb.ScheduledArtifactUpload{{Index: 3, Size: revisionHistorySize, Sha256: revisionHistorySHA}},
	}
}

func scheduledUploadResult(scene liveThreadScene, key string) *controlpb.ExecutionResult {
	return &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_ScheduledArtifactsDelivered{ScheduledArtifactsDelivered: &controlpb.ScheduledArtifactsDelivered{
			Artifacts: []*controlpb.StoredObject{{Key: key, Size: revisionHistorySize, Sha256: revisionHistorySHA}},
		}},
	}
}

func TestScheduledArtifactGRPCDispatchGrantAndRegistration(t *testing.T) {
	f := setup(t)
	scene, execution := scheduledArtifactScene(t, f)
	client := controlpb.NewAgentRunServiceClient(f.controlConn)
	req := scheduledGrantRequest(execution)
	_, err := client.GrantScheduledArtifactUpload(asController("another-controller"), req)
	if err == nil {
		t.Fatal("another controller must not obtain an upload capability")
	}
	bad := proto.Clone(req).(*controlpb.GrantScheduledArtifactUploadRequest)
	bad.Artifacts[0].Index++
	_, err = client.GrantScheduledArtifactUpload(asController("ctrl-a"), bad)
	expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
	granted, err := client.GrantScheduledArtifactUpload(asController("ctrl-a"), req)
	must(t, err)
	if len(granted.Grants) != 1 {
		t.Fatal("exactly one declared artifact must be granted")
	}
	grant := granted.Grants[0]
	var deadline time.Time
	must(t, f.store.Pool.QueryRow(`SELECT deadline_at FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1`, scene.runID).Scan(&deadline))
	if grant.GetExpiresAt().AsTime().After(deadline) {
		t.Fatal("upload grant outlives the frozen delivery deadline")
	}
	_, err = f.deliveryTerminal(scene, execution, "uploaded-artifact", 1, scheduledUploadResult(scene, grant.GetObjectKey()), "uploaded-artifact")
	must(t, err)
	if f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1 AND artifact_index=3`, scene.runID) != 1 {
		t.Fatal("verified artifact was not registered through the real gRPC path")
	}
	if f.scalar(`SELECT count(*) FROM scheduled_run_artifact_rejections WHERE issue_run_id=$1`, scene.runID) != 1 {
		t.Fatal("the executor's rejected candidate must remain visible")
	}
	if len(f.objects.probed()) != 1 {
		t.Fatal("registration must verify exactly the declared object")
	}
	finishScheduledRevision(t, f, scene)
	if f.scalar(`SELECT count(*) FROM issue_runs WHERE id=$1 AND phase='releasing'`, scene.runID) != 1 {
		t.Fatal("the last delivery must release the run")
	}
}

func finishScheduledRevision(t *testing.T, f *fixture, scene liveThreadScene) {
	t.Helper()
	in := f.deliveryInputOf("scheduled-revision")
	f.push(in, in.S("baseCommit"))
	_, err := f.deliveryTerminal(scene, "scheduled-revision", "scheduled-revision-finished", 1,
		unchangedResult(scene, in), "scheduled-revision-finished")
	must(t, err)
}

func TestScheduledArtifactGRPCRevisionSettlesFirst(t *testing.T) {
	f := setup(t)
	scene, execution := scheduledArtifactScene(t, f)
	finishScheduledRevision(t, f, scene)
	if f.scalar(`SELECT count(*) FROM issue_runs WHERE id=$1 AND phase='delivering'`, scene.runID) != 1 {
		t.Fatal("a settled Revision must wait for the independent artifact delivery")
	}
	key := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/3"
	_, err := f.deliveryTerminal(scene, execution, "artifacts-last", 1, scheduledUploadResult(scene, key), "artifacts-last")
	must(t, err)
	if f.scalar(`SELECT count(*) FROM issue_runs WHERE id=$1 AND phase='releasing'`, scene.runID) != 1 {
		t.Fatal("artifact settlement must release a run whose Revision already settled")
	}
}

func TestScheduledArtifactGRPCGrantLifetime(t *testing.T) {
	f := setup(t)
	scene, execution := scheduledArtifactScene(t, f)
	client := controlpb.NewAgentRunServiceClient(f.controlConn)
	for _, tc := range []struct {
		name      string
		remaining string
		ttl       time.Duration
		maximum   time.Duration
	}{
		{"remaining-deadline", "30 seconds", 15 * time.Minute, 30 * time.Second},
		{"configured-ttl", "10 minutes", 20 * time.Second, 20 * time.Second},
		{"subsecond-expiry", "0.5 seconds", 15 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.objects.config.UploadGrantTTL = tc.ttl
			_, err := f.store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
				SET deadline_at=clock_timestamp()+$2::interval WHERE issue_run_id=$1`, scene.runID, tc.remaining)
			must(t, err)
			granted, err := client.GrantScheduledArtifactUpload(asController("ctrl-a"), scheduledGrantRequest(execution))
			if tc.maximum == 0 {
				if err == nil {
					t.Fatal("a subsecond remainder must not round up into a new upload capability")
				}
				return
			}
			must(t, err)
			grant := granted.Grants[0]
			var deadline time.Time
			must(t, f.store.Pool.QueryRow(`SELECT deadline_at FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1`, scene.runID).Scan(&deadline))
			u, err := url.Parse(grant.GetUrl())
			must(t, err)
			issued, err := time.Parse("20060102T150405Z", u.Query().Get("X-Amz-Date"))
			must(t, err)
			seconds, err := strconv.Atoi(u.Query().Get("X-Amz-Expires"))
			must(t, err)
			lifetime := time.Duration(seconds) * time.Second
			if lifetime > tc.maximum || lifetime <= 0 || issued.Add(lifetime).After(deadline) ||
				!issued.Add(lifetime).Equal(grant.GetExpiresAt().AsTime()) {
				t.Fatal("the signed expiry must agree with both the response and the frozen deadline")
			}
		})
	}
}

func TestScheduledArtifactGRPCLateSuccessACK(t *testing.T) {
	for _, scanFirst := range []bool{false, true} {
		name := "receipt-before-scan"
		if scanFirst {
			name = "scan-before-receipt"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			scene, execution := scheduledArtifactScene(t, f)
			_, err := f.store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET deadline_at=now()-interval '1 second' WHERE issue_run_id=$1`, scene.runID)
			must(t, err)
			if scanFirst {
				must(t, f.store.ExpireScheduledArtifactDeliveriesOnce(context.Background()))
			}
			key := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/3"
			result := scheduledUploadResult(scene, key)
			// An unavailable object store must not prevent acknowledgement after the deadline.
			f.store.ObjectStore = nil
			wrongNode := proto.Clone(result).(*controlpb.ExecutionResult)
			wrongNode.Node.NodeId = "unrelated-node"
			_, err = f.deliveryTerminal(scene, execution, "wrong-node", 1, wrongNode, "late-upload")
			if err == nil {
				t.Fatal("expiry must not bypass the original execution's Node identity")
			}
			for _, submission := range []string{"late-upload", "late-upload", "late-upload-replay"} {
				_, err := f.deliveryTerminal(scene, execution, submission, 1, result, "late-upload")
				must(t, err)
			}
			if f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, execution) != 1 ||
				f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1`, scene.runID) != 0 {
				t.Fatal("late success must ACK one receipt and register no objects")
			}
			if len(f.objects.probed()) != 0 {
				t.Fatal("late success must not HEAD an object that cannot be registered")
			}
		})
	}
}
