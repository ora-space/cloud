//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/nodehistory"
)

var errEventStore = errors.New("agent_event_write_failed")

type turnOutcome struct {
	turnID string
	result execution.Result
	err    error
}

type activeTurn struct {
	id   string
	done chan turnOutcome
}

type agentSession struct {
	frozen            execution.Session
	scope             execution.Scope
	execution         string
	project           *projectRuntime
	checkout          *checkout
	projection        *nodehistory.Projection
	ready             chan struct{}
	prepareErr        error
	stopped           chan struct{}
	stopOnce          sync.Once
	commandMu         sync.Mutex
	mu                sync.Mutex
	started           bool
	ended             bool
	ending            bool
	broken            bool
	publicationFailed bool
	active            *activeTurn
	emit              Emit
}

func (d *Driver) session(ctx context.Context, scope execution.Scope, session execution.Session, config Project) (*agentSession, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, execution.ErrUnavailable
	}
	a := d.sessions[session.ID]
	if a != nil && (a.scope != scope || a.frozen.WorkspaceID != session.WorkspaceID || a.frozen.Target.ProjectID != session.Target.ProjectID || *a.frozen.Executor != *session.Executor) {
		d.mu.Unlock()
		return nil, execution.ErrConflict
	}
	if a == nil {
		target, executor := *session.Target, *session.Executor
		session.Target, session.Executor = &target, &executor
		a = &agentSession{frozen: session, scope: scope, execution: d.nextID("a") + "-" + digest(session.ID), ready: make(chan struct{}), stopped: make(chan struct{})}
		d.sessions[session.ID] = a
		go func() {
			defer func() {
				if a.prepareErr != nil {
					// Preparation has not sent Start: there is no physical Agent to retain.
					// Release only this reservation; another attempt may already own the map key.
					a.markStopped()
					d.mu.Lock()
					if d.sessions[session.ID] == a {
						delete(d.sessions, session.ID)
					}
					d.mu.Unlock()
				}
				close(a.ready)
			}()
			prepare, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			p, err := d.project(prepare, session.Target.ProjectID, config)
			a.project = p
			if err != nil {
				a.prepareErr = err
				return
			}
			if p.configuration.MirrorPath != config.MirrorPath {
				a.prepareErr = execution.ErrUnavailable
				return
			}
			p.mu.Lock()
			if p.fatal {
				p.mu.Unlock()
				a.prepareErr = execution.ErrUnavailable
				return
			}
			if p.owners[session.WorkspaceID] != "" && p.owners[session.WorkspaceID] != session.ID {
				p.mu.Unlock()
				a.prepareErr = execution.ErrWorkspaceBusy
				return
			}
			p.owners[session.WorkspaceID] = session.ID
			p.mu.Unlock()
			// Run has checked the executor names a known plugin; its install overlaps the clone.
			plugin, _ := findPlugin(session.Executor.AgentID, session.Executor.PluginVersion)
			installed := p.install(plugin)
			a.checkout, a.prepareErr = p.prepareCheckout(prepare, session.WorkspaceID, config)
			if a.prepareErr != nil {
				return
			}
			if a.prepareErr = installed.wait(prepare, p); a.prepareErr != nil {
				return
			}
			a.projection = nodehistory.NewProjection(p.reader, a.execution)
			p.skillsMu.Lock()
			defer p.skillsMu.Unlock()
			p.mu.Lock()
			if p.fatal {
				p.mu.Unlock()
				a.prepareErr = execution.ErrUnavailable
				return
			}
			p.agents[a.execution] = a
			p.mu.Unlock()
		}()
	}
	d.mu.Unlock()
	select {
	case <-a.ready:
		return a, a.prepareErr
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *agentSession) begin(turn string, emit Emit) (*activeTurn, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended || a.ending {
		return nil, false, execution.ErrSessionEnded
	}
	if a.broken {
		return nil, false, execution.ErrUnavailable
	}
	if a.active != nil {
		return nil, false, execution.ErrWorkspaceBusy
	}
	select {
	case <-a.project.dead:
		return nil, false, execution.ErrUnavailable
	default:
	}
	call := &activeTurn{id: turn, done: make(chan turnOutcome, 1)}
	a.active, a.emit = call, emit
	fresh := !a.started
	a.started = true // A failed write can be ambiguous; End still addresses this execution.
	return call, fresh, nil
}

