// Package executionruntime wires v2's single-process execution service to PostgreSQL and
// model/Agent drivers. A process-local mutex owns admission; there is no distributed scheduler.
package executionruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionwait"
	"github.com/wanglongan587/cloud/v2-server/internal/modelturn"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/strictjson"
)

// AgentDriver owns project Nodes and their physical Agent sessions. Run may publish the
// terminal result through emit before returning, so durable Node events can be ACKed afterward.
type AgentDriver interface {
	Run(context.Context, execution.Scope, execution.Session, execution.Turn, func(context.Context, execution.Event) error) (execution.Result, error)
	End(context.Context, string) error
	// Rebuild marks the Node serving a session for replacement after End could not confirm
	// its stop. The project's next session, or a later End, stops the old process tree first.
	Rebuild(string)
	// Reattach makes a workflow workspace's checkout, recorded by an earlier process, the one its
	// sessions start on, so a restarted server continues a run in its own directory.
	Reattach(workspaceID, cloneExecution, path string)
	Close(context.Context) error
	// StopTurn asks to stop only the running turn ref, keeping its session, as Cancel wants. It
	// reports false when the session cannot be kept through a stop; Cancel then ends it. Run
	// returns the turn's result, with Result.SessionKept when the session outlived the stop.
	StopTurn(ref execution.Ref) bool
}

// ModelChoice separates the public immutable selection from the private provider binding.
type ModelChoice struct {
	Model   execution.Model
	Binding nodeservices.Binding
}

// Config is trusted startup configuration. Agent may be nil for model-only operation.
type Config struct {
	Store    *store.Store
	Models   map[string]ModelChoice
	Agent    AgentDriver
	Delivery DeliveryFilesystem
}

// pending is a turn result whose write still failed after retries. The service keeps storing it
// until the database takes it, so a recovered database records the real outcome and observers
// waiting on the durable turn end are released without a restart.
type pending struct {
	ref    execution.Ref
	result execution.Result
}

// Limits of the per-session failure handling. Each Service copies them at construction, so a
// test shortens its own instance without touching another's.
var (
	// How long one turn retries its result write before only its own session fails: a short
	// database outage, not an indefinite wait.
	defaultFinishRetryDelays = []time.Duration{time.Second, 2 * time.Second}
	// The pace of the writes that continue after a worker's own retries are exhausted.
	defaultConcludeInterval = 5 * time.Second
	// The service's own budget for one physical stop. Callers may wait less, and a caller
	// without a deadline never waits longer.
	defaultStopTimeout = 30 * time.Second
)

// stopCall is one shared attempt to confirm a session's physical stop.
type stopCall struct {
	done chan struct{}
	err  error
}

type worker struct {
	terminalMu sync.Mutex
	staged     bool
	cancel     context.CancelFunc
	done       chan struct{}
	ref        execution.Ref
	scope      execution.Scope
	group      string
	err        error
}

// Service owns all execution workers. Close must finish before the database is closed.
type Service struct {
	db         *store.Store
	models     map[string]ModelChoice
	agent      AgentDriver
	delivery   DeliveryFilesystem
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	workers    map[string]*worker
	groups     map[string]string
	workspaces map[string]string
	// held lists the live LifetimeHeld sessions: those this process started, and those recovery
	// kept from the previous process.
	held map[string]bool
	// recovering lists the held sessions recovery kept whose Agent was lost with the previous
	// process and has not been started again; their next turn starts it (see recovery).
	recovering map[string]bool
	// clones are the clone executions the held sessions recovery kept are bound to (see
	// store.RecoveredSession), so a session whose checkout was replaced is not continued on the
	// replacement. Only New writes it, before any turn runs, so readers need no lock.
	clones            map[string]string
	ending            map[string]bool
	unsaved           map[string]pending
	stops             map[string]*stopCall
	stopping          sync.WaitGroup
	joined            bool
	concluding        sync.WaitGroup
	finishRetryDelays []time.Duration
	concludeInterval  time.Duration
	stopTimeout       time.Duration
	readRetry         executionwait.Policy
	readDeliveryWork  func(context.Context, execution.Ref) (store.DeliveryWork, error)
	commitDelivery    func(context.Context, store.DeliveryWork, store.DeliveryRecord) (store.DeliveryRecord, error)
	finishTurn        func(context.Context, execution.Scope, execution.Ref, execution.Result) (execution.Result, error)
	// observerReadFailed, when set by a test before admission, observes actual
	// failed SQL reads so an outage need not be paced by sleeps.
	observerReadFailed func()
	closed             bool
}

var _ execution.Service = (*Service)(nil)
var _ execution.Catalog = (*Service)(nil)

