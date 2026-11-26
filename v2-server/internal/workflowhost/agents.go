package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Dispatch starts one Agent attempt in its own goroutine and returns at once. A run's attempts
// execute concurrently, as desktop's: each is its own execution session in the run's shared
// directory, and Node admits several live sessions on one checkout.
func (h *Host) Dispatch(d workflow.AgentDispatch) {
	if d.Node.Kind == "form" || d.Node.Kind == "createTasks" {
		h.workers.Add(1)
		go func() { defer h.workers.Done(); h.dispatchReview(d) }()
		return
	}
	st := h.state(d.RunID)
	if st == nil {
		// Only StartRun admits runs, and it registers the owner first; reaching here is a bug,
		// which still must not leave the attempt running forever. The engine holds the run's
		// gate while dispatching, so the failure is recorded after Dispatch returns.
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "the run has no execution owner in this process"})
		}()
		return
	}
	st.mu.Lock()
	generation := st.generation
	st.dispatched++
	st.mu.Unlock()
	h.workers.Add(1)
	go func() {
		defer h.settled(d.RunID, st)
		if h.dispatching != nil {
			h.dispatching(d)
		}
		st.mu.Lock()
		dropped := st.cancelling || generation != st.generation || h.ctx.Err() != nil
		st.mu.Unlock()
		// An attempt dispatched before a cancellation request, or one a resume replaced, is
		// dropped: the run closes as cancelled together with its row, or the resume soft-deleted
		// it. One whose run ended meanwhile (a sibling failed it) is closed without a turn.
		if dropped || h.abandoned(d) {
			return
		}
		h.runAgent(st, d, generation)
	}()
}

// settled ends one goroutine counted in runState.dispatched.
func (h *Host) settled(runID string, st *runState) {
	st.mu.Lock()
	st.dispatched--
	st.mu.Unlock()
	h.forgetFinished(runID, st)
	h.workers.Done()
}

// forgetFinished drops the state of a run that is not running while nothing of it is in flight,
// so the process does not keep every run it ever executed. A running run keeps its owner for the
// next wave. A run still pending once no start is under way was refused (bad input, an unsupported
// node): forgetting its starter lets the next start record its own owner instead of executing under
// the scope of whoever tried first.
func (h *Host) forgetFinished(runID string, st *runState) {
	c, err := h.store.WorkflowEngineContext(h.ctx, runID)
	if err != nil || c.Run.Status == workflow.StatusRunning {
		return
	}
	h.releaseOrphans(st, runID)
	h.mu.Lock()
	defer h.mu.Unlock()
	st.mu.Lock()
	idle := st.dispatched == 0 && st.starters == 0 && len(st.attempts) == 0 && len(st.interactive) == 0 && len(st.unended) == 0
	st.mu.Unlock()
	if idle && h.runs[runID] == st {
		delete(h.runs, runID)
	}
}

