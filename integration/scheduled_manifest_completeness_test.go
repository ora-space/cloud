package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

func TestScheduledManifestCompletenessSurvivesTakeoverAndRegistration(t *testing.T) {
	f := setup(t)
	scene, execution := scheduledArtifactSceneForManifest(t, f, &controlpb.ScheduledArtifactManifest{
		Items:             []*controlpb.ScheduledArtifactItem{{Index: 3, DisplayName: "report.md", Kind: controlpb.ScheduledArtifactKind_SCHEDULED_ARTIFACT_KIND_MARKDOWN, SizeBytes: revisionHistorySize, Sha256: revisionHistorySHA}},
		Rejections:        []*controlpb.ScheduledArtifactRejection{{DisplayName: ".ora-scheduled-artifacts", Reason: controlpb.ScheduledArtifactRejectionReason_SCHEDULED_ARTIFACT_REJECTION_REASON_CANDIDATE_LIMIT}},
		OmittedRejections: 448, WalkTruncated: true,
	})
	key := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/3"
	_, err := f.deliveryTerminal(scene, execution, "complete-upload", 1, scheduledUploadResult(scene, key), "complete-upload")
	must(t, err)
	if f.scalar(`SELECT count(*) FROM scheduled_run_artifact_deliveries
		WHERE issue_run_id=$1 AND omitted_rejections=448 AND walk_truncated`, scene.runID) != 1 ||
		f.scalar(`SELECT count(*) FROM scheduled_run_artifact_rejections WHERE issue_run_id=$1 AND reason='candidate_limit'`, scene.runID) != 1 {
		t.Fatal("manifest completeness must remain queryable after delivery settlement")
	}
}