func (a *agentSession) submit(turn, text string, fresh bool) error {
	content := []any{map[string]string{"type": "text", "text": text}}
	if fresh {
		// Run has already checked that the executor names a known plugin and binding.
		plugin, _ := findPlugin(a.frozen.Executor.AgentID, a.frozen.Executor.PluginVersion)
		configuration, err := plugin.agentConfiguration(a.project.driver.opts.Bindings[a.frozen.Executor.Model.ID].Model)
		if err != nil {
			return execution.ErrUnavailable
		}
		if err := a.project.services.Permit(a.execution, a.frozen.Executor.Model.ID); err != nil {
			return execution.ErrUnavailable
		}
		spec := map[string]any{
			"node_id": a.project.nodeID(), "agent_plugin_id": plugin.id, "agent_plugin_version": plugin.version,
			"checkout_execution_id": a.checkout.id, "model_binding_id": a.frozen.Executor.Model.ID,
			"git_identity": map[string]string{"name": "V2", "email": "v2@example.test"},
			"initial_turn": map[string]any{"turn_id": turn, "content": content},
		}
		if configuration != nil {
			spec["agent_configuration"] = configuration
		}
		return a.project.send("start_agent_session", a.execution, map[string]any{"spec": spec})
	}
	return a.project.send("submit_user_turn", a.execution, map[string]any{"node_id": a.project.nodeID(), "command_id": "turn-" + digest(turn), "turn": map[string]any{"turn_id": turn, "content": content}})
}

// requestEnd never takes down the project's shared Node. The Driver owns process-tree retirement.
func (a *agentSession) requestEnd() {
	a.mu.Lock()
	if a.ended || a.ending {
		a.mu.Unlock()
		return
	}
	if !a.started {
		a.ending = true
		a.mu.Unlock()
		if a.project != nil {
			select {
			case <-a.project.dead:
				return
			default:
			}
		}
		a.markStopped()
		return
	}
	a.ending = true
	a.mu.Unlock()
	go func() {
		a.commandMu.Lock()
		defer a.commandMu.Unlock()
		if a.project.send("end_session", a.execution, map[string]string{"node_id": a.project.nodeID(), "command_id": "end-" + digest(a.frozen.ID), "reason": "user_ended"}) != nil {
			a.fail("agent_stop_unconfirmed", execution.ErrStopUnconfirmed)
		}
	}()
}

func (a *agentSession) markStopped() {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.ended = true
		a.mu.Unlock()
		if a.project != nil {
			if a.project.services != nil {
				a.project.services.End(a.execution)
			}
			a.project.mu.Lock()
			if a.project.owners[a.frozen.WorkspaceID] == a.frozen.ID {
				delete(a.project.owners, a.frozen.WorkspaceID)
			}
			a.project.mu.Unlock()
		}
		close(a.stopped)
	})
}

func (a *agentSession) complete(outcome turnOutcome) {
	a.mu.Lock()
	call := a.active
	if call != nil && call.id == outcome.turnID {
		a.active = nil
	} else {
		call = nil
	}
	a.mu.Unlock()
	if call != nil {
		call.done <- outcome
	}
}

func (a *agentSession) fail(code string, err error) {
	a.mu.Lock()
	a.broken = true
	call := a.active
	a.mu.Unlock()
	if call != nil {
		// Infrastructure strings are deliberately not exposed through Run.
		if !errors.Is(err, execution.ErrStopUnconfirmed) {
			err = errors.New(code)
		}
		a.complete(turnOutcome{call.id, failed(code), err})
	}
}

func (a *agentSession) publish(event execution.Event) error {
	a.mu.Lock()
	emit := a.emit
	a.mu.Unlock()
	if emit == nil {
		return errEventStore
	}
	event.Ref.SessionID = a.frozen.ID
	event.OccurredAt = time.Now().UTC()
	ctx, cancel := context.WithTimeout(a.project.ctx, 30*time.Second)
	defer cancel()
	if emit(ctx, event) != nil {
		a.mu.Lock()
		a.publicationFailed = true
		a.mu.Unlock()
		return errEventStore
	}
	return nil
}

