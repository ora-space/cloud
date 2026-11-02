package workflowhost

import (
	"context"
	"fmt"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// CleanupLoopExit fences new starts in this scope before asynchronously draining its active
// turn. It never waits under the engine gate: that turn may need the gate for its late callback.
func (h *Host) CleanupLoopExit(runID, parentID, scopeID string) {
	st := h.state(runID)
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.closedScopes == nil {
		st.closedScopes = map[string]bool{}
	}
	if st.closedScopes[scopeID] {
		st.mu.Unlock()
		return
	}
	st.closedScopes[scopeID] = true
	st.mu.Unlock()
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		ctx, cancel := context.WithTimeout(h.ctx, h.stopTimeout)
		defer cancel()
		err := h.drainLoopScope(ctx, st, scopeID)
		if h.ctx.Err() != nil {
			return
		} // startup interrupts; it never replays an exit
		if err != nil {
			err = fmt.Errorf("LoopExit could not confirm session cleanup: %s", publicReason(err))
		}
		if finishErr := h.engine.FinishLoopExit(h.ctx, runID, parentID, scopeID, err); finishErr != nil {
			h.log.Error("settle workflow LoopExit", "run", runID, "scope", scopeID, "error", finishErr)
		}
		h.forgetFinished(runID, st)
	}()
}

func (h *Host) drainLoopScope(ctx context.Context, st *runState, scopeID string) error {
	st.mu.Lock()
	for st.activeScope == scopeID && st.starting != nil {
		started := st.starting
		st.mu.Unlock()
		select {
		case <-started:
		case <-ctx.Done():
			return ctx.Err()
		}
		st.mu.Lock()
	}
	var ref *execution.Ref
	var done <-chan struct{}
	if st.activeScope == scopeID && st.active != nil {
		copy := *st.active
		ref = &copy
		done = st.turnDone
	}
	st.mu.Unlock()
	if ref == nil {
		return st.scopeCleanupError(scopeID)
	}
	if _, err := h.exec.Cancel(ctx, st.scope, *ref); err != nil {
		return err
	}
	// The worker owns End and clears its slot only after that call. Merely cancelling the turn
	// is not enough to advance while its session/workspace cleanup is still under way.
	select {
	case <-done:
		return st.scopeCleanupError(scopeID)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (st *runState) scopeCleanupError(scopeID string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.scopeCleanupErrors[scopeID]
}
