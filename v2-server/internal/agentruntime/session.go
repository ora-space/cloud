//go:build linux

package agentruntime

import (
	"bytes"
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

// previewInterval bounds how often Node previews become retained history_preview events. Node
// sends up to ten a second; every retained event is stored and replayed when a page loads.
var previewInterval = 250 * time.Millisecond

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
	observeRequests   bool
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
	// publishMu orders history publications, so a preview never lands after a newer snapshot.
	publishMu     sync.Mutex
	preview       *execution.HistorySnapshot // latest pending set not yet published
	previewQueued bool
	previewAt     time.Time // when the last preview flush ran
	previewSent   []byte    // the pending set the page last received
	timing        *preparation
}

func (d *Driver) session(ctx context.Context, scope execution.Scope, session execution.Session, config Project, caller execution.CallerKind, timing *preparation) (*agentSession, error) {
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
		a = &agentSession{observeRequests: caller == execution.CallerTask || caller == execution.CallerChat, frozen: session, scope: scope, execution: d.nextID("a") + "-" + digest(session.ID), ready: make(chan struct{}), stopped: make(chan struct{})}
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
			timing.enter(stageRuntime, time.Now())
			p, reused, err := d.acquire(prepare, session.Target.ProjectID, config, true)
			a.project = p
			if err != nil {
				a.prepareErr = err
				return
			}
			// Owning a workspace (or failing) ends the pin; until then idle reclaim must wait.
			defer p.unpin()
			if reused {
				timing.reused()
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
			timing.enter(stageWorkspace, time.Now())
			a.checkout, a.prepareErr = p.prepareCheckout(prepare, scope, session.WorkspaceID, config, caller, timing)
			if a.prepareErr != nil {
				return
			}
			// The install overlaps the clone; this stage is only what remains of it.
			timing.enter(stagePlugin, time.Now())
			if a.prepareErr = installed.wait(prepare, p); a.prepareErr != nil {
				return
			}
			if session.Skills != nil {
				if err := p.applyWorkflowSkills(a.checkout.path, session.WorkspaceID, *session.Skills); err != nil {
					a.prepareErr = err
					return
				}
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
				a.project.usedAt = a.project.driver.clock()
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
	snapshot := historyOf(observation.Snapshot)
	a.mu.Lock()
	timing := a.timing
	a.mu.Unlock()
	if timing != nil {
		timing.observeHistory(snapshot)
	}
	if observation.Preview {
		a.queuePreview(snapshot)
		return nil, true
	}
	turnID := observation.TurnID
	if turnID == "" && call != nil {
		turnID = call.id
	}
	// A full snapshot carries the current pending records too and supersedes any queued preview.
	a.publishMu.Lock()
	a.mu.Lock()
	a.preview = nil
	a.mu.Unlock()
	err = a.publish(execution.Event{Ref: execution.Ref{TurnID: turnID}, Kind: execution.EventHistorySnapshot, History: snapshot})
	if err == nil {
		a.mu.Lock()
		a.previewSent = pendingKey(snapshot)
		a.mu.Unlock()
	}
	a.publishMu.Unlock()
	if err != nil {
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

func historyOf(projected nodehistory.Snapshot) *execution.HistorySnapshot {
	snapshot := &execution.HistorySnapshot{Incomplete: projected.Incomplete}
	for _, record := range projected.Records {
		snapshot.Records = append(snapshot.Records, execution.HistoryRecord{Sequence: record.Sequence, Data: record.Data, Preview: record.Preview})
	}
	return snapshot
}

// pendingKey identifies the pending part of a snapshot, so an unchanged preview is not retained.
func pendingKey(snapshot *execution.HistorySnapshot) []byte {
	pending := execution.HistorySnapshot{Incomplete: snapshot.Incomplete}
	for _, record := range snapshot.Records {
		if record.Preview {
			pending.Records = append(pending.Records, record)
		}
	}
	key, _ := json.Marshal(pending)
	return key
}

// queuePreview keeps only the newest pending set and schedules one flush per previewInterval;
// the trailing flush still publishes the last preview before a pause, such as a running tool.
func (a *agentSession) queuePreview(pending *execution.HistorySnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.emit == nil || a.ended {
		return
	}
	a.preview = pending
	if a.previewQueued {
		return
	}
	a.previewQueued = true
	time.AfterFunc(time.Until(a.previewAt.Add(previewInterval)), a.flushPreview)
}

// flushPreview publishes the queued pending set. Previews are best effort and never ACKed or
// treated as an answer: a lost one is replaced by the next preview or by settled history.
func (a *agentSession) flushPreview() {
	a.publishMu.Lock()
	defer a.publishMu.Unlock()
	a.mu.Lock()
	preview, emit, call := a.preview, a.emit, a.active
	a.preview, a.previewQueued, a.previewAt = nil, false, time.Now()
	if preview == nil || a.broken || a.ended || bytes.Equal(pendingKey(preview), a.previewSent) {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	ref := execution.Ref{SessionID: a.frozen.ID}
	if call != nil {
		ref.TurnID = call.id
	}
	ctx, cancel := context.WithTimeout(a.project.ctx, 10*time.Second)
	defer cancel()
	if emit(ctx, execution.Event{Ref: ref, OccurredAt: time.Now().UTC(), Kind: execution.EventHistoryPreview, History: preview}) == nil {
		a.mu.Lock()
		a.previewSent = pendingKey(preview)
		a.mu.Unlock()
	}
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