// New reconciles persisted executions and reserves the workspaces of surviving held sessions. Models may be empty when the
// deployment has no real provider, leaving the catalog empty until one is configured. Node
// process recovery belongs to AgentDriver initialization; callers must construct that driver
// before exposing this service.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, execution.ErrUnavailable
	}
	models := make(map[string]ModelChoice, len(cfg.Models))
	for id, choice := range cfg.Models {
		if id == "" || choice.Model.ID != id || choice.Model.Name == "" || choice.Model.BindingVersion == "" || choice.Binding.Provider == nil || (choice.Binding.Protocol != nodeservices.OpenAICompletions && choice.Binding.Protocol != nodeservices.AnthropicMessages) {
			return nil, execution.ErrInvalidRequest
		}
		models[id] = choice
	}
	recoverExecution := cfg.Store.RecoverExecutions
	if cfg.Delivery != nil {
		recoverExecution = cfg.Store.RecoverExecutionsWithDeliveries
	}
	kept, err := recoverExecution(ctx)
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	s := &Service{db: cfg.Store, models: models, agent: cfg.Agent, ctx: owned, cancel: cancel, delivery: cfg.Delivery, finishTurn: cfg.Store.FinishExecutionTurn,
		readDeliveryWork: cfg.Store.DeliveryWork, commitDelivery: cfg.Store.CommitTurnDelivery,
		finishRetryDelays: defaultFinishRetryDelays, concludeInterval: defaultConcludeInterval, stopTimeout: defaultStopTimeout,
		workers: map[string]*worker{}, groups: map[string]string{}, workspaces: map[string]string{}, held: map[string]bool{}, recovering: map[string]bool{}, clones: map[string]string{}, ending: map[string]bool{}, unsaved: map[string]pending{}, stops: map[string]*stopCall{}}
	for _, r := range kept {
		// The kept session is still held; its Agent went with the old process, so its next turn
		// starts a new one (see recovery).
		s.held[r.Session.ID] = true
		s.recovering[r.Session.ID] = true
		if r.CloneExecution != "" {
			s.clones[r.Session.ID] = r.CloneExecution
		}
	}
	// Every parked node's run keeps its directory, also when the node's own session ended before
	// the restart and it can only be completed: the run's later sessions, a successor of a node
	// completed right after the restart included, share the workspace (which only this process's
	// map remembers) and start on the recorded checkout with its edits. So does every other run
	// still running and not a task's (see store.RunningWorkflowSessions), which the sweep may
	// absorb and resume (an interrupted Iteration, a pending LoopExit cleanup) with no parked node.
	// A running run the sweep fails instead keeps the mapping too: resuming it from the failure
	// continues on its checkout, as it does without a restart.
	parked, err := cfg.Store.ParkedWorkflowSessions(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	running, err := cfg.Store.RunningWorkflowSessions(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	reattached := map[string]bool{}
	for _, r := range append(parked, running...) {
		if r.Caller.Kind != execution.CallerWorkflow {
			continue
		}
		// A parked node's workspace comes first; every workspace listed reattaches its checkout once.
		key := groupKey(r.Scope, r.Caller) + "/" + r.Session.Target.ProjectID
		if _, mapped := s.workspaces[key]; !mapped {
			s.workspaces[key] = r.Session.WorkspaceID
		}
		if s.agent == nil || reattached[r.Session.WorkspaceID] {
			continue
		}
		reattached[r.Session.WorkspaceID] = true
		at, err := cfg.Store.ExecutionWorkspace(ctx, r.Session.WorkspaceID)
		if errors.Is(err, store.ErrNotFound) {
			continue // the recovered session's turn reports it (see recovery)
		}
		if err != nil {
			cancel()
			return nil, err
		}
		// A checkout that is gone is left out, and the recovered session's turn reports it; any
		// other doubt reattaches, so the recovered Agent never lands in a fresh clone.
		if _, err := os.Stat(at.Path); !errors.Is(err, fs.ErrNotExist) {
			s.agent.Reattach(r.Session.WorkspaceID, at.CloneExecution, at.Path)
		}
	}
	return s, nil
}

// Start validates the project and freezes a binding before recording the first Agent turn.
func (s *Service) Start(ctx context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
	// Own the nested slices before admission; callers cannot mutate the retained snapshot.
	if req.Skills != nil {
		if req.Caller.Kind != execution.CallerWorkflow {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
		owned := *req.Skills
		owned.Documents = slices.Clone(owned.Documents)
		owned.Plugins = slices.Clone(owned.Plugins)
		req.Skills = &owned
	}
	switch req.Lifetime {
	case execution.LifetimeTurn:
	case execution.LifetimeHeld:
		if req.Caller.Kind != execution.CallerWorkflow {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
	default:
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if req.MCP != nil && req.Caller.Kind != execution.CallerWorkflow {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if req.Host != nil && req.Lifetime != execution.LifetimeHeld {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if req.Release != nil && req.Caller.Kind != execution.CallerWorkflow {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	if err := req.MCP.Validate(); err != nil {
		return execution.Ref{}, err
	}
	// Drivers without a lifecycle configuration source cannot consume admitted targets.
	// The real driver checks the connected Node's capabilities before any Agent preparation.
	if req.MCP != nil && len(req.MCP.Bindings) != 0 {
		live, ok := s.agent.(interface{ SupportsMCPLifecycle() bool })
		if !ok || !live.SupportsMCPLifecycle() {
			return execution.Ref{}, &execution.MCPFailure{Code: "mcp_lifecycle_not_ready"}
		}
	}
	req.MCP = req.MCP.Clone()
	return s.start(ctx, scope, req.Caller, req.Prompt, &req.Target, req.ExecutorID, req.AgentID, req.ModelID, req.Skills, req.MCP, req.Lifetime, req.Host, req.Release)
}

// StartModel starts an independent model conversation without creating a directory or Node.
func (s *Service) StartModel(ctx context.Context, scope execution.Scope, req execution.ModelStartRequest) (execution.Ref, error) {
	return s.start(ctx, scope, req.Caller, req.Prompt, nil, "", "", req.ModelID, nil, nil, execution.LifetimeTurn, nil, nil)
}

func (s *Service) start(ctx context.Context, scope execution.Scope, caller execution.Caller, prompt execution.Prompt, target *execution.Target, executorID, agentID, modelID string, skills *execution.Skills, mcp *execution.MCPSelection, lifetime execution.Lifetime, host execution.HostTools, release <-chan struct{}) (execution.Ref, error) {
	if err := validate(caller, prompt); err != nil {
		return execution.Ref{}, err
	}
	if target != nil {
		if target.ProjectID == "" {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
		if executorID == "" && (agentID == "" || modelID == "") {
			return execution.Ref{}, execution.ErrInvalidRequest
		}
	} else if executorID != "" || agentID != "" {
		return execution.Ref{}, execution.ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return execution.Ref{}, execution.ErrUnavailable
	}
	// Admission is where execution starts on the user's behalf, so it applies tenant authorization;
	// the store repeats it inside the transaction that records the session or turn.
	if _, err := s.db.TenantAccess(ctx, scope.TenantID, scope.UserID); err != nil {
		return execution.Ref{}, publicError(err)
	}
	// Retained attempts remain readable even when the original runtime has ended.
	if old, err := s.db.ReadExecutionAttempt(ctx, scope, caller); err == nil {
		session, e := s.db.ReadExecutionSession(ctx, scope, old.Ref.SessionID)
		if e != nil {
			return execution.Ref{}, publicError(e)
		}
		if !session.MCP.SameMembers(mcp) || !reflect.DeepEqual(session.Skills, skills) || !sameJSON(old.Prompt, prompt) || !reflect.DeepEqual(session.Target, target) || (target == nil && session.Model.ID != modelID) || (target != nil && !sameAgentSelection(session.Executor, executorID, agentID, modelID)) {
			return execution.Ref{}, execution.ErrConflict
		}
		return old.Ref, nil
	} else if !errors.Is(err, execution.ErrNotFound) {
		return execution.Ref{}, publicError(err)
	}
	sessionID := newID()
	group := turnGroup(scope, caller, sessionID)
	if s.groups[group] != "" {
		return execution.Ref{}, execution.ErrWorkspaceBusy
	}
	var executor *execution.Executor
	if target != nil {
		if s.agent == nil {
			return execution.Ref{}, execution.ErrUnavailable
		}
		if _, err := s.db.GetProject(ctx, scope, target.ProjectID); err != nil {
			return execution.Ref{}, publicError(err)
		}
		var ok bool
		executor, ok = s.resolveExecutor(executorID, agentID, modelID)
		if !ok {
			return execution.Ref{}, execution.ErrNotFound
		}
		modelID = executor.Model.ID
	}
	choice, ok := s.models[modelID]
	// A direct model turn speaks OpenAI chat completions; Messages bindings serve Agents only.
	if !ok || (target == nil && choice.Binding.Protocol != nodeservices.OpenAICompletions) {
		return execution.Ref{}, execution.ErrNotFound
	}
	session := execution.Session{MCP: mcp, Skills: skills, ID: sessionID, Target: target, Executor: executor, Model: choice.Model, CreatedAt: time.Now().UTC()}
	if target != nil {
		session.WorkspaceID = newID()
		if caller.Kind == execution.CallerWorkflow {
			key := groupKey(scope, caller) + "/" + target.ProjectID
			if id := s.workspaces[key]; id != "" {
				session.WorkspaceID = id
			} else {
				s.workspaces[key] = session.WorkspaceID
			}
			// The run's sessions share this directory and stay live side by side: a held session
			// (an interactive node waiting for its starter) is not ended when the next node
			// starts. The Node admits several live sessions per checkout (its
			// shared_checkout_sessions capability, which the Agent driver requires) and refuses
			// only the session over its own limit, without touching the others. Each session
			// takes one turn at a time; the run's sessions run theirs concurrently (see turnGroup).
		}
	}
	turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: newID()}, Caller: caller, Prompt: copyPrompt(prompt), StartedAt: time.Now().UTC()}
	turn, err := s.db.AdmitExecutionSession(ctx, scope, session, turn, s.delivery != nil)
	if err != nil {
		return execution.Ref{}, publicError(err)
	}
	if lifetime == execution.LifetimeHeld {
		s.held[session.ID] = true
	}
	turn.Host = host
	s.launch(scope, session, turn, group, release)
	return turn.Ref, nil
}

// resolveExecutor accepts either the legacy combined binding ID or the independent Agent and
// model IDs. Mixed forms must describe the same binding; display names never select execution.
func (s *Service) resolveExecutor(executorID, agentID, modelID string) (*execution.Executor, bool) {
	if executorID == "" && (agentID == "" || modelID == "") {
		return nil, false
	}
	for id, choice := range s.models {
		for _, a := range agentExecutors {
			e, ok := executorFor(a, choice)
			if !ok {
				continue
			}
			if executorID != "" && e.ID != executorID {
				continue
			}
			if executorID == "" && (e.AgentID != agentID || id != modelID) {
				continue
			}
			if executorID != "" && ((agentID != "" && e.AgentID != agentID) || (modelID != "" && id != modelID)) {
				continue
			}
			copy := e
			return &copy, true
		}
	}
	return nil, false
}

// sameAgentSelection compares an existing frozen executor snapshot with either the legacy
// combined ID or the independent Agent/model IDs without consulting the mutable catalog.
func sameAgentSelection(e *execution.Executor, executorID, agentID, modelID string) bool {
	if e == nil || (executorID == "" && agentID == "" && modelID == "") {
		return false
	}
	if executorID != "" && e.ID != executorID {
		return false
	}
	if agentID != "" && e.AgentID != agentID {
		return false
	}
	if modelID != "" && e.Model.ID != modelID {
		return false
	}
	return true
}

// Append continues a live logical session. A durable terminal event may precede driver cleanup;
// the next turn waits for that worker without releasing its directory or requiring another event.
// A server restart never reconstructs old workers.
func (s *Service) Append(ctx context.Context, scope execution.Scope, req execution.AppendRequest) (execution.Ref, error) {
	if err := validate(req.Caller, req.Prompt); err != nil {
		return execution.Ref{}, err
	}
	// A new turn starts execution on the user's behalf, so it applies tenant authorization like
	// start; the store repeats it inside the transaction that records the turn.
	if _, err := s.db.TenantAccess(ctx, scope.TenantID, scope.UserID); err != nil {
		return execution.Ref{}, publicError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed || s.ctx.Err() != nil {
			return execution.Ref{}, execution.ErrUnavailable
		}
		session, err := s.db.ReadExecutionSession(ctx, scope, req.SessionID)
		if err != nil {
			return execution.Ref{}, publicError(err)
		}
		if old, e := s.db.ReadExecutionAttempt(ctx, scope, req.Caller); e == nil {
			if old.Ref.SessionID != req.SessionID || !sameJSON(old.Prompt, req.Prompt) {
				return execution.Ref{}, execution.ErrConflict
			}
			return old.Ref, nil
		} else if !errors.Is(e, execution.ErrNotFound) {
			return execution.Ref{}, publicError(e)
		}
		if session.EndedAt != nil || s.ending[session.ID] {
			return execution.Ref{}, execution.ErrSessionEnded
		}
		first, err := s.db.ReadExecutionTurns(ctx, scope, session.ID, "", 1)
		if err != nil {
			return execution.Ref{}, publicError(err)
		}
		if len(first.Items) != 1 || first.Items[0].Caller.Kind != req.Caller.Kind || first.Items[0].Caller.ID != req.Caller.ID {
			return execution.Ref{}, execution.ErrConflict
		}
		group := turnGroup(scope, req.Caller, session.ID)
		if active := s.groups[group]; active != "" {
			prior := s.workers[active]
			if prior == nil {
				return execution.Ref{}, execution.ErrWorkspaceBusy
			}
			retained, err := s.readTurn(ctx, scope, prior.ref)
			if err != nil {
				return execution.Ref{}, err
			}
			if retained.Result == nil {
				return execution.Ref{}, execution.ErrWorkspaceBusy
			}
			// Publication precedes the Node ACK and worker cleanup. Keep ownership
			// intact, but do not tell an observer of that terminal event to wait for
			// a second event that will never arrive. Cleanup itself needs this mutex.
			s.mu.Unlock()
			var waitErr error
			select {
			case <-ctx.Done():
				waitErr = ctx.Err()
			case <-s.ctx.Done():
				waitErr = execution.ErrUnavailable
			case <-prior.done:
			}
			s.mu.Lock()
			if waitErr != nil {
				return execution.Ref{}, waitErr
			}
			// Another Append, End, Close or permission change may have won while
			// unlocked. Recheck admission and attempt identity before writing anything.
			continue
		}
		turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: newID()}, Caller: req.Caller, Prompt: copyPrompt(req.Prompt), StartedAt: time.Now().UTC()}
		turn, err = s.db.AdmitExecutionTurn(ctx, scope, session.ID, turn, s.delivery != nil)
		if err != nil {
			return execution.Ref{}, publicError(err)
		}
		turn.Host = req.Host
		s.launch(scope, session, turn, group, nil)
		return turn.Ref, nil
	}
}

// CheckHostTools reports whether sessions of the project can be given host tools, starting the
// project's Node if it is not running: its capabilities are known only once it is. A deployment
// without an Agent runtime never can, which retrying does not change; a Node that could not be
// started says nothing about it.
func (s *Service) CheckHostTools(ctx context.Context, scope execution.Scope, projectID string) error {
	if s.agent == nil {
		return execution.ErrHostMCPUnavailable
	}
	driver, ok := s.agent.(interface {
		CheckHostTools(context.Context, execution.Scope, string) error
	})
	if !ok {
		return execution.ErrHostMCPUnavailable
	}
	return driver.CheckHostTools(ctx, scope, projectID)
}

// HostExecutionEnding reports whether the driver already decided why the host execution of the
// session ended, which may be stored only later.
func (s *Service) HostExecutionEnding(sid, executionID string) bool {
	driver, ok := s.agent.(interface {
		HostExecutionEnding(sid, executionID string) bool
	})
	return ok && driver.HostExecutionEnding(sid, executionID)
}

// launch is called under mu; workers use the backend's lifetime, never an HTTP request context.
func (s *Service) launch(scope execution.Scope, session execution.Session, turn execution.Turn, group string, release <-chan struct{}) {
	ctx, cancel := context.WithCancel(s.ctx)
	w := &worker{cancel: cancel, done: make(chan struct{}), ref: turn.Ref, scope: scope, group: group}
	held := s.held[session.ID]
	s.workers[turn.Ref.TurnID] = w
	s.groups[group] = turn.Ref.TurnID
	go func() {
		defer cancel()
		var finalErr error
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			w.err = finalErr
			delete(s.groups, group)
			delete(s.workers, turn.Ref.TurnID)
			close(w.done)
		}()
		emit := func(_ context.Context, event execution.Event) error {
			write, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			event.Ref = turn.Ref
			if event.Kind == execution.EventTurnEnded {
				if event.Result == nil {
					return execution.ErrInvalidRequest
				}
				if session.Target != nil {
					_, err := s.stageDelivery(write, w, *event.Result)
					return err
				}
				_, err := s.db.FinishExecutionTurn(write, scope, turn.Ref, *event.Result)
				return err
			}
			_, err := s.db.AppendExecutionEvent(write, scope, event)
			return err
		}
		var result execution.Result
		var agentErr error
		var recovered, nodeLost bool
		lifecycle := turn.MCPGeneration != nil || session.MCP != nil && len(session.MCP.Bindings) != 0
		if session.Target != nil {
			// A first turn the caller holds back (StartRequest.Release) reaches no Agent until it
			// is let through; the caller abandoning it, an End, a stop or shutdown ends it as never
			// started.
			released := awaitRelease(ctx, release)
			for attempt := 0; ; attempt++ {
				s.mu.Lock()
				recovered = s.recovering[session.ID]
				s.mu.Unlock()
				var gone bool
				session.Recovery, agentErr = nil, nil
				if recovered {
					session.Recovery, gone, agentErr = s.recovery(ctx, session, turn)
				}
				switch {
				case !released:
					result = execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled", NotStarted: true}
				case gone:
					// The checkout the Agent worked in no longer exists: nothing can continue it.
					result = failedResult(ctx, agentErr)
					result.FailureCode, result.NotStarted = execution.FailureWorkspaceMissing, true
				case agentErr != nil:
					result = failedResult(ctx, agentErr)
					result.NotStarted = true
				default:
					result, agentErr = s.agent.Run(ctx, scope, session, turn, emit)
					if agentErr != nil && result.Status == "" {
						result = failedResult(ctx, agentErr)
					}
				}
				if recovered && !result.NotStarted {
					s.mu.Lock()
					delete(s.recovering, session.ID) // the new Agent now holds the conversation
					s.mu.Unlock()
				}
				// A held session outlives its Node: like a restart, the Agent's loss leaves the
				// conversation recorded, and a new Agent continues it on the same checkout.
				nodeLost = held && !lifecycle && result.FailureCode == execution.FailureNodeLost && !s.firstTurn(ctx, scope, turn)
				if !nodeLost {
					break
				}
				s.mu.Lock()
				s.recovering[session.ID] = true
				s.mu.Unlock()
				if attempt > 0 || !errors.Is(agentErr, execution.ErrNodeLost) || ctx.Err() != nil {
					break // the turn reached the lost Agent, or its replacement was lost too
				}
				// The Node was gone before this turn reached the Agent: nothing ran, so its
				// replacement takes the turn now.
			}
		} else {
			messages, err := s.messages(ctx, scope, turn)
			if err != nil {
				result = failedResult(ctx, err)
			} else {
				result, _ = modelturn.Run(ctx, s.models[session.Model.ID].Binding, messages, func(c context.Context, delta string) error {
					return emit(c, execution.Event{Kind: execution.EventTextDelta, TextDelta: delta})
				})
			}
		}
		if result.Status == "" {
			result = failedResult(ctx, execution.ErrUnavailable)
		}
		// A turn that never started (its recovery or its new Agent could not be set up) never
		// reached an Agent: the driver marks the turns it refused itself, this the rest.
		if result.NotStarted {
			result.NotDelivered = true
		}
		// Read before delivery and persistence, which return the stored result without it.
		kept := result.SessionKept && agentErr == nil
		// A recovered session whose new Agent could not be started keeps waiting for another
		// turn: nothing ran, and the conversation is still recorded. Only a missing checkout
		// ends it.
		canRecover := result.FailureCode != execution.FailureWorkspaceMissing && !(lifecycle && result.FailureCode == execution.FailureNodeLost)
		retry := nodeLost || recovered && result.NotStarted && canRecover
		// An Agent error means the session is gone; the driver marks the results it publishes
		// itself, and this marks the ones only the runtime writes.
		result.AgentLost = !retry && (result.AgentLost || agentErr != nil)
		var err error
		var finalized bool
		if s.delivery != nil && session.Target != nil {
			result, finalized, err = s.finishDelivery(w, result, agentErr)
			if err != nil && s.ctx.Err() != nil {
				finalErr = err
				return // Keep the durable obligation for boot after shutdown.
			}
			if err != nil {
				result, err = s.finishUnavailableDelivery(w, result, err)
				finalized = err == nil
				if err != nil {
					// A delivery record could not be stored. The ordinary failure path still
					// concludes execution and releases its task to pending.
					agentErr = errors.Join(agentErr, err)
					retry = false
					result.AgentLost = true
				}
			}
		}
		if !finalized {
			if session.Target != nil {
				// Run has returned. A Stop retry must not end a session after the
				// terminal has been handed to the ordinary persistence path.
				w.terminalMu.Lock()
				w.staged = true
				w.terminalMu.Unlock()
			}
			result, err = s.finish(scope, turn.Ref, result)
		}
		// Failures stay with this session: nothing here stops admission for other sessions.
		// A held session survives a turn the Agent itself ended without completing: the driver
		// reports no error then, and the Agent is still there to take the next turn. So does any
		// session whose turn the caller stopped when the driver kept it (Result.SessionKept).
		broken := session.Target != nil && !retry && (agentErr != nil || (result.Status != execution.StatusCompleted && !held && !kept))
		if nodeLost && err == nil {
			// The session stays open for its next turn, but the lost Node's process tree is
			// retired now, with the supervisor's proof, rather than when the project is next used.
			// An unconfirmed stop needs nothing more here: stopAgent marks the Node for rebuild,
			// and the project's next use retires it before starting a new incarnation.
			_ = s.stopAgent(context.Background(), session.ID)
		}
		if err != nil || broken {
			// An unstored result or a failed/cancelled physical Agent is never advertised as
			// resumable, held or not. Keep admission occupied until stop is confirmed.
			s.mu.Lock()
			s.ending[session.ID] = true
			if err != nil {
				// A held session's caller reads AgentLost to learn that no turn can follow, and
				// a result that could not be stored ends the session as a lost Agent does.
				result.AgentLost = result.AgentLost || held
				s.unsaved[session.ID] = pending{ref: turn.Ref, result: result}
			}
			s.mu.Unlock()
			stopped := true
			if session.Target != nil && s.stopAgent(context.Background(), session.ID) != nil {
				stopped = false
				err = errors.Join(err, execution.ErrStopUnconfirmed)
			}
			write, stop := context.WithTimeout(context.Background(), 10*time.Second)
			concludeErr := s.conclude(write, scope, session.ID, stopped)
			stop()
			if concludeErr != nil {
				// Callers wait on the durable turn end, so the write must not depend on
				// anyone calling End: keep trying until the database takes it.
				err = errors.Join(err, concludeErr)
				s.concludeLater(scope, session.ID, stopped)
			}
		}
		finalErr = err
	}()
}

// firstTurn reports whether turn opened its session. A first turn lost with its Node is not kept
// for a new Agent: nothing of it was recorded to hand over, so it fails as a session that could
// not be set up. An unreadable answer counts as a first turn, the safe side.
func (s *Service) firstTurn(ctx context.Context, scope execution.Scope, turn execution.Turn) bool {
	page, err := s.db.ReadExecutionTurns(context.WithoutCancel(ctx), scope, turn.Ref.SessionID, "", 1)
	return err != nil || len(page.Items) == 0 || page.Items[0].Ref == turn.Ref
}

// finish retries a short database outage and returns the result it tried to store. A result
// the store rejects as invalid can never be written as it is, so it is stored as a failed turn
// rather than left running; everything else the turn produced is kept, AgentLost and NotStarted
// included.
func (s *Service) finish(scope execution.Scope, ref execution.Ref, result execution.Result) (execution.Result, error) {
	for attempt := 0; ; attempt++ {
		write, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.finishTurn(write, scope, ref, result)
		stop()
		if errors.Is(err, execution.ErrInvalidRequest) && result.Status != execution.StatusFailed {
			result.Status, result.FailureCode = execution.StatusFailed, "execution_failed"
			attempt--
			continue
		}
		if err == nil || attempt >= len(s.finishRetryDelays) {
			return result, err
		}
		select {
		case <-s.ctx.Done():
			return result, err
		case <-time.After(s.finishRetryDelays[attempt]):
		}
	}
}

// conclude stores the result a session's worker could not write and, once its physical stop is
// confirmed, ends the session. Both writes are idempotent, so the worker, its background retry
// and End may race. It never holds mu across database I/O.
func (s *Service) conclude(ctx context.Context, scope execution.Scope, sid string, stopped bool) error {
	s.mu.Lock()
	p, unsaved := s.unsaved[sid]
	s.mu.Unlock()
	if unsaved {
		if _, err := s.finishTurn(ctx, scope, p.ref, p.result); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.unsaved, sid)
		s.mu.Unlock()
	}
	if !stopped {
		// The Node is marked for rebuild; only an End that obtains stop proof ends the session.
		return nil
	}
	var ends map[string]string
	if driver, ok := s.agent.(interface {
		HostExecutionEnds(string) map[string]string
	}); ok {
		ends = driver.HostExecutionEnds(sid)
	}
	if err := s.db.EndHostExecutionSession(ctx, scope, sid, ends); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.held, sid)
	delete(s.recovering, sid)
	s.mu.Unlock()
	return nil
}

// concludeLater repeats conclude after the worker has gone, until it succeeds, the store rejects
// the write for good (the session no longer exists or the request is invalid), or the service
// closes. Only workers call it, so Close can join every retry.
func (s *Service) concludeLater(scope execution.Scope, sid string, stopped bool) {
	s.concluding.Add(1)
	go func() {
		defer s.concluding.Done()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(s.concludeInterval):
			}
			write, stop := context.WithTimeout(s.ctx, 10*time.Second)
			err := s.conclude(write, scope, sid, stopped)
			stop()
			if err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, execution.ErrNotFound) || errors.Is(err, execution.ErrInvalidRequest) {
				return
			}
		}
	}()
}

