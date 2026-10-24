package core

import "sync"

// SpaceEvent is a lightweight invalidation notice broadcast after commit.
// Clients refetch authoritative state over REST; events never carry it.
//
// IssueID/RunID/LastSeq belong to the one event that addresses a single resource rather than a
// whole space — `issue_run.thread_appended` (Thread D5). They are additive and
// omitted when empty, so every event that predates them serializes to the same bytes it did before
// (`project.created`'s ProjectID is the precedent). An event still never carries business state:
// LastSeq says only "there is data at or below this seq", never which entries exist, and it never
// advances a client's cursor — only the Thread GET does that.
type SpaceEvent struct {
	Type            string `json:"type"`
	SpaceID         string `json:"spaceId"`
	ProjectID       string `json:"projectId,omitempty"`
	Version         int64  `json:"version,omitempty"`
	IssueID         string `json:"issueId,omitempty"`
	RunID           string `json:"runId,omitempty"`
	LastSeq         int64  `json:"lastSeq,omitempty"`
	ScheduledTaskID string `json:"scheduledTaskId,omitempty"`
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
// is refreshed by the next mutation or an explicit refetch. The notice is taken
// by value on purpose: `ch <- e` copies it into each subscriber's buffer, so a
// publisher that reuses its own SpaceEvent can never race with a reader, while a
// pointer would alias one struct across every subscriber channel.
//
//nolint:gocritic // by value on purpose, see above: each subscriber gets its own copy.
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
//
//nolint:gocritic // by value for the same reason as Publish: every subscriber gets its own copy.
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