// abandoned asks the engine whether the attempt's run ended while it was queued. If the check
// itself fails, the attempt still executes: its outcome is then recorded as a late callback, the
// behavior before this check existed.
func (h *Host) abandoned(d workflow.AgentDispatch) bool {
	abandoned, err := h.engine.Abandon(h.ctx, d.RunID, d.NodeRunID)
	if err != nil {
		h.log.Warn("check workflow run before starting an Agent", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
	return abandoned
}

func (h *Host) resolvePreviousFailure(tenant string, d workflow.AgentDispatch) string {
	run, err := h.store.WorkflowRun(h.ctx, tenant, d.RunID)
	if err != nil {
		if h.ctx.Err() == nil {
			h.log.Warn("read workflow run for previous failure injection", "run", d.RunID, "error", err)
		}
		return ""
	}
	inject := true
	if rp, ok, err := workflow.DecodeRunPayload(run.RuntimePayload); err != nil {
		h.log.Warn("decode workflow run payload for previous failure injection", "run", d.RunID, "error", err)
		return ""
	} else if ok {
		inject = rp.InjectLastFailure
	}
	if !inject {
		return ""
	}
	prevRow, err := h.store.FindLastFailedWorkflowNodeAttempt(h.ctx, tenant, d.RunID, d.Node.ID, d.Iteration)
	if err != nil {
		if h.ctx.Err() == nil {
			h.log.Warn("find previous failed workflow node attempt", "run", d.RunID, "node", d.Node.ID, "error", err)
		}
		return ""
	}
	prev := previousFailureForInjection(inject, prevRow)
	if prev == nil {
		return ""
	}
	return renderPreviousFailure(*prev, d.Locale)
}

func (h *Host) recordInjectedFailure(tenant, runID, nodeRunID, text string) {
	if _, err := h.store.MergeWorkflowNodePayload(h.ctx, tenant, runID, nodeRunID, func(existing []byte) ([]byte, error) {
		return workflow.MergePayload(existing, map[string]any{"injected_failure_context": text})
	}); err != nil && h.ctx.Err() == nil {
		h.log.Warn("record workflow node injected failure", "run", runID, "node_run", nodeRunID, "error", err)
	}
}

func (h *Host) fail(d workflow.AgentDispatch, f workflow.Failure) {
	if err := h.resultRetry.Store(h.ctx, func(ctx context.Context) error {
		return h.engine.FailNode(ctx, d.RunID, d.NodeRunID, f)
	}); err != nil && h.ctx.Err() == nil {
		h.log.Error("record workflow node failure", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

func (h *Host) complete(d workflow.AgentDispatch, c workflow.Completion) {
	reschedule := false
	if err := h.resultRetry.Store(h.ctx, func(ctx context.Context) error {
		if err := h.engine.CompleteNode(ctx, d.RunID, d.NodeRunID, c); err != nil {
			reschedule = true
			return err
		}
		// CompleteNode may have stored the result before a scheduling read failed.
		// Its duplicate callback is a no-op, so explicitly resume that scheduling pass.
		if reschedule {
			return h.engine.Resume(ctx, d.RunID)
		}
		return nil
	}); err != nil && h.ctx.Err() == nil {
		h.log.Error("record workflow node completion", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

// finishCancel closes a run whose cancellation was requested once its turn is known to have
// ended, covering a cancellation whose stop the execution service did not confirm in time.
func (h *Host) finishCancel(st *runState, runID string) {
	_, err := h.store.CancelWorkflowRun(h.ctx, st.scope.TenantID, runID)
	if err == nil || errors.Is(err, store.ErrWorkflowRunState) {
		h.engine.ReleaseRetries(runID) // the run is closed; its waits are settled
	}
	if err != nil {
		if h.ctx.Err() == nil { // at shutdown the next start interrupts the run instead
			h.log.Error("close cancelled workflow run", "run", runID, "error", err)
		}
		return
	}
	h.hub.Publish(runID)
}

// runAgent drives one Agent attempt as desktop's drive_agent_node: resolve the executor, read the
// role definition, render the prompt, start the session, bind it to the attempt, wait for the
// owning turn's result, end the session and report the outcome to the engine.
func (h *Host) runAgent(st *runState, d workflow.AgentDispatch, generation uint64) {
	cfg := d.Node.Agent()
	// Interactive mode applies to root-scope nodes only. Desktop rejects it inside Iterations (graph
	// validation already does here) but parks Loop members too; v2 does not, so a Loop member runs
	// as an automatic node, and the workflow package's unapplied rule lists it for that member.
	interactive := cfg.Interactive && d.ScopeID == "" && d.Iteration == nil
	fail := func(f workflow.Failure) {
		if interactive {
			f = f.Interactive() // a person drives the node: it never fails as auto-retryable
		}
		h.fail(d, f)
	}
	skills, err := h.store.ResolveWorkflowSkills(h.ctx, st.scope, d.ProjectID, cfg.Skills)
	if err != nil {
		if errors.Is(err, store.ErrSkillNotFound) {
			fail(workflow.Failure{Kind: workflow.FailureMissingAgentConfig, Message: err.Error()})
			return
		}
		if h.ctx.Err() == nil {
			h.log.Warn("resolve workflow skills", "run", d.RunID, "node_run", d.NodeRunID, "error", publicReason(err), "error_type", fmt.Sprintf("%T", err))
		}
		fail(workflow.Failure{Kind: workflow.FailureSession, Message: "could not resolve selected Skills"})
		return
	}
	mcp, err := h.store.ResolveWorkflowMCP(h.ctx, st.scope, d.ProjectID, cfg.MCPs)
	if err != nil {
		var setup *execution.MCPFailure
		if h.ctx.Err() == nil && !errors.As(err, &setup) {
			reason := publicReason(err)
			if errors.Is(err, pluginmarket.ErrCredentials) {
				reason = pluginmarket.ErrCredentials.Error()
			}
			h.log.Warn("resolve workflow MCP", "run", d.RunID, "node_run", d.NodeRunID, "error", reason, "error_type", fmt.Sprintf("%T", err))
		}
		fail(mcpFailure(err))
		return
	}
	// The role is read again here, live, as desktop does right before the session starts: the
	// catalog may have changed since the run started, and the name is matched case-insensitively
	// with an id fallback. A role deleted after the start refuses this node with a resumable
	// failure, so creating it again lets the run continue from here.
	roleContent := ""
	if cfg.RoleID != "" {
		role, err := h.store.ResolveWorkflowRole(h.ctx, st.scope.TenantID, cfg.RoleID)
		switch {
		case errors.Is(err, store.ErrRoleNotFound):
			fail(workflow.Failure{Kind: workflow.FailureWorkflowRoleNotFound, Message: fmt.Sprintf("role %q is not available in this workspace", cfg.RoleID)})
			return
		case err != nil:
			fail(workflow.Failure{Kind: workflow.FailureMissingAgentConfig, Message: err.Error()})
			return
		}
		roleContent = role.Content
	}
	executor, failure := h.resolveExecutor(st.scope, cfg)
	if failure != nil {
		fail(*failure)
		return
	}
	task, err := workflow.RenderTemplate(cfg.Prompt, &d.Pool)
	if err != nil {
		fail(workflow.Failure{Kind: workflow.FailurePromptTemplate, Message: err.Error()})
		return
	}
	injectedFailure := h.resolvePreviousFailure(st.scope.TenantID, d)
	caller := execution.Caller{Kind: execution.CallerWorkflow, ID: d.RunID, NodeID: d.Node.ID, AttemptID: d.NodeRunID}
	request := execution.StartRequest{
		MCP:    mcp,
		Skills: skills, Caller: caller, Target: execution.Target{ProjectID: d.ProjectID},
		ExecutorID: executor.executor.ID, Prompt: agentPrompt(d, task, roleContent, injectedFailure),
	}
	if interactive {
		// The session outlives its turns until completion, a stop or the run's end ends it.
		request.Lifetime = execution.LifetimeHeld
	}
	// The first prompt reaches the Agent only once the node is bound to its session: an Agent
	// reporting through the completion tool right away must find its node bound. Every path after
	// a started session lets it through (a value) or abandons it (closing the channel), which
	// ends the held-back turn at once, also when the session cannot be ended then.
	release := make(chan struct{}, 1)
	request.Release = release
	// Only a root-scope interactive attempt whose frozen policy is agent may report completion
	// (completion signal ADR D1); no other session even sees the tool.
	policy, tracked := workflow.CompletionSignalEndTurn, false
	if interactive && cfg.CompletionSignal == workflow.CompletionSignalAgent {
		if h.tools == nil {
			fail(workflow.Failure{Kind: workflow.FailureInteractiveSession, Message: server.ErrWorkflowCompletionToolUnavailable.Error()})
			return
		}
		policy = workflow.CompletionSignalAgent
		request.Host = h.tools.Attempt(d.NodeRunID, caller.AttemptID)
		// Until the node is tracked nothing else withdraws what its Agent may have been given.
		defer func() {
			if !tracked {
				h.tools.Revoke(d.NodeRunID)
			}
		}()
	}
	var baseline *fileBaseline
	if interactive {
		// Before the Agent can touch the checkout: what a mustChange deliverable is compared with.
		baseline = h.captureBaseline(h.ctx, st.scope.TenantID, d.RunID, cfg.CompletionFiles)
	}
	a := h.beginSession(st, d, generation)
	if a == nil {
		return
	}
	// Resolving the executor and rendering the prompt took a while after the attempt's abandon
	// check: the run may have ended (a sibling failed it) or been cancelled meanwhile. Check again
	// with the attempt registered, so nothing can end the run between this check and the start
	// without waiting for the start to return.
	if h.abandoned(d) {
		if st.releaseStart(a) {
			h.finishCancel(st, d.RunID)
		}
		return
	}
	if injectedFailure != "" {
		h.recordInjectedFailure(st.scope.TenantID, d.RunID, d.NodeRunID, injectedFailure)
	}
	ref, err := h.exec.Start(h.ctx, st.scope, request)
	cancelling, last, unstopped := st.endStart(a, ref, err)
	if err != nil {
		if cancelling {
			// The run closes once its last turn is gone, never while a sibling's stop is
			// still unconfirmed.
			if last {
				h.finishCancel(st, d.RunID)
			}
			return
		}
		var setup *execution.MCPFailure
		if errors.As(err, &setup) {
			fail(mcpFailure(err))
			return
		}
		fail(h.sessionFailure(st.scope, "execution refused to start the Agent: "+publicReason(err)))
		return
	}

	// Neither let through nor abandoned (an unforeseen return or a panic below): abandon the
	// first turn and end the session rather than leave the turn waiting.
	held := true
	defer func() {
		if held {
			close(release)
			_ = h.end(st.scope, ref.SessionID)
		}
	}()
	// Desktop publishes the binding only after the owning prompt is accepted; Start returning a
	// Ref is that acceptance here, and the prompt waits for the binding before it reaches the
	// Agent. A refused binding (the attempt was cancelled or already bound elsewhere) abandons
	// the held-back turn, which never reaches an Agent, before the session is ended: the turn
	// ends even when the End cannot read the session.
	if _, err := h.store.BindWorkflowNodeSession(h.ctx, st.scope.TenantID, d.RunID, d.NodeRunID, ref.SessionID); err != nil {
		held = false
		close(release)
		endErr := h.end(st.scope, ref.SessionID)
		cancelling, last := h.endTurn(st, d.RunID, a, endErr)
		if cancelling {
			if last {
				h.finishCancel(st, d.RunID)
			}
			return
		}
		fail(workflow.Failure{Kind: workflow.FailureSessionBindingRejected, Message: "the attempt refused its session binding"})
		return
	}
	held = false
	// Decided under mu, which CancelRun holds to set cancelling: a cancellation either comes
	// first and the prompt is abandoned, or comes after and stops a released turn.
	st.mu.Lock()
	cancelling = st.cancelling
	if cancelling {
		close(release)
	} else {
		release <- struct{}{}
	}
	st.mu.Unlock()
	if cancelling {
		// The run is being cancelled while the first prompt waits: like an attempt abandoned
		// before its Start, no Agent ever sees it, and the session is ended after.
		endErr := h.end(st.scope, ref.SessionID)
		if _, last := h.endTurn(st, d.RunID, a, endErr); last {
			h.finishCancel(st, d.RunID)
		}
		return
	}
	h.hub.Publish(d.RunID)
	if unstopped {
		// The run's cancellation gave up waiting for this Start: stop the turn it returned. The
		// turn then ends as cancelled like any other, and the last one closes the run.
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), h.stopBudget())
			defer cancel()
			if _, err := h.exec.Cancel(stopCtx, st.scope, ref); err != nil {
				h.log.Warn("stop a turn started after its run was cancelled", "run", d.RunID, "node_run", d.NodeRunID, "error", publicReason(err))
			}
		}()
	}

	if interactive {
		// Desktop parks an interactive node after its first turn instead of ending the session.
		n := st.track(&interactiveNode{d: d, executor: executor, cfg: cfg, task: task, session: ref.SessionID, phase: phaseTurn,
			policy: policy, lastOrigin: originWorkflow, baseline: baseline, baselineUnwritten: baseline != nil})
		// From here on whoever ends the node's session withdraws its tool.
		tracked = true
		h.refreshSummary(st, n, store.WorkflowStatusRunning) // the starter can stop the first turn too
		result, err := h.awaitTurn(st.scope, ref, caller)
		h.settleInteractiveTurn(st, a, n, 0, result, err)
		return
	}
	result, err := h.awaitTurn(st.scope, ref, caller)
	endErr := h.end(st.scope, ref.SessionID)
	if cancelling, last := h.endTurn(st, d.RunID, a, endErr); cancelling {
		if last {
			h.finishCancel(st, d.RunID)
		}
		return
	}
	if err != nil {
		if h.ctx.Err() != nil {
			return // shutting down; the next start interrupts the run
		}
		fail(workflow.Failure{Kind: workflow.FailureSession, Message: "lost the Agent turn: " + publicReason(err)})
		return
	}
	h.settle(d, result, executor, cfg, task)
}

// beginStart registers a turn about to start under key (the turn's caller attempt) for the node
// run nodeRun, or returns nil once cancellation was requested, the turn's Loop scope was closed,
// or the dispatch belongs to a generation a resume has since replaced.
func (st *runState) beginStart(key, nodeRun, scopeID string, generation uint64) *attemptState {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.register(key, nodeRun, scopeID, generation)
}

// register is beginStart for a caller holding mu.
func (st *runState) register(key, nodeRun, scopeID string, generation uint64) *attemptState {
	if st.cancelling || st.closedScopes[scopeID] || generation != st.generation {
		return nil
	}
	if st.attempts == nil {
		st.attempts = map[string]*attemptState{}
	}
	a := &attemptState{nodeRun: nodeRun, scope: scopeID, starting: make(chan struct{}), done: make(chan struct{})}
	st.attempts[key] = a
	return a
}

// maxRunSessions is the pinned Node's limit of unfinished sessions on one checkout
// (shared_checkout_sessions). A run's sessions share its checkout, so the bridge never asks for more.
const maxRunSessions = 8

// beginSession is beginStart for a turn that starts a new session (an Agent attempt, or an
// interactive node's first turn). It first waits until the run holds fewer unfinished sessions
// than the Node admits on its checkout, so a fan-out wider than the free slots queues here instead
// of being refused by Node; parked interactive sessions keep their slots until they end. It
// returns nil like beginStart, also when the host shuts down while it waits.
//
// An attempt that has to wait says so: its row carries `session_slot_wait` from the first time it
// finds no free slot until it takes one or gives up, so the run view can tell it from a turn that
// is executing. One that finds a slot at once writes nothing.
func (h *Host) beginSession(st *runState, d workflow.AgentDispatch, generation uint64) *attemptState {
	a, waited := h.awaitSessionSlot(st, d, generation)
	if waited {
		// Cleared before the Start, so the node reads as running once its session is asked for, and
		// also when the attempt gave up: a cancelled row must not keep reading as queued.
		h.clearSessionSlotWait(d)
	}
	return a
}

// awaitSessionSlot is beginSession's wait; waited reports whether it marked the attempt as queued.
func (h *Host) awaitSessionSlot(st *runState, d workflow.AgentDispatch, generation uint64) (a *attemptState, waited bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for {
		if st.cancelling || st.closedScopes[d.ScopeID] || generation != st.generation || h.ctx.Err() != nil {
			return nil, waited
		}
		if st.sessionsInUse() < h.sessionLimit {
			a := st.register(d.NodeRunID, d.NodeRunID, d.ScopeID, generation)
			if a != nil {
				a.session = true
			}
			return a, waited
		}
		if !waited {
			// Written outside mu, so siblings freeing slots never wait on the database; the slots
			// are looked at again right after.
			waited = true
			st.mu.Unlock()
			h.markAwaitingSessionSlot(d)
			st.mu.Lock()
			continue
		}
		if st.slotsChanged == nil {
			st.slotsChanged = make(chan struct{})
		}
		changed := st.slotsChanged
		st.mu.Unlock()
		select {
		case <-changed:
		case <-h.ctx.Done():
		}
		st.mu.Lock()
	}
}

// markAwaitingSessionSlot records that the attempt queues for a session slot. A failed write only
// costs the hint: the attempt still waits and starts as usual.
func (h *Host) markAwaitingSessionSlot(d workflow.AgentDispatch) {
	wait := store.WorkflowSessionSlotWait{Since: h.now().UnixMilli(), Limit: h.sessionLimit}
	if err := h.store.MarkWorkflowNodeAwaitingSessionSlot(h.ctx, d.RunID, d.NodeRunID, wait); err != nil {
		if h.ctx.Err() == nil {
			h.log.Warn("record a workflow node waiting for a session slot", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
		}
		return
	}
	h.hub.Publish(d.RunID)
}

// clearSessionSlotWait drops the marker markAwaitingSessionSlot wrote. It also runs while the host
// shuts down, so the row the next start finds does not keep reading as queued.
func (h *Host) clearSessionSlotWait(d workflow.AgentDispatch) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), h.stopTimeout)
	defer cancel()
	if err := h.store.ClearWorkflowNodeSessionSlotWait(ctx, d.RunID, d.NodeRunID); err != nil {
		h.log.Warn("clear the session slot wait of a workflow node", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
		return
	}
	h.hub.Publish(d.RunID)
}

// sessionsInUse counts the run's unfinished sessions: every turn starting a new session, from just
// before its Start until its attempt is forgotten (an automatic attempt after its session's End
// was confirmed), every tracked interactive node whose session's End was not confirmed, counted once
// while its first turn is also such a turn, and every lingering session (see linger). Callers hold mu.
func (st *runState) sessionsInUse() int {
	used := 0
	starting := map[string]bool{}
	for _, a := range st.attempts {
		if a.session {
			used++
			starting[a.nodeRun] = true
		}
	}
	for _, n := range st.interactive {
		if !n.sessionGone && !starting[n.d.NodeRunID] {
			used++
		}
	}
	return used + len(st.unended)
}

// slotsMayHaveChanged wakes the attempts waiting in beginSession. Callers hold mu.
func (st *runState) slotsMayHaveChanged() {
	if st.slotsChanged != nil {
		close(st.slotsChanged)
		st.slotsChanged = nil
	}
}

// releaseStart forgets a turn beginStart registered that will not start, and reports whether it
// was the cancelled run's last turn (see endTurn).
func (st *runState) releaseStart(a *attemptState) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.forget(a)
	return st.lastCancelled()
}

// endStart publishes the started turn to a waiting cancellation and reports whether one was
// requested meanwhile, whether a refused Start left the cancelled run with no turn (see endTurn),
// and whether a cancellation gave up waiting for this Start, so that nobody stops the turn it
// returned unless its attempt does (see stopTurns).
func (st *runState) endStart(a *attemptState, ref execution.Ref, err error) (cancelling, last, unstopped bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil {
		st.forget(a)
		return st.cancelling, st.lastCancelled(), false
	}
	a.ref = &ref
	close(a.starting)
	a.starting = nil
	st.startResolved()
	return st.cancelling, false, a.unstopped
}

// endTurn forgets the turn that ended and reports whether cancellation was requested, and whether
// this was the cancelled run's last turn. Only that last one closes the run: a sibling whose stop
// is not confirmed yet keeps the run open until its own turn ends.
func (st *runState) endTurn(a *attemptState, endErr error) (cancelling, last bool) {
	cancelling, last, _ = st.endSessionTurn(a, endErr)
	return cancelling, last
}

// endTurn is runState.endTurn for a turn that started a new session and then had it ended: when
// that End was not confirmed, the session keeps its slot in the same critical section that forgets
// the turn, and is ended again in the background (see linger).
func (h *Host) endTurn(st *runState, runID string, a *attemptState, endErr error) (cancelling, last bool) {
	cancelling, last, session := st.endSessionTurn(a, endErr)
	if session != "" {
		h.retryEnd(st, runID, session)
	}
	return cancelling, last
}

// endSessionTurn implements both endTurns and returns the session that must linger, if any.
func (st *runState) endSessionTurn(a *attemptState, endErr error) (cancelling, last bool, lingering string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if endConfirmed(endErr) {
		endErr = nil // a session already gone is ended
	}
	if endErr != nil && a.scope != "" {
		if st.scopeCleanupErrors == nil {
			st.scopeCleanupErrors = map[string]error{}
		}
		st.scopeCleanupErrors[a.scope] = endErr
	}
	if endErr != nil && a.session && a.ref != nil && st.linger(a.ref.SessionID) {
		lingering = a.ref.SessionID
	}
	st.forget(a)
	return st.cancelling, st.lastCancelled(), lingering
}

// startResolved wakes whoever waits for any of the run's Starts to return (see stopTurns).
// Callers hold mu.
func (st *runState) startResolved() {
	if st.startsChanged != nil {
		close(st.startsChanged)
		st.startsChanged = nil
	}
}

// lastCancelled reports whether cancellation was requested and no turn is left. Callers hold mu.
func (st *runState) lastCancelled() bool {
	return st.cancelling && len(st.attempts) == 0
}

// forget removes the turn and wakes whoever waits for its start or its end. Callers hold mu.
func (st *runState) forget(a *attemptState) {
	for key, current := range st.attempts {
		if current == a {
			delete(st.attempts, key)
		}
	}
	if a.starting != nil {
		close(a.starting)
		a.starting = nil
		st.startResolved()
	}
	select {
	case <-a.done:
	default:
		close(a.done)
	}
	if a.session {
		st.slotsMayHaveChanged()
	}
}

// awaitTurn follows the session until the owning turn's terminal event. If the stream ends or
// breaks first, recoverTurn reads the durable attempt and re-subscribes from the last observed
// sequence whenever the turn is still running.
func (h *Host) awaitTurn(scope execution.Scope, ref execution.Ref, caller execution.Caller) (execution.Result, error) {
	var cursor uint64
	resubscribed := false
	stream, err := h.exec.Subscribe(h.ctx, scope, ref.SessionID, cursor)
	for {
		if err == nil {
			previousCursor := cursor
			var result execution.Result
			var done bool
			result, done, err = h.drainTurnStream(stream, ref.TurnID, &cursor)
			if done {
				return result, nil
			}
			if resubscribed && cursor == previousCursor {
				if pauseErr := h.resultRetry.Pause(h.ctx); pauseErr != nil {
					return execution.Result{}, errors.Join(err, pauseErr)
				}
			}
		}
		var next execution.Stream
		var result execution.Result
		var got bool
		result, next, got, err = h.recoverTurn(scope, ref, caller, cursor, err)
		if got || err != nil {
			return result, err
		}
		stream = next
		resubscribed = true
	}
}

func (h *Host) drainTurnStream(stream execution.Stream, turnID string, cursor *uint64) (execution.Result, bool, error) {
	defer stream.Close()
	for {
		event, err := stream.Next(h.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = execution.ErrSessionEnded
			}
			return execution.Result{}, false, err
		}
		if event.Sequence > *cursor {
			*cursor = event.Sequence
		}
		if event.Kind == execution.EventTurnEnded && event.Ref.TurnID == turnID && event.Result != nil {
			return *event.Result, true, nil
		}
	}
}

func (h *Host) recoverTurn(scope execution.Scope, ref execution.Ref, caller execution.Caller, cursor uint64, cause error) (execution.Result, execution.Stream, bool, error) {
	if h.ctx.Err() != nil {
		return execution.Result{}, nil, false, h.ctx.Err()
	}
	var result execution.Result
	var next execution.Stream
	var got bool
	err := h.resultRetry.Read(h.ctx, func(ctx context.Context) error {
		turn, err := h.exec.ReadAttempt(ctx, scope, caller)
		if err != nil {
			return err
		}
		if turn.Result != nil {
			result = *turn.Result
			got = true
			return nil
		}
		if turn.EndedAt == nil && !errors.Is(cause, execution.ErrSessionEnded) {
			stream, err := h.exec.Subscribe(h.ctx, scope, ref.SessionID, cursor)
			if err == nil {
				next = stream
				return nil
			}
			return err
		}
		return execution.ErrUnavailable // ended without a written result; keep polling within budget
	})
	if err != nil {
		if next != nil {
			_ = next.Close()
		}
		return execution.Result{}, nil, false, errors.Join(cause, err)
	}
	return result, next, got, nil
}

// end stops the node's session, as desktop's stop_session after the turn; the transcript stays
// readable. Failing to confirm is logged: the outcome is already decided by the turn's result.
func (h *Host) end(scope execution.Scope, sessionID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), h.stopTimeout)
	defer cancel()
	if err := h.exec.End(ctx, scope, sessionID); err != nil {
		h.log.Warn("end workflow node session", "session", sessionID, "error", err)
		return err
	}
	return nil
}