// stopAgent confirms a session's physical stop within the service's own budget. Concurrent
// callers share one attempt and may stop waiting when their context ends; the attempt itself
// continues. So a caller that gives up early never marks a healthy Node, a caller without a
// deadline never waits forever, and a stop the Node does not confirm within the budget marks
// only that project's Node for rebuild. Close joins every attempt, and none starts once it has.
// The caller must not hold mu.
//
// Without an Agent runtime there is no Agent to stop, and it returns at once without End: this
// process started none, and its start already stopped every Node of earlier processes, or did not
// start at all when it could not (agentruntime.Recover in main). A session that execution recovery
// kept for a parked node (store.RecoverExecutions) is then only ended in the store.
func (s *Service) stopAgent(ctx context.Context, sid string) error {
	if s.agent == nil {
		return nil
	}
	s.mu.Lock()
	call := s.stops[sid]
	if call == nil {
		if s.joined {
			// Close has already stopped every Agent through the driver.
			s.mu.Unlock()
			return execution.ErrStopUnconfirmed
		}
		call = &stopCall{done: make(chan struct{})}
		s.stops[sid] = call
		s.stopping.Add(1)
		go func() {
			defer s.stopping.Done()
			budget, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
			call.err = s.agent.End(budget, sid)
			cancel()
			if call.err != nil {
				// Only this project's Node is untrusted; its next session or End retires it.
				s.agent.Rebuild(sid)
			}
			s.mu.Lock()
			delete(s.stops, sid)
			s.mu.Unlock()
			close(call.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-call.done:
		if call.err != nil {
			return execution.ErrStopUnconfirmed
		}
		return nil
	case <-ctx.Done():
		return execution.ErrStopUnconfirmed
	}
}

func (s *Service) messages(ctx context.Context, scope execution.Scope, current execution.Turn) ([]modelturn.Message, error) {
	var messages []modelturn.Message
	cursor := ""
	for {
		page, err := s.db.ReadExecutionTurns(ctx, scope, current.Ref.SessionID, cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, turn := range page.Items {
			text, err := turn.Prompt.Text()
			if err != nil {
				return nil, err
			}
			messages = append(messages, modelturn.Message{Role: "user", Content: text})
			if turn.Ref.TurnID == current.Ref.TurnID {
				return messages, nil
			}
			if turn.Result != nil {
				messages = append(messages, modelturn.Message{Role: "assistant", Content: turn.Result.Answer})
			}
		}
		if page.NextCursor == "" {
			return nil, execution.ErrNotFound
		}
		cursor = page.NextCursor
	}
}

// Cancel stops an active Agent turn through the driver's StopTurn, which cancels only the turn
// when the session's Node keeps sessions after a stop; otherwise, or when the driver cannot, the
// session ends. Its worker context stays live meanwhile, so End, Close and shutdown still end the
// session at once. Model-only cancellation stops the requested turn and leaves its conversation
// open.
func (s *Service) Cancel(ctx context.Context, scope execution.Scope, ref execution.Ref) (execution.Result, error) {
	turn, err := s.readTurn(ctx, scope, ref)
	if err != nil {
		return execution.Result{}, err
	}
	if turn.Result != nil {
		return *turn.Result, nil
	}
	session, err := s.db.ReadExecutionSession(ctx, scope, ref.SessionID)
	if err != nil {
		return execution.Result{}, publicError(err)
	}
	if session.Target != nil {
		// Serialize with Append: a late retry for an already finished turn must not
		// stop a newer turn. The worker keeps the session's group until it has gone, so no new
		// turn enters the session before it is known whether the stop kept it.
		var staged bool
		if s.delivery != nil {
			work, workErr := s.readDeliveryWork(ctx, ref)
			if workErr != nil && !errors.Is(workErr, store.ErrNotFound) {
				return execution.Result{}, publicError(workErr)
			}
			staged = workErr == nil && work.Result != nil
		}
		s.mu.Lock()
		w := s.workers[ref.TurnID]
		s.mu.Unlock()
		var stopping, turnOnly bool
		if w != nil {
			// Wait for a racing stage without holding the service mutex. A worker
			// that has already left must never cause a newer turn to be stopped.
			w.terminalMu.Lock()
			staged = staged || w.staged
			if !staged {
				s.mu.Lock()
				stopping = s.workers[ref.TurnID] == w
				s.mu.Unlock()
				// The driver takes its own locks, so it is asked outside mu; terminalMu still
				// keeps the worker from staging its terminal meanwhile.
				turnOnly = stopping && s.agent.StopTurn(ref)
				if stopping && !turnOnly {
					// Once cancellation wins, no new turn can enter this session.
					s.mu.Lock()
					if s.workers[ref.TurnID] == w {
						s.ending[ref.SessionID] = true
						w.cancel()
					}
					s.mu.Unlock()
				}
			}
			w.terminalMu.Unlock()
		}
		if turnOnly {
			select {
			case <-ctx.Done():
				return execution.Result{}, ctx.Err()
			case <-w.done:
			}
			s.mu.Lock()
			_, unsaved := s.unsaved[ref.SessionID]
			ending := s.ending[ref.SessionID] || unsaved
			s.mu.Unlock()
			if !ending {
				// The worker kept the session (or the turn ended on its own first): its stored
				// result is the answer, and the session takes the next turn.
				turn, err := s.readTurn(ctx, scope, ref)
				if err != nil {
					return execution.Result{}, err
				}
				if turn.Result == nil {
					return execution.Result{}, execution.ErrStopUnconfirmed
				}
				return *turn.Result, nil
			}
		}
		if !stopping {
			if staged && w != nil {
				select {
				case <-ctx.Done():
					return execution.Result{}, ctx.Err()
				case <-w.done:
				}
			}
			turn, err := s.readTurn(ctx, scope, ref)
			if err != nil {
				return execution.Result{}, err
			}
			if turn.Result != nil {
				return *turn.Result, nil
			}
			s.mu.Lock()
			pending, unsaved := s.unsaved[ref.SessionID]
			s.mu.Unlock()
			if !unsaved || pending.ref != ref {
				return execution.Result{}, execution.ErrStopUnconfirmed
			}
		}
		if err := s.End(ctx, scope, ref.SessionID); err != nil {
			return execution.Result{}, err
		}
		turn, err = s.readTurn(ctx, scope, ref)
		if err != nil {
			return execution.Result{}, err
		}
		if turn.Result == nil {
			return execution.Result{}, execution.ErrStopUnconfirmed
		}
		return *turn.Result, nil
	}
	s.mu.Lock()
	w := s.workers[ref.TurnID]
	if w != nil {
		w.cancel()
	}
	s.mu.Unlock()
	if w == nil {
		// Completion can win after the initial read and remove the worker. Its durable
		// result is still the authority; absence from the map is not a stop failure.
		// Only a result its worker could not write is concluded here, and a model-only session
		// has nothing physical left to stop. A turn that merely finished keeps its session open.
		s.mu.Lock()
		_, unsaved := s.unsaved[ref.SessionID]
		s.mu.Unlock()
		if unsaved {
			if err := s.conclude(ctx, scope, ref.SessionID, true); err != nil {
				return execution.Result{}, publicError(err)
			}
		}
		turn, err = s.readTurn(ctx, scope, ref)
		if err != nil {
			return execution.Result{}, err
		}
		if turn.Result != nil {
			return *turn.Result, nil
		}
		return execution.Result{}, execution.ErrStopUnconfirmed
	}
	select {
	case <-ctx.Done():
		return execution.Result{}, ctx.Err()
	case <-w.done:
	}
	turn, err = s.readTurn(ctx, scope, ref)
	if err != nil {
		return execution.Result{}, err
	}
	if turn.Result == nil {
		return execution.Result{}, execution.ErrStopUnconfirmed
	}
	return *turn.Result, nil
}

// End stops and joins the active worker before persisting the session terminal event.
func (s *Service) End(ctx context.Context, scope execution.Scope, sid string) error {
	session, err := s.db.ReadExecutionSession(ctx, scope, sid)
	if err != nil {
		return publicError(err)
	}
	if session.EndedAt != nil {
		return nil
	}
	s.mu.Lock()
	s.ending[sid] = true
	var wait []*worker
	for _, w := range s.workers {
		if w.ref.SessionID == sid {
			w.cancel()
			wait = append(wait, w)
		}
	}
	s.mu.Unlock()
	for _, w := range wait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
		}
		if w.err != nil {
			return execution.ErrStopUnconfirmed
		}
	}
	if session.Target != nil {
		if err := s.stopAgent(ctx, sid); err != nil {
			return execution.ErrStopUnconfirmed
		}
	}
	return publicError(s.conclude(ctx, scope, sid, true))
}

// Close shuts down workers and physical Agents. Persisted sessions are ended by recovery on
// the next start, so no old conversation is advertised as resumable across process restarts.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	var wait []*worker
	for _, w := range s.workers {
		wait = append(wait, w)
	}
	s.mu.Unlock()
	for _, w := range wait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.done:
		}
	}
	// Every worker has gone, so only End and Cancel could still start a stop.
	s.mu.Lock()
	s.joined = true
	s.mu.Unlock()
	var err error
	if s.agent != nil {
		err = s.agent.Close(ctx)
	}
	// Nothing of the service outlives Close: a stop attempt runs on its own budget and a
	// background result write uses the database, so both are joined here.
	settled := make(chan struct{})
	go func() {
		s.stopping.Wait()
		s.concluding.Wait()
		close(settled)
	}()
	select {
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	case <-settled:
	}
	return err
}

