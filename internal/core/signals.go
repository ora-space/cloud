package core

import "sync"

// ControlSignal is an at-most-once hint to the lease-holding Controller. Signals carry no state and
// change no ownership: WorkAvailable only says a claim is worth trying, Drain only asks the holder
// to stop claiming. Anything durable is fetched through the control RPCs.
type ControlSignal struct {
	Kind        ControlSignalKind
	OperationID string
	NodeID      string
}

// ControlSignalKind enumerates the signals the Watch stream can carry.
type ControlSignalKind string

const (
	// SignalWorkAvailable follows a committed clone request; OperationID names it.
	SignalWorkAvailable ControlSignalKind = "work_available"
	// SignalOperationAvailable follows a committed transaction that queued a runtime Workspace
	// operation (created or retried); OperationID names it. A retry_wait operation becoming due has no
	// commit to follow, so the holder's periodic claim covers it.
	SignalOperationAvailable ControlSignalKind = "operation_available"
	// SignalDrain precedes this instance's shutdown: the holder stops claiming from it until a new
	// Watch is established, keeps its lease, and leaves in-flight coordination and user work alone.
	SignalDrain ControlSignalKind = "drain"
)

// ControlHub fans control signals out to live Watch streams. Like SpaceHub it is an in-memory,
// single-instance facility: no persistence, no replay, no cross-instance delivery. A slow
// subscriber loses signals rather than blocking the publisher; the periodic claim covers the loss.
type ControlHub struct {
	mu       sync.Mutex
	subs     map[chan ControlSignal]struct{}
	draining bool
}

// NewControlHub returns an empty hub ready for subscription.
func NewControlHub() *ControlHub { return &ControlHub{subs: map[chan ControlSignal]struct{}{}} }

// Subscribe registers one bounded stream; ok is false once the hub is draining, so a Watch opened
// during shutdown ends immediately instead of waiting for a Drain it would never see.
func (h *ControlHub) Subscribe() (signals <-chan ControlSignal, cancel func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return nil, func() {}, false
	}
	ch := make(chan ControlSignal, 16)
	h.subs[ch] = struct{}{}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		})
	}, true
}

// Publish delivers without blocking; a full subscriber drops the signal.
func (h *ControlHub) Publish(s ControlSignal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- s:
		default:
		}
	}
}

// Drain sends the final Drain signal to every subscriber and closes their streams; later
// subscriptions are refused. It is idempotent so shutdown paths may call it more than once.
func (h *ControlHub) Drain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return
	}
	h.draining = true
	for ch := range h.subs {
		select {
		case ch <- ControlSignal{Kind: SignalDrain}:
		default:
		}
		close(ch)
		delete(h.subs, ch)
	}
}
