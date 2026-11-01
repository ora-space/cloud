package workflowhost

import "sync"

// Hub fans out run invalidations inside this process. An event carries no data: it only tells a
// reader that the run changed and must be read again, so a slow reader loses nothing by having
// several events coalesced into one. The run events route subscribes here; the engine publishes
// after every committed transition.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscription]struct{}
}

type subscription struct{ ch chan struct{} }

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[string]map[*subscription]struct{}{}} }

// Subscribe returns a channel that receives a value after each change of the run, coalescing
// changes the reader has not consumed yet, and a cancel function that releases the subscription.
// The channel is never closed, so a reader selects on it together with its own context.
func (h *Hub) Subscribe(runID string) (<-chan struct{}, func()) {
	sub := &subscription{ch: make(chan struct{}, 1)}
	h.mu.Lock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[*subscription]struct{}{}
	}
	h.subs[runID][sub] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[runID], sub)
			if len(h.subs[runID]) == 0 {
				delete(h.subs, runID)
			}
			h.mu.Unlock()
		})
	}
}

// Publish notifies every current subscriber of the run without blocking.
func (h *Hub) Publish(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[runID] {
		select {
		case sub.ch <- struct{}{}:
		default: // an unconsumed invalidation already covers this change
		}
	}
}
