package integration

import (
	"context"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// The production give-up scanner declares release; no SQL stages business settlement or hides the
// queued delivery. Substrate/Node are fixtures, while ClaimWork uses real gRPC and PostgreSQL.
func TestReleasedDeliveryIsNotOfferedAgain(t *testing.T) {
	f := setup(t)
	scene := deliveringScene(t, f)
	f.store.DeliveryGiveUpAfter = time.Nanosecond
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if phase := f.runPhase(scene.runID); phase != "releasing" {
		t.Fatalf("precondition: delivery give-up must have declared deletion, phase=%s", phase)
	}
	for attempt := range 3 {
		picked, err := f.executions.ClaimWork(asController("ctrl-a"), &controlpb.ClaimWorkRequest{Epoch: 1})
		must(t, err)
		if picked.GetItem() != nil {
			t.Errorf("claim %d: abandoned delivery is still offered after the Run entered releasing", attempt+1)
		}
	}
}