// settle maps the turn's result onto the engine like desktop's report_outcome. Only an
// authoritative complete answer can succeed; the stop reason then decides as desktop does.
func (h *Host) settle(d workflow.AgentDispatch, r execution.Result, ex resolvedExecutor, cfg workflow.AgentConfig, task string) {
	if r.MCPFailure != nil {
		h.fail(d, mcpFailure(r.MCPFailure))
		return
	}
	var output *string
	if r.Answer != "" {
		output = &r.Answer
	}
	if r.Status != execution.StatusCompleted || !r.AnswerComplete {
		reason := string(r.Status)
		if r.FailureCode != "" {
			reason += ": " + r.FailureCode
		} else if r.Status == execution.StatusCompleted {
			reason = "the final answer could not be read completely"
		}
		h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "the Agent turn did not complete (" + reason + ")", Output: output})
		return
	}
	switch r.StopReason {
	case "end_turn", "max_tokens", "max_turn_requests":
		answer := r.Answer
		completion := workflow.Completion{Output: &answer, Keys: completionKeys(r, ex, cfg, task)}
		if cfg.Structured != nil {
			// desktop's apply_output_contract: the raw answer stays the output, the validated
			// object becomes {node}.structured_output, and a nonconforming answer fails the node
			// while keeping the answer inspectable.
			structured, err := workflow.ParseStructuredOutput(answer, cfg.Structured)
			if err != nil {
				h.fail(d, workflow.Failure{Kind: workflow.FailureStructuredOutput, Message: err.Error(), Output: &answer})
				return
			}
			completion.Structured = structured
		}
		h.complete(d, completion)
	case "refusal":
		h.fail(d, workflow.Failure{Kind: workflow.FailureAgentRefusal, Message: "agent refused the request", Output: output})
	case "":
		h.fail(d, workflow.Failure{Kind: workflow.FailureSessionEndedWithoutReason, Message: "the Agent turn ended without a stop reason", Output: output})
	default:
		h.fail(d, workflow.Failure{Kind: workflow.FailureUnknownStopReason, Message: "agent stopped for a reason this Ora version does not recognize", Output: output})
	}
}

