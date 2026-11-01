package workflowhost

import "testing"

func TestHubCoalescesAndReleases(t *testing.T) {
	h := NewHub()
	events, cancel := h.Subscribe("run-1")
	other, cancelOther := h.Subscribe("run-2")
	defer cancelOther()
	h.Publish("run-1")
	h.Publish("run-1") // coalesced into the unconsumed invalidation
	<-events
	select {
	case <-events:
		t.Fatal("two invalidations delivered for one unconsumed change")
	case <-other:
		t.Fatal("another run's subscriber was notified")
	default:
	}
	cancel()
	cancel() // idempotent
	h.Publish("run-1")
	select {
	case <-events:
		t.Fatal("a released subscription was notified")
	default:
	}
	if _, ok := h.subs["run-1"]; ok {
		t.Fatal("released run kept its subscriber set")
	}
}
