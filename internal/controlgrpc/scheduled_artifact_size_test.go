package controlgrpc

import (
	"math"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

func TestScheduledArtifactResultDistinguishesEmptyFromOverflow(t *testing.T) {
	object := &controlpb.StoredObject{Key: "artifact", Size: math.MaxUint64, Sha256: strings.Repeat("a", 64)}
	message := &controlpb.ExecutionResult{
		Node:    &controlpb.NodeIdentity{NodeId: "node", NodeIncarnationId: "incarnation"},
		Outcome: &controlpb.ExecutionResult_ScheduledArtifactsDelivered{ScheduledArtifactsDelivered: &controlpb.ScheduledArtifactsDelivered{Artifacts: []*controlpb.StoredObject{object}}},
	}
	if _, err := resultObject(message); err == nil {
		t.Fatal("an overflowing wire size must not become a legitimate zero-byte object")
	}
	object.Size = 0
	got, err := resultObject(message)
	if err != nil || rows(got["artifacts"])[0].N("size") != 0 {
		t.Fatalf("a real zero-byte size must survive: %v, %v", got, err)
	}
}
