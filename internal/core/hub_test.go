package core

import (
	"encoding/json"
	"testing"
)

// TestSpaceHubPublishAllAndSubscriptionLifecycle covers the hub additions the
// plugin feature relies on: space-agnostic broadcast, non-blocking delivery,
// and safe unsubscribe semantics.
func TestSpaceHubPublishAllAndSubscriptionLifecycle(t *testing.T) {
	h := NewSpaceHub()
	a, cancelA := h.Subscribe("space-a")
	b, cancelB := h.Subscribe("space-b")
	defer cancelA()
	defer cancelB()

	// A catalog refresh reaches every subscribed space, regardless of id.
	h.PublishAll(SpaceEvent{Type: "plugins.catalog_updated"})
	if got := (<-a).Type; got != "plugins.catalog_updated" {
		t.Fatalf("space-a event = %q", got)
	}
	if got := (<-b).Type; got != "plugins.catalog_updated" {
		t.Fatalf("space-b event = %q", got)
	}

	// Space-scoped events only reach their own subscribers.
	h.Publish(SpaceEvent{Type: "space.plugins_updated", SpaceID: "space-a"})
	if got := (<-a).Type; got != "space.plugins_updated" {
		t.Fatalf("space-a event = %q", got)
	}
	select {
	case ev := <-b:
		t.Fatalf("space-b received a space-scoped event of space-a: %+v", ev)
	default:
	}

	// A slow subscriber whose buffer is full is skipped, never blocked: fill
	// the buffer without reading, then publish repeatedly.
	blocked, cancelBlocked := h.Subscribe("space-b")
	defer cancelBlocked()
	for i := 0; i < 8; i++ {
		h.PublishAll(SpaceEvent{Type: "plugins.catalog_updated"})
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			h.PublishAll(SpaceEvent{Type: "plugins.catalog_updated"})
		}
		close(done)
	}()
	<-done
	_ = blocked

	// Cancellation removes the subscriber; a repeated cancel is safe. Use a
	// fresh subscription so earlier buffered events cannot confuse the assert.
	fresh, cancelFresh := h.Subscribe("space-b")
	cancelFresh()
	cancelFresh()
	h.PublishAll(SpaceEvent{Type: "plugins.catalog_updated"})
	select {
	case <-fresh:
		t.Fatal("canceled subscriber still received events")
	default:
	}
}

// TestSpaceEventSerialization pins the SSE wire shape: the two new event types
// carry exactly the invalidation fields, and the catalog event has no spaceId.
func TestSpaceEventSerialization(t *testing.T) {
	withSpace, e := json.Marshal(SpaceEvent{Type: "space.plugins_updated", SpaceID: "s", Version: 3})
	if e != nil {
		t.Fatal(e)
	}
	if string(withSpace) != `{"type":"space.plugins_updated","spaceId":"s","version":3}` {
		t.Fatalf("space event JSON = %s", withSpace)
	}
	catalog, e := json.Marshal(SpaceEvent{Type: "plugins.catalog_updated"})
	if e != nil {
		t.Fatal(e)
	}
	if string(catalog) != `{"type":"plugins.catalog_updated","spaceId":""}` {
		t.Fatalf("catalog event JSON = %s", catalog)
	}
}

// TestPluginAggregateRuleMatrix pins the fan-out aggregation table: failure
// wins, in-progress states keep the direction's progress state, all-terminal
// (or no targets) means terminal.
func TestPluginAggregateRuleMatrix(t *testing.T) {
	cases := []struct {
		name     string
		desired  string
		states   []string
		expected string
	}{
		{"no live targets means installed", "installed", nil, "installed"},
		{"no live targets means removed", "removed", nil, "removed"},
		{"all installed", "installed", []string{"installed", "installed"}, "installed"},
		{"one pending keeps installing", "installed", []string{"installed", "pending"}, "installing"},
		{"one installing keeps installing", "installed", []string{"installed", "installing"}, "installing"},
		{"any failure wins", "installed", []string{"installed", "failed"}, "failed"},
		{"all removed", "removed", []string{"removed"}, "removed"},
		{"one removing keeps removing", "removed", []string{"removed", "removing"}, "removing"},
		{"removal failure wins", "removed", []string{"removed", "failed"}, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pluginAggregate(tc.desired, tc.states); got != tc.expected {
				t.Fatalf("pluginAggregate(%s, %v) = %q, want %q", tc.desired, tc.states, got, tc.expected)
			}
		})
	}
}
