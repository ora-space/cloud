package integration

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

func TestScheduledArtifactGRPCBatchFailureRejections(t *testing.T) {
	for _, order := range []string{"failure", "receipt-before-scan", "scan-before-receipt", "failure-after-expiry"} {
		t.Run(order, func(t *testing.T) {
			f := setup(t)
			scene, execution := scheduledArtifactScene(t, f)
			result := scheduledUploadResult(scene, "scheduled-runs/"+scene.tenantID+"/"+scene.runID+"/3")
			reason := "deadline_expired"
			if order == "failure" || order == "failure-after-expiry" {
				result.Outcome = &controlpb.ExecutionResult_ScheduledArtifactsFailed{ScheduledArtifactsFailed: &controlpb.ScheduledArtifactsFailed{
					Reason: controlpb.ScheduledArtifactsFailureReason_SCHEDULED_ARTIFACTS_FAILURE_REASON_UPLOAD_FAILED,
				}}
			}
			if order == "failure" {
				reason = "upload_failed"
			} else {
				_, err := f.store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
					SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.runID)
				must(t, err)
			}
			if order == "scan-before-receipt" {
				must(t, f.store.ExpireScheduledArtifactDeliveriesOnce(t.Context()))
				assertScheduledArtifactRejections(t, f, scene.runID, [][2]string{{"notes.pdf", "unsupported_type"}, {"report.md", reason}})
			}
			for _, submission := range []string{"batch-terminal", "batch-terminal", "batch-replay"} {
				_, err := f.deliveryTerminal(scene, execution, submission, 1, result, "batch-terminal")
				must(t, err)
				if order == "failure" {
					_, err := f.store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
						SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.runID)
					must(t, err)
				}
				must(t, f.store.ExpireScheduledArtifactDeliveriesOnce(t.Context()))
				assertScheduledArtifactRejections(t, f, scene.runID, [][2]string{{"notes.pdf", "unsupported_type"}, {"report.md", reason}})
			}
			if f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, execution) != 1 ||
				f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1`, scene.runID) != 0 ||
				f.scalar(`SELECT count(*) FROM scheduled_run_artifact_deliveries
					WHERE issue_run_id=$1 AND state='failed' AND settled_reason=$2`, scene.runID, reason) != 1 {
				t.Fatal("batch failure must preserve one ACK, zero objects and its first settlement")
			}
			if len(f.objects.probed()) != 0 {
				t.Fatal("failed or expired batches need no object verification")
			}
		})
	}
}

func TestScheduledArtifactGRPCPartialSettlementPreservesItemFacts(t *testing.T) {
	f := setup(t)
	manifest := &controlpb.ScheduledArtifactManifest{
		Rejections: []*controlpb.ScheduledArtifactRejection{{DisplayName: "notes.pdf", Reason: controlpb.ScheduledArtifactRejectionReason_SCHEDULED_ARTIFACT_REJECTION_REASON_UNSUPPORTED_TYPE}},
	}
	for i, name := range []string{"report.md", "wrong.md", "missing.md"} {
		manifest.Items = append(manifest.Items, &controlpb.ScheduledArtifactItem{
			Index: uint32(i + 3), DisplayName: name, Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_MARKDOWN,
			SizeBytes: revisionHistorySize, Sha256: revisionHistorySHA,
		})
	}
	scene, execution := scheduledArtifactSceneForManifest(t, f, manifest)
	prefix := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/"
	result := scheduledUploadResult(scene, prefix+"3")
	wrong := proto.Clone(result.GetScheduledArtifactsDelivered().Artifacts[0]).(*controlpb.StoredObject)
	wrong.Key = prefix + "4"
	result.GetScheduledArtifactsDelivered().Artifacts = append(result.GetScheduledArtifactsDelivered().Artifacts, wrong)
	f.objects.breakObject(wrong.Key)
	for _, submission := range []string{"partial", "partial", "partial-replay"} {
		_, err := f.deliveryTerminal(scene, execution, submission, 1, result, "partial")
		must(t, err)
		assertScheduledArtifactRejections(t, f, scene.runID, [][2]string{
			{"missing.md", "upload_failed"}, {"notes.pdf", "unsupported_type"}, {"wrong.md", "verification_failed"},
		})
	}
	if f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1 AND artifact_index=3`, scene.runID) != 1 ||
		f.scalar(`SELECT count(*) FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1 AND state='partial'`, scene.runID) != 1 ||
		len(f.objects.probed()) != 2 {
		t.Fatal("settlement must preserve its verified success and original item reasons without probing on replay")
	}
}

func assertScheduledArtifactRejections(t *testing.T, f *fixture, runID string, want [][2]string) {
	t.Helper()
	rows, err := f.store.Pool.Query(`SELECT display_name, reason FROM scheduled_run_artifact_rejections
		WHERE issue_run_id=$1 ORDER BY display_name`, runID)
	must(t, err)
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var item [2]string
		must(t, rows.Scan(&item[0], &item[1]))
		got = append(got, item)
	}
	must(t, rows.Err())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rejections = %v; want collection refusals and unregistered upload failures %v", got, want)
	}
}