func mcpFailure(err error) workflow.Failure {
	// Infrastructure and authorization errors are retryable execution failures. Never expose
	// their raw text (which may contain SQL or credentials) as a public MCP diagnostic.
	failure := workflow.Failure{Kind: workflow.FailureSession, Message: "execution could not resolve MCP configuration"}
	var setup *execution.MCPFailure
	if errors.As(err, &setup) && execution.IsMCPFailureCode(setup.Code) {
		failure.Kind = workflow.FailureMissingAgentConfig
		failure.Message, failure.Code = setup.Code, setup.Code
		failure.PluginID, failure.SettingID, failure.Transport = setup.PluginID, setup.SettingID, setup.Transport
	}
	return failure
}

// completionKeys are the attempt payload keys a successful Agent writes: desktop's stop_reason,
// plus what v2 adds so the run detail can show the executor actually used, the task instruction
// as rendered from the variable pool (the attempt input keeps the template) and the configured
// capabilities v2 ran without.
func completionKeys(r execution.Result, ex resolvedExecutor, cfg workflow.AgentConfig, task string) map[string]any {
	unapplied := cfg.Unapplied
	if unapplied == nil {
		unapplied = []string{}
	}
	return map[string]any{
		"stop_reason": r.StopReason,
		"executor": map[string]any{
			"id": ex.executor.ID, "agentId": ex.executor.AgentID, "name": ex.executor.AgentName,
			"pluginVersion": ex.executor.PluginVersion,
		},
		"model": map[string]any{
			"id": ex.executor.Model.ID, "name": ex.executor.Model.Name,
			"requested": cfg.ModelID, "fallback": false,
		},
		"unapplied": unapplied,
		"skills":    cfg.Skills,
		"task":      task,
	}
}

