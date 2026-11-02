package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Config wires a Host. Store, Execution and Catalog are required.
type Config struct {
	Store     *store.Store
	Execution execution.Service
	Catalog   execution.Catalog
	// Hub receives run invalidations; nil creates a private one (see Host.Hub).
	Hub    *Hub
	Logger *slog.Logger
	// Now reads the clock failure details and system variables use; nil uses the process clock.
	Now func() time.Time
	// StopTimeout bounds waiting for execution to confirm a cancellation; zero means 30s.
	StopTimeout time.Duration
	// BusyRetry bounds how long an Agent start waits for the run's previous turn to release the
	// shared directory; zero means 10s.
	BusyRetry time.Duration
}

// Host executes workflow runs in this process and implements server.WorkflowRunHost. Every run
// is driven by the stateless engine; the host only keeps what cannot outlive the process: who
// started each run (execution sessions are scoped to that user), its queued Agent attempts and
// the turn currently executing.
type Host struct {
	store       *store.Store
	exec        execution.Service
	catalog     execution.Catalog
	hub         *Hub
	log         *slog.Logger
	now         func() time.Time
	stopTimeout time.Duration
	busyRetry   time.Duration
	engine      *workflow.Engine

	ctx     context.Context
	stop    context.CancelFunc
	workers sync.WaitGroup

	mu   sync.Mutex
	runs map[string]*runState
}

var _ server.WorkflowRunHost = (*Host)(nil)

// NewHost returns a ready host. Call Interrupt before serving requests and Close on shutdown.
func NewHost(c Config) (*Host, error) {
	if c.Store == nil || c.Execution == nil || c.Catalog == nil {
		return nil, errors.New("workflow host requires a store, execution and catalog")
	}
	h := &Host{
		store: c.Store, exec: c.Execution, catalog: c.Catalog, hub: c.Hub, log: c.Logger, now: c.Now,
		stopTimeout: c.StopTimeout, busyRetry: c.BusyRetry, runs: map[string]*runState{},
	}
	if h.hub == nil {
		h.hub = NewHub()
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.stopTimeout == 0 {
		h.stopTimeout = 30 * time.Second
	}
	if h.busyRetry == 0 {
		h.busyRetry = 10 * time.Second
	}
	h.ctx, h.stop = context.WithCancel(context.Background())
	h.engine = workflow.NewEngine(repository{c.Store}, h, h.hub.Publish, h.now)
	return h, nil
}

// Hub is where run invalidations are published, for the run events route.
func (h *Host) Hub() *Hub { return h.hub }

// Close stops observing executing turns and waits for the run workers. It does not cancel the
// turns themselves: the execution service ends them on its own shutdown, and the next start
// interrupts the runs.
func (h *Host) Close() {
	h.stop()
	h.workers.Wait()
}

const interruptedMessage = "the server restarted while this run was executing"

// Interrupt fails every attempt and run a previous process left executing, with desktop's
// interrupted_by_restart detail. v2 replays no Agent turn and has no interactive node to keep
// waiting, so nothing survives a restart. It must run before the host serves requests.
func (h *Host) Interrupt(ctx context.Context) error {
	f := workflow.Failure{Kind: workflow.FailureInterruptedByRestart, Message: interruptedMessage}
	now := h.now()
	ids, err := h.store.InterruptWorkflowRuns(ctx, interruptedMessage, func(attempt int, node []byte) ([]byte, error) {
		return workflow.MergePayload(node, map[string]any{"error_detail": f.Detail(attempt, true, now)})
	})
	if err != nil {
		return fmt.Errorf("interrupt workflow runs: %w", err)
	}
	if len(ids) != 0 {
		h.log.Info("interrupted workflow runs left by the previous process", "runs", len(ids))
	}
	return nil
}

// runState is one started run's process-local execution state.
type runState struct {
	scope execution.Scope

	mu         sync.Mutex
	starters   int
	cancelling bool
	queue      []workflow.AgentDispatch
	working    bool
	// starting is closed when an in-flight Start returns, so a cancellation can learn its ref.
	starting chan struct{}
	active   *execution.Ref
}

// register records who is starting a run and counts the start under way. A run that already
// started keeps its first owner; a pending run's state is dropped again when every start of it
// failed (see forgetFinished), so a later start by someone else executes under their scope.
func (h *Host) register(runID string, scope execution.Scope) *runState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.runs[runID]
	if st == nil {
		st = &runState{scope: scope}
		h.runs[runID] = st
	}
	st.mu.Lock()
	st.starters++
	st.mu.Unlock()
	return st
}

// started ends one start counted by register.
func (st *runState) started() {
	st.mu.Lock()
	st.starters--
	st.mu.Unlock()
}

func (h *Host) state(runID string) *runState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[runID]
}

