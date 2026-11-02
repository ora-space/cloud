package workflowhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// Dispatch queues one Agent attempt on its run's worker and returns at once. A run's Agent
// attempts execute one at a time: v2's execution service lends every workflow run one shared
// directory and refuses a second concurrent turn with ErrWorkspaceBusy.
func (h *Host) Dispatch(d workflow.AgentDispatch) {
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
	defer st.mu.Unlock()
	st.queue = append(st.queue, d)
	if !st.working {
		st.working = true
		h.workers.Add(1)
		go h.work(d.RunID, st)
	}
}

// work drains the run's queue. Attempts dispatched after a cancellation request are dropped:
// the run is about to close as cancelled together with their rows. Attempts whose run ended
// while they waited (a sibling failed it) are closed without starting a turn.
func (h *Host) work(runID string, st *runState) {
	defer h.workers.Done()
	for {
		st.mu.Lock()
		if len(st.queue) == 0 || h.ctx.Err() != nil {
			st.queue = nil
			st.working = false
			st.mu.Unlock()
			h.forgetFinished(runID, st)
			return
		}
		d := st.queue[0]
		st.queue = st.queue[1:]
		cancelling := st.cancelling
		st.mu.Unlock()
		if cancelling || h.abandoned(d) {
			continue
		}
		h.runAgent(st, d)
	}
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
	h.mu.Lock()
	defer h.mu.Unlock()
	st.mu.Lock()
	idle := !st.working && st.starters == 0 && st.starting == nil && st.active == nil
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

func (h *Host) fail(d workflow.AgentDispatch, f workflow.Failure) {
	if err := h.engine.FailNode(h.ctx, d.RunID, d.NodeRunID, f); err != nil {
		h.log.Error("record workflow node failure", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

func (h *Host) complete(d workflow.AgentDispatch, c workflow.Completion) {
	if err := h.engine.CompleteNode(h.ctx, d.RunID, d.NodeRunID, c); err != nil {
		h.log.Error("record workflow node completion", "run", d.RunID, "node_run", d.NodeRunID, "error", err)
	}
}

// finishCancel closes a run whose cancellation was requested once its turn is known to have
// ended, covering a cancellation whose stop the execution service did not confirm in time.
func (h *Host) finishCancel(st *runState, runID string) {
	if _, err := h.store.CancelWorkflowRun(h.ctx, st.scope.TenantID, runID); err != nil {
		if h.ctx.Err() == nil { // at shutdown the next start interrupts the run instead
			h.log.Error("close cancelled workflow run", "run", runID, "error", err)
		}
		return
	}
	h.hub.Publish(runID)
}

// runAgent drives one Agent attempt as desktop's drive_agent_node: resolve the executor, render
// the prompt, start the session, bind it to the attempt, wait for the owning turn's result, end
// the session and report the outcome to the engine.
func (h *Host) runAgent(st *runState, d workflow.AgentDispatch) {
	cfg := d.Node.Agent()
	executor, failure := h.resolveExecutor(st.scope, cfg)
	if failure != nil {
		h.fail(d, *failure)
		return
	}
	task, err := workflow.RenderTemplate(cfg.Prompt, &d.Pool)
	if err != nil {
		h.fail(d, workflow.Failure{Kind: workflow.FailurePromptTemplate, Message: err.Error()})
		return
	}
	caller := execution.Caller{Kind: execution.CallerWorkflow, ID: d.RunID, NodeID: d.Node.ID, AttemptID: d.NodeRunID}
	request := execution.StartRequest{
		Caller: caller, Target: execution.Target{ProjectID: d.ProjectID},
		ExecutorID: executor.executor.ID, Prompt: agentPrompt(d, task),
	}

	if !st.beginStart(d.ScopeID) {
		return
	}
	ref, err := h.start(st.scope, request)
	cancelling := st.endStart(ref, err)
	if err != nil {
		if cancelling {
			h.finishCancel(st, d.RunID)
			return
		}
		h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "execution refused to start the Agent: " + publicReason(err)})
		return
	}

	// Desktop publishes the binding only after the owning prompt is accepted; Start returning a
	// Ref is that acceptance here. A refused binding (the attempt was cancelled or already
	// bound elsewhere) must not leave the new session running unowned.
	if _, err := h.store.BindWorkflowNodeSession(h.ctx, st.scope.TenantID, d.RunID, d.NodeRunID, ref.SessionID); err != nil {
		endErr := h.end(st.scope, ref.SessionID)
		if st.endTurn(endErr) {
			h.finishCancel(st, d.RunID)
			return
		}
		h.fail(d, workflow.Failure{Kind: workflow.FailureSessionBindingRejected, Message: "the attempt refused its session binding"})
		return
	}
	h.hub.Publish(d.RunID)

	result, err := h.awaitTurn(st.scope, ref, caller)
	endErr := h.end(st.scope, ref.SessionID)
	if st.endTurn(endErr) {
		h.finishCancel(st, d.RunID)
		return
	}
	if err != nil {
		if h.ctx.Err() != nil {
			return // shutting down; the next start interrupts the run
		}
		h.fail(d, workflow.Failure{Kind: workflow.FailureSession, Message: "lost the Agent turn: " + publicReason(err)})
		return
	}
	h.settle(d, result, executor, cfg, task)
}

// beginStart reserves the start slot, or reports false once cancellation was requested.
func (st *runState) beginStart(scopeID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cancelling || st.closedScopes[scopeID] {
		return false
	}
	st.starting = make(chan struct{})
	st.turnDone = make(chan struct{})
	st.activeScope = scopeID
	return true
}

// endStart publishes the started turn to a waiting cancellation and reports whether one was
// requested meanwhile.
func (st *runState) endStart(ref execution.Ref, err error) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err == nil {
		st.active = &ref
	} else {
		close(st.turnDone)
		st.turnDone = nil
		st.activeScope = ""
	}
	close(st.starting)
	st.starting = nil
	return st.cancelling
}

// endTurn forgets the finished turn and reports whether cancellation was requested.
func (st *runState) endTurn(endErr error) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.active = nil
	if endErr != nil && st.activeScope != "" {
		if st.scopeCleanupErrors == nil {
			st.scopeCleanupErrors = map[string]error{}
		}
		st.scopeCleanupErrors[st.activeScope] = endErr
	}
	st.activeScope = ""
	if st.turnDone != nil {
		close(st.turnDone)
		st.turnDone = nil
	}
	return st.cancelling
}