type resolvedExecutor struct {
	executor execution.Executor
}

// resolveExecutor maps the node's agentCli to an installed executor's AgentID and its modelId to
// one of that Agent's models by ID or name. An unknown model fails with desktop's
// model-not-found error; an unknown Agent keeps the existing missing-ref failure.
func (h *Host) resolveExecutor(scope execution.Scope, cfg workflow.AgentConfig) (resolvedExecutor, *workflow.Failure) {
	if cfg.AgentCLI == "" {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "the node selects no Agent"}
	}
	executors, err := h.catalog.ListExecutors(h.ctx, scope)
	if err != nil {
		failure := h.sessionFailure(scope, "list executors: "+publicReason(err))
		return resolvedExecutor{}, &failure
	}
	return matchExecutor(executors, cfg)
}

// recoveredExecutor is resolveExecutor for a node parked before a restart. Failing that node ends
// a wait the starter can only replace by starting the node over, so a catalog that cannot be read
// is no verdict on it: a transient error is retried within resultRetry's budget, and an error that
// outlasts it, or any other error that says nothing about this node, is returned to abort startup
// like a session that cannot be read, leaving the row for the next start. A verdict fails the node
// as before the restart: an executor the catalog no longer offers, or a catalog that has nothing
// for the run's scope (ErrNotFound; today's catalog ignores the scope and never returns it, the
// branch is for a scoped catalog), which would otherwise abort every later start on the same row.
func (h *Host) recoveredExecutor(ctx context.Context, scope execution.Scope, cfg workflow.AgentConfig) (resolvedExecutor, *workflow.Failure, error) {
	if cfg.AgentCLI == "" {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "the node selects no Agent"}, nil
	}
	var executors []execution.Executor
	err := h.resultRetry.Read(ctx, func(ctx context.Context) error {
		var err error
		executors, err = h.catalog.ListExecutors(ctx, scope)
		return err
	})
	if errors.Is(err, execution.ErrNotFound) {
		failure := h.sessionFailure(scope, "list executors: "+publicReason(err))
		return resolvedExecutor{}, &failure, nil
	}
	if err != nil {
		return resolvedExecutor{}, nil, fmt.Errorf("list executors: %w", err)
	}
	executor, failure := matchExecutor(executors, cfg)
	return executor, failure, nil
}