func executionScope(s server.WorkflowRunScope) execution.Scope {
	return execution.Scope{TenantID: s.TenantID, UserID: s.UserID}
}

// StartRun authorizes the run against the caller's tenant, then starts it and its first wave.
func (h *Host) StartRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	run, err := h.store.WorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	if run.Status != store.WorkflowStatusPending {
		return store.WorkflowRun{}, server.ErrWorkflowRunNotStartable
	}
	st := h.register(runID, executionScope(scope))
	// The run outlives the request once it starts; a disconnecting client must not abandon the
	// first scheduling pass halfway.
	err = h.engine.Start(context.WithoutCancel(ctx), runID)
	st.started()
	// A run without Agents can finish within its first pass, and a refused start leaves the run
	// pending without an owner.
	h.forgetFinished(runID, st)
	if err != nil {
		return store.WorkflowRun{}, lifecycleError(err)
	}
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// UpdateRunInput replaces the kickoff text and Start values while the run is still pending.
func (h *Host) UpdateRunInput(ctx context.Context, scope server.WorkflowRunScope, runID string, input *string, values map[string]json.RawMessage) (store.WorkflowRun, error) {
	if _, err := h.store.WorkflowRun(ctx, scope.TenantID, runID); err != nil {
		return store.WorkflowRun{}, err
	}
	if err := h.engine.UpdateInput(ctx, runID, input, values); err != nil {
		if errors.Is(err, store.ErrWorkflowRunState) {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunNotEditable, err)
		}
		return store.WorkflowRun{}, lifecycleError(err)
	}
	h.hub.Publish(runID)
	return h.store.WorkflowRun(ctx, scope.TenantID, runID)
}

// CompleteNode is the interactive node's human completion. v2 never parks a node for input,
// so after authorizing the run it reports that the node is not awaiting input.
func (h *Host) CompleteNode(ctx context.Context, scope server.WorkflowRunScope, runID, _ string, _ bool, _ string) (store.WorkflowRun, error) {
	if _, err := h.store.WorkflowRun(ctx, scope.TenantID, runID); err != nil {
		return store.WorkflowRun{}, err
	}
	return store.WorkflowRun{}, server.ErrWorkflowNodeNotAwaitingInput
}

// CancelRun stops the run's executing Agent turn and waits for execution to confirm it before
// closing the run as cancelled. Without confirmation the run stays open and the caller learns
// the stop is unconfirmed; the run still never advances, and its worker closes it as cancelled
// once the turn ends.
func (h *Host) CancelRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	run, err := h.store.WorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	st := h.state(runID)
	if st != nil && (run.Status == store.WorkflowStatusPending || run.Status == store.WorkflowStatusRunning) {
		stopCtx, cancel := context.WithTimeout(ctx, h.stopTimeout)
		defer cancel()
		ref, err := st.requestCancel(stopCtx)
		if err != nil {
			return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
		}
		if ref != nil {
			if _, err := h.exec.Cancel(stopCtx, st.scope, *ref); err != nil {
				return store.WorkflowRun{}, fmt.Errorf("%w: %w", server.ErrWorkflowRunStopUnconfirmed, err)
			}
		}
	}
	run, err = h.store.CancelWorkflowRun(ctx, scope.TenantID, runID)
	if err != nil {
		return store.WorkflowRun{}, err
	}
	h.hub.Publish(runID)
	if st != nil {
		h.forgetFinished(runID, st)
	}
	return run, nil
}

// requestCancel marks the run cancelling, so no further Agent starts, and returns the turn that
// is executing. A Start already in flight is awaited, since its turn exists once it returns.
func (st *runState) requestCancel(ctx context.Context) (*execution.Ref, error) {
	st.mu.Lock()
	st.cancelling = true
	for st.starting != nil {
		starting := st.starting
		st.mu.Unlock()
		select {
		case <-starting:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		st.mu.Lock()
	}
	defer st.mu.Unlock()
	if st.active == nil {
		return nil, nil
	}
	ref := *st.active
	return &ref, nil
}

// lifecycleError translates the engine's refusals into the HTTP layer's lifecycle errors.
func lifecycleError(err error) error {
	var start *workflow.StartError
	var rejection *workflow.InputRejection
	switch {
	case errors.As(err, &rejection):
		return &server.WorkflowRunInputInvalid{Variable: rejection.Variable, Reason: rejection.Reason}
	case errors.As(err, &start) && start.Variable != "":
		return &server.WorkflowRunInputInvalid{Variable: start.Variable, Reason: start.Detail}
	case errors.As(err, &start), errors.Is(err, workflow.ErrNotStartable):
		return fmt.Errorf("%w: %w", server.ErrWorkflowRunNotStartable, err)
	}
	return err
}