func (s *Service) ReadSession(ctx context.Context, scope execution.Scope, id string) (execution.Session, error) {
	v, e := s.db.ReadExecutionSession(ctx, scope, id)
	return v, publicError(e)
}
func (s *Service) ReadTurns(ctx context.Context, scope execution.Scope, id, cursor string, limit int) (execution.TurnPage, error) {
	v, e := s.db.ReadExecutionTurns(ctx, scope, id, cursor, limit)
	return v, publicError(e)
}
func (s *Service) ReadAttempt(ctx context.Context, scope execution.Scope, caller execution.Caller) (execution.Turn, error) {
	v, e := s.db.ReadExecutionAttempt(ctx, scope, caller)
	return v, publicError(e)
}
func (s *Service) readTurn(ctx context.Context, scope execution.Scope, ref execution.Ref) (execution.Turn, error) {
	cursor := ""
	for {
		p, err := s.ReadTurns(ctx, scope, ref.SessionID, cursor, 100)
		if err != nil {
			return execution.Turn{}, err
		}
		for _, t := range p.Items {
			if t.Ref == ref {
				return t, nil
			}
		}
		if p.NextCursor == "" {
			return execution.Turn{}, execution.ErrNotFound
		}
		cursor = p.NextCursor
	}
}

func (s *Service) ListModels(ctx context.Context, scope execution.Scope) ([]execution.Model, error) {
	// Consumers use the catalog as an execution-readiness gate, so a closing runtime must
	// not keep advertising selections. One session's failure never closes it for others.
	s.mu.Lock()
	unavailable := s.closed || s.ctx.Err() != nil
	s.mu.Unlock()
	if unavailable {
		return nil, execution.ErrUnavailable
	}
	items := make([]execution.Model, 0, len(s.models))
	for _, c := range s.models {
		if c.Binding.Protocol == nodeservices.OpenAICompletions {
			items = append(items, c.Model)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}
func (s *Service) ListExecutors(ctx context.Context, scope execution.Scope) ([]execution.Executor, error) {
	// ListModels supplies the scope and readiness checks; Messages bindings are not in it.
	if _, err := s.ListModels(ctx, scope); err != nil {
		return nil, err
	}
	items := []execution.Executor{}
	if s.agent == nil {
		return items, nil
	}
	ids := slices.Sorted(maps.Keys(s.models))
	// OpenCode stays first, so pages that preselect the first executor keep their default.
	for _, a := range agentExecutors {
		for _, id := range ids {
			if e, ok := executorFor(a, s.models[id]); ok {
				items = append(items, e)
			}
		}
	}
	return items, nil
}
func groupKey(scope execution.Scope, caller execution.Caller) string {
	return scope.TenantID + "/" + scope.UserID + "/" + string(caller.Kind) + "/" + caller.ID
}

// turnGroup is the admission unit that allows one active turn. A conversation, schedule or task
// caller owns one session, so its caller is the unit. A workflow run hosts concurrent Agent
// attempts, each in its own session, so each session is its own unit.
func turnGroup(scope execution.Scope, caller execution.Caller, session string) string {
	if caller.Kind == execution.CallerWorkflow {
		return "session/" + session
	}
	return groupKey(scope, caller)
}
func validate(c execution.Caller, p execution.Prompt) error {
	if c.ID == "" || c.AttemptID == "" || (c.Kind != execution.CallerChat && c.Kind != execution.CallerWorkflow && c.Kind != execution.CallerSchedule && c.Kind != execution.CallerTask) {
		return execution.ErrInvalidRequest
	}
	if c.Kind != execution.CallerWorkflow && (c.NodeID != "" || len(c.Rounds) != 0) {
		return execution.ErrInvalidRequest
	}
	for _, r := range c.Rounds {
		if r.ContainerID == "" || r.Index < 0 {
			return execution.ErrInvalidRequest
		}
	}
	size := 0
	for _, raw := range p {
		size += len(raw)
		if size > 1<<20 || !strictjson.Valid(raw) {
			return execution.ErrInvalidRequest
		}
	}
	_, err := p.Text()
	return err
}
func copyPrompt(p execution.Prompt) execution.Prompt {
	v := make(execution.Prompt, len(p))
	for i, b := range p {
		v[i] = append(json.RawMessage(nil), b...)
	}
	return v
}
func sameJSON(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var x, y any
	if json.Unmarshal(aa, &x) != nil || json.Unmarshal(bb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// awaitRelease waits for the caller's decision on a held-back turn (StartRequest.Release) and
// reports whether the turn may go on: only a value lets it through, while the channel's closing
// abandons it. A ctx that ended first, even together with the release, holds it back. A nil
// release never holds back.
func awaitRelease(ctx context.Context, release <-chan struct{}) bool {
	if release == nil {
		return true
	}
	var released bool
	select {
	case _, released = <-release:
	case <-ctx.Done():
	}
	return released && ctx.Err() == nil
}

func failedResult(ctx context.Context, err error) execution.Result {
	if errors.Is(ctx.Err(), context.Canceled) {
		return execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"}
	}
	return execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}
}
func publicError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, execution.ErrNotFound, execution.ErrInvalidRequest, execution.ErrConflict, execution.ErrWorkspaceBusy, execution.ErrSessionEnded, execution.ErrStopUnconfirmed} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		return execution.ErrNotFound
	}
	return execution.ErrUnavailable
}