// start submits the turn, waiting out ErrWorkspaceBusy while the run's previous turn releases the
// shared directory: its terminal event is published before the execution worker cleans up.
// Repeating the same caller attempt is idempotent, so a retry never starts a second turn.
func (h *Host) start(scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
	deadline := time.Now().Add(h.busyRetry)
	for {
		ref, err := h.exec.Start(h.ctx, scope, req)
		if !errors.Is(err, execution.ErrWorkspaceBusy) || time.Now().After(deadline) {
			return ref, err
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-h.ctx.Done():
			return execution.Ref{}, h.ctx.Err()
		}
	}
}

// awaitTurn follows the session until the owning turn's terminal event. If the stream ends or
// breaks first, the recorded attempt is the authority: it carries the result once written.
func (h *Host) awaitTurn(scope execution.Scope, ref execution.Ref, caller execution.Caller) (execution.Result, error) {
	stream, err := h.exec.Subscribe(h.ctx, scope, ref.SessionID, 0)
	if err != nil {
		return h.recordedResult(scope, caller, err)
	}
	defer stream.Close()
	for {
		event, err := stream.Next(h.ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = execution.ErrSessionEnded
			}
			return h.recordedResult(scope, caller, err)
		}
		if event.Kind == execution.EventTurnEnded && event.Ref.TurnID == ref.TurnID && event.Result != nil {
			return *event.Result, nil
		}
	}
}

func (h *Host) recordedResult(scope execution.Scope, caller execution.Caller, cause error) (execution.Result, error) {
	if h.ctx.Err() != nil {
		return execution.Result{}, h.ctx.Err()
	}
	turn, err := h.exec.ReadAttempt(h.ctx, scope, caller)
	if err == nil && turn.Result != nil {
		return *turn.Result, nil
	}
	return execution.Result{}, cause
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
			"requested": cfg.ModelID, "fallback": ex.fallback,
		},
		"unapplied": unapplied,
		"task":      task,
	}
}

type resolvedExecutor struct {
	executor execution.Executor
	fallback bool
}

// resolveExecutor maps the node's agentCli to an installed executor's AgentID and its modelId to
// one of that Agent's models by ID or name. An unknown model falls back to the Agent's first
// (default) executor and says so; an unknown Agent fails the node like desktop's missing ref.
func (h *Host) resolveExecutor(scope execution.Scope, cfg workflow.AgentConfig) (resolvedExecutor, *workflow.Failure) {
	if cfg.AgentCLI == "" {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "the node selects no Agent"}
	}
	executors, err := h.catalog.ListExecutors(h.ctx, scope)
	if err != nil {
		return resolvedExecutor{}, &workflow.Failure{Kind: workflow.FailureSession, Message: "list executors: " + publicReason(err)}
	}
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
	return resolvedExecutor{executor: *first, fallback: true}, nil
}

// publicReason names an execution error by its stable sentinel, never infrastructure detail.
func publicReason(err error) string {
	for _, known := range []error{
		execution.ErrUnavailable, execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrWorkspaceBusy,
		execution.ErrConflict, execution.ErrUnsupported, execution.ErrStopUnconfirmed, execution.ErrSessionEnded,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "execution error"
}
