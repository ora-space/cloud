package controlgrpc

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

func TestScheduledManifestCompletenessWireRoundTrip(t *testing.T) {
	want := &controlpb.ScheduledArtifactManifest{
		Rejections:        []*controlpb.ScheduledArtifactRejection{{DisplayName: ".ora-scheduled-artifacts", Reason: controlpb.ScheduledArtifactRejectionReason_SCHEDULED_ARTIFACT_REJECTION_REASON_CANDIDATE_LIMIT}},
		OmittedRejections: 448, WalkTruncated: true,
	}
	encoded, err := proto.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got := &controlpb.ScheduledArtifactManifest{}
	if err := proto.Unmarshal(encoded, got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("manifest completeness lost on wire: got %v, want %v", got, want)
	}
}