// matchExecutor is resolveExecutor's choice among the catalog's executors.
func matchExecutor(executors []execution.Executor, cfg workflow.AgentConfig) (resolvedExecutor, *workflow.Failure) {
	var first *execution.Executor
	for i := range executors {
		e := executors[i]
		if e.AgentID != cfg.AgentCLI {
			continue
		}
		if cfg.ModelID != "" && (e.Model.ID == cfg.ModelID || e.Model.Name == cfg.ModelID) {
			return resolvedExecutor{executor: e}, nil
		}
		if first == nil {
			first = &executors[i]
		}
	}
	if first == nil {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: fmt.Sprintf("agent %s is not available in v2", cfg.AgentCLI)}
	}
	modelLabel := "model"
	if cfg.ModelID != "" {
		modelLabel += " " + cfg.ModelID
	}
	return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureWorkflowModelNotFound, Message: fmt.Sprintf("%s is not advertised by agent %s", modelLabel, cfg.AgentCLI)}
}

// publicReason names an execution error by its stable sentinel, never infrastructure detail.
func publicReason(err error) string {
	if errors.Is(err, executionwait.ErrExhausted) {
		return "execution result could not be read before the retry deadline"
	}
	for _, known := range []error{
		execution.ErrUnavailable, execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrWorkspaceBusy,
		execution.ErrConflict, execution.ErrUnsupported, execution.ErrStopUnconfirmed, execution.ErrSessionEnded,
		execution.ErrHostMCPUnavailable,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "execution error"
}

// sessionFailure classifies a refusal to list executors or start an Agent under scope. When the
// identity the run executes under is no longer an active member of the workspace, the refusal
// fails the node as starter_unavailable, which never retries automatically: a retry under the same
// identity is refused again. Anything else, including a failed membership lookup, stays a session
// failure.
func (h *Host) sessionFailure(scope execution.Scope, message string) workflow.Failure {
	if active, err := h.activeMember(h.ctx, scope.TenantID, scope.UserID); err == nil && !active {
		return workflow.Failure{Kind: workflow.FailureStarterUnavailable, Message: "the run's starter is no longer an active member of the workspace (" + message + ")"}
	}
	return workflow.Failure{Kind: workflow.FailureSession, Message: message}
}

// activeMember reports whether user may still act in tenant: a member whose account is active, in
// an active workspace, the same rule every request and every execution start checks.
func (h *Host) activeMember(ctx context.Context, tenant, user string) (bool, error) {
	_, err := h.store.TenantAccess(ctx, tenant, user)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
