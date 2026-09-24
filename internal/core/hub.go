package core

import "sync"

// SpaceEvent is a lightweight invalidation notice broadcast after commit.
// Clients refetch authoritative state over REST; events never carry it.
type SpaceEvent struct {
	Type      string `json:"type"`
	SpaceID   string `json:"spaceId"`
	ProjectID string `json:"projectId,omitempty"`
	Version   int64  `json:"version,omitempty"`
}

// SpaceHub fans committed workspace events out to live subscribers.
// It is an in-memory, single-instance MVP facility: no persistence, no replay,
// no cross-instance delivery. Multi-instance deployments replace it with a
// broker behind the same Publish/Subscribe boundary.
type SpaceHub struct {
	mu   sync.Mutex
	subs map[string]map[chan SpaceEvent]struct{}
}

// NewSpaceHub returns an empty hub ready for subscription.
func NewSpaceHub() *SpaceHub { return &SpaceHub{subs: map[string]map[chan SpaceEvent]struct{}{}} }

// Subscribe registers a buffered stream for one workspace and returns it with
// a cancel function. Events published before subscribe are not replayed.
func (h *SpaceHub) Subscribe(spaceID string) (events <-chan SpaceEvent, cancel func()) {
	ch := make(chan SpaceEvent, 8)
	h.mu.Lock()
	if h.subs[spaceID] == nil {
		h.subs[spaceID] = map[chan SpaceEvent]struct{}{}
	}
	h.subs[spaceID][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[spaceID], ch)
			h.mu.Unlock()
		})
	}
}

// Publish delivers without blocking; a slow subscriber drops stale events and
// is refreshed by the next mutation or an explicit refetch.
func (h *SpaceHub) Publish(e SpaceEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[e.SpaceID] {
		select {
		case ch <- e:
		default:
		}
	}
}

// PublishAll fans a space-agnostic event (the plugin catalog refreshed) out to
// every live subscriber regardless of which space stream they hold, with the
// same non-blocking delivery as Publish. MVP single-instance facility; a
// multi-instance deployment replaces it with a broker behind the same
// PublishAll/Subscribe boundary.
func (h *SpaceHub) PublishAll(e SpaceEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subs {
		for ch := range subs {
			select {
			case ch <- e:
			default:
			}
		}
	}
}
