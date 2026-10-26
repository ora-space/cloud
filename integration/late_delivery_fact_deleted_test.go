package integration

import (
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// Soft deletion hides business views, not the original identity retained for fact takeover.
// D1 still admits the given-up execution; D2's evidence and creation order decide registration.
func TestLateDeliveryFactSoftDeletedOriginalRunOverGRPC(t *testing.T) {
	for _, done := range []bool{false, true} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("done=%v/later=%v", done, later), func(t *testing.T) {
				f := setup(t)
				scene, req := lateDeliveryRegistered(t, f)
				lateDeliveryGiveUp(t, f, scene, done)
				if later {
					lateDeliveryEnqueueLater(t, f, scene)
				}
				_, err := f.store.Pool.Exec("UPDATE issue_runs SET deleted_at=now() WHERE id=$1", scene.runID)
				must(t, err)
				before := lateDeliveryBusinessSnapshot(t, f, scene)
				lateDeliveryNoSettlement(f)
				accepted := lateDeliveryAccepted(t, f, req)
				if later {
					if f.revisionRow(scene.runID) != nil || f.revisionVerification(req.ExecutionId) != nil {
						t.Fatal("soft deletion bypassed the later-Run registration guard")
					}
				} else if row := f.revisionRow(scene.runID); row == nil || row.RunID != scene.runID ||
					f.revisionVerification(req.ExecutionId).S("outcome") != "delivered" {
					t.Fatal("soft deletion prevented original verified Revision registration")
				}
				reads, probes := len(f.refs.queried()), len(f.objects.probed())
				for _, submission := range []string{req.SubmissionId, "soft-deleted-replay"} {
					replay := proto.Clone(req).(*controlpb.TakeOverNodeEventRequest)
					replay.SubmissionId = submission
					if response := lateDeliveryAccepted(t, f, replay); !proto.Equal(response, accepted) {
						t.Fatal("soft-deleted execution replay changed its original answer")
					}
				}
				if lateDeliveryBusinessSnapshot(t, f, scene) != before ||
					len(f.refs.queried()) != reads || len(f.objects.probed()) != probes {
					t.Fatal("soft-deleted fact intake/replay rewrote business or reread evidence")
				}
			})
		}
	}
}
