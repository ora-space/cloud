package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

type lateDeliveryReply struct {
	response *controlpb.TakeOverNodeEventResponse
	err      error
}

// lateDeliveryBlockedRead uses the actual object-store HTTP request as the barrier. No database
// transaction or product hook is paused: D5/Run creation can genuinely commit while evidence is
// being read, and only the final production transaction decides whether registration is allowed.
func lateDeliveryBlockedRead(t *testing.T, f *fixture, req *controlpb.TakeOverNodeEventRequest) (<-chan struct{}, func(), <-chan lateDeliveryReply) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			f.objects.head(w, r)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	cfg := *f.objects.config
	cfg.Endpoint = server.URL
	f.store.ObjectStore = &cfg
	finished := make(chan lateDeliveryReply, 1)
	ctx, cancel := context.WithTimeout(asController("ctrl-a"), 20*time.Second)
	t.Cleanup(cancel)
	go func() {
		response, err := f.executions.TakeOverNodeEvent(ctx, req)
		finished <- lateDeliveryReply{response: response, err: err}
	}()
	return entered, unblock, finished
}

func lateDeliveryWaitForRead(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("takeover never reached the external-read barrier")
	}
}

func lateDeliveryWaitReply(t *testing.T, finished <-chan lateDeliveryReply) lateDeliveryReply {
	t.Helper()
	select {
	case reply := <-finished:
		return reply
	case <-time.After(20 * time.Second):
		t.Fatal("takeover did not terminate after the evidence barrier was released")
		return lateDeliveryReply{}
	}
}

func TestLateDeliveryFactReclassifiesD5DuringExternalReadOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	entered, release, finished := lateDeliveryBlockedRead(t, f, req)
	lateDeliveryWaitForRead(t, entered)
	lateDeliveryGiveUp(t, f, scene, false)
	lateDeliveryNoSettlement(f)
	before := lateDeliveryBusinessSnapshot(t, f, scene)
	release()
	reply := lateDeliveryWaitReply(t, finished)
	must(t, reply.err)
	if reply.response.GetRecord().GetExecutionId() != req.ExecutionId || f.revisionRow(scene.runID) == nil ||
		f.receiptEvent(req.ExecutionId, 1) != string(req.Event) || lateDeliveryBusinessSnapshot(t, f, scene) != before {
		t.Fatal("final transaction failed to reclassify D5 or resettled business after the external read")
	}
}

func TestLateDeliveryFactLaterRunCommitsDuringExternalReadOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, false)
	entered, release, finished := lateDeliveryBlockedRead(t, f, req)
	lateDeliveryWaitForRead(t, entered)
	lateDeliveryEnqueueLater(t, f, scene)
	before := lateDeliveryBusinessSnapshot(t, f, scene)
	release()
	reply := lateDeliveryWaitReply(t, finished)
	must(t, reply.err)
	if reply.response.GetRecord().GetExecutionId() != req.ExecutionId || f.revisionRow(scene.runID) != nil ||
		f.revisionVerification(req.ExecutionId) != nil || f.receiptEvent(req.ExecutionId, 1) != string(req.Event) ||
		lateDeliveryBusinessSnapshot(t, f, scene) != before {
		t.Fatal("final transaction ignored the later Run committed during evidence I/O")
	}
}

func TestLateDeliveryFactLeaseExpiresDuringExternalReadOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, false)
	entered, release, finished := lateDeliveryBlockedRead(t, f, req)
	lateDeliveryWaitForRead(t, entered)
	_, err := f.store.Pool.Exec("UPDATE controller_leases SET epoch=2,expires_at=clock_timestamp()-interval '1 second' WHERE name='global'")
	must(t, err)
	release()
	reply := lateDeliveryWaitReply(t, finished)
	if reply.response != nil {
		t.Fatal("stale Controller received a successful terminal response")
	}
	expectStatus(t, reply.err, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
	lateDeliveryNoFact(t, f, req)
	_, err = f.store.Pool.Exec("UPDATE controller_leases SET epoch=1,expires_at=clock_timestamp()+interval '30 minutes' WHERE name='global'")
	must(t, err)
	// Restored authority submits the original event; the stale writer stored nothing.
	lateDeliveryAccepted(t, f, req)
}

func TestLateDeliveryFactExternalFailureDoesNotAcknowledgeOverGRPC(t *testing.T) {
	for _, unavailable := range []bool{true, false} {
		name := "negative-evidence"
		if unavailable {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryRegistered(t, f)
			lateDeliveryGiveUp(t, f, scene, false)
			lateDeliveryNoSettlement(f)
			before := lateDeliveryBusinessSnapshot(t, f, scene)
			if unavailable {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "injected unavailable storage", http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				cfg := *f.objects.config
				cfg.Endpoint = server.URL
				f.store.ObjectStore = &cfg
				response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
				if response != nil {
					t.Fatal("unavailable evidence returned an ACKable response")
				}
				expectStatus(t, err, codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNAVAILABLE)
				lateDeliveryNoFact(t, f, req)
				f.store.ObjectStore = f.objects.config
			} else {
				f.objects.breakObject(f.deliveryInputOf(req.ExecutionId).S("historyKey"))
			}
			lateDeliveryAccepted(t, f, req)
			if unavailable {
				if f.revisionRow(scene.runID) == nil {
					t.Fatal("recovered external evidence failed to register the original Revision")
				}
			} else if f.revisionRow(scene.runID) != nil || f.revisionVerification(req.ExecutionId).S("outcome") != "failed" {
				t.Fatal("invalid evidence was promoted from a Node declaration to a verified Revision")
			}
			if lateDeliveryBusinessSnapshot(t, f, scene) != before {
				t.Fatal("evidence failure changed the historical business outcome")
			}
		})
	}
}