func (a *agentSession) observe(message node.Message) (*turnOutcome, bool) {
	a.mu.Lock()
	call, broken, publicationFailed := a.active, a.broken, a.publicationFailed
	a.mu.Unlock()
	if message.OperationID != "op-"+a.execution && message.Type != "thread_preview" {
		a.fail("agent_identity_mismatch", execution.ErrUnavailable)
		a.requestEnd()
		return nil, false
	}
	if message.Type == "agent_session_ended" {
		var ended struct {
			Kind   string
			Result struct {
				Reason string
				Node   node.Identity
			}
		}
		validReason := func(reason string) bool {
			switch reason {
			case "user_ended", "idle_timeout", "cancelled", "agent_failed", "interrupted":
				return true
			default:
				return false
			}
		}
		if json.Unmarshal(message.Payload, &ended) != nil || ended.Kind != "agent_session_ended" || !validReason(ended.Result.Reason) || ended.Result.Node.NodeID == "" || ended.Result.Node != a.project.ipc().Identity() {
			a.fail("agent_terminal_invalid", execution.ErrUnavailable)
			return nil, false
		}
		var outcome *turnOutcome
		if call != nil {
			result := failed("agent_ended_before_turn")
			if ended.Result.Reason == "cancelled" || ended.Result.Reason == "user_ended" {
				result = execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"}
			}
			if !publicationFailed && a.publish(execution.Event{Ref: execution.Ref{TurnID: call.id}, Kind: execution.EventTurnEnded, Result: &result}) == nil {
				outcome = &turnOutcome{call.id, result, nil}
			} else {
				a.fail("agent_event_write_failed", errEventStore)
				publicationFailed = true
			}
		}
		a.markStopped()
		return outcome, !publicationFailed
	}
	if message.Type == "session_command_rejected" {
		a.fail("agent_session_ended", execution.ErrSessionEnded)
		return nil, true
	}
	if message.Type != "thread_event" && message.Type != "thread_preview" {
		return nil, !publicationFailed
	}
	if broken {
		return nil, !publicationFailed
	}
	observation, err := a.projection.Consume(a.project.ctx, message)
	if err != nil {
		a.mu.Lock()
		a.broken = true
		a.mu.Unlock()
		a.requestEnd()
		if call == nil {
			return nil, false
		}
		result := failed(nodehistory.FailureCode(err))
		if a.publish(execution.Event{Ref: execution.Ref{TurnID: call.id}, Kind: execution.EventTurnEnded, Result: &result}) != nil {
			a.fail("agent_event_write_failed", errEventStore)
			return nil, false
		}
		return &turnOutcome{call.id, result, errors.New(result.FailureCode)}, true
	}
	snapshot := &execution.HistorySnapshot{Incomplete: observation.Snapshot.Incomplete}
	for _, record := range observation.Snapshot.Records {
		snapshot.Records = append(snapshot.Records, execution.HistoryRecord{Sequence: record.Sequence, Data: record.Data, Preview: record.Preview})
	}
	turnID := observation.TurnID
	if turnID == "" && call != nil {
		turnID = call.id
	}
	if a.publish(execution.Event{Ref: execution.Ref{TurnID: turnID}, Kind: execution.EventHistorySnapshot, History: snapshot}) != nil {
		a.fail("agent_event_write_failed", errEventStore)
		a.requestEnd()
		return nil, false
	}
	if observation.Turn == nil {
		return nil, true
	}
	if call == nil || observation.TurnID != call.id {
		a.fail("agent_turn_identity_mismatch", execution.ErrUnavailable)
		a.requestEnd()
		return nil, false
	}
	result := turnResult(observation.Turn)
	if a.publish(execution.Event{Ref: execution.Ref{TurnID: turnID}, Kind: execution.EventTurnEnded, Result: &result}) != nil {
		a.fail("agent_event_write_failed", errEventStore)
		a.requestEnd()
		return nil, false
	}
	return &turnOutcome{turnID, result, nil}, true
}

func turnResult(turn *nodehistory.Turn) execution.Result {
	result := execution.Result{Answer: turn.Answer, StopReason: turn.StopReason}
	switch turn.StopReason {
	case "end_turn":
		result.Status, result.AnswerComplete = execution.StatusCompleted, true
	case "cancelled":
		result.Status = execution.StatusCancelled
	case "max_tokens", "max_turn_requests", "refusal":
		result.Status, result.FailureCode = execution.StatusFailed, "agent_turn_incomplete"
	default:
		result.Status, result.FailureCode, result.StopReason = execution.StatusFailed, "agent_stop_unknown", ""
	}
	return result
}
