package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// NodeRun is the engine's view of one live node attempt.
type NodeRun struct {
	ID, NodeID, NodeType, Status string
	Iteration                    *int
	Output                       *string
	SessionID                    *string
	FinishedAt                   *time.Time
}

// RunContext is everything one scheduling pass reads about a run in one query.
type RunContext struct {
	RunID, WorkflowID, ProjectID, Status string
	Input                                *string
	Payload                              []byte
	Graph                                string
}

// NodeStart is one node attempt a scheduling wave starts.
type NodeStart struct {
	NodeID, NodeType string
	Input            *string
}

// Repository is the engine's persistence port, the subset of desktop's
// WorkflowRunEngineRepository the v2 scope needs. Every method is one transaction; payload
// changes are passed as pure fold functions so the store never interprets the payload.
type Repository interface {
	RunContext(ctx context.Context, runID string) (RunContext, error)
	// NodeRuns lists the live root-scope attempts of a run in creation order.
	NodeRuns(ctx context.Context, runID string) ([]NodeRun, error)
	// StartRun moves a pending run with no started nodes to running, applies prepare to its
	// payload and inserts the entry attempt. false means the run was not startable.
	StartRun(ctx context.Context, runID string, entry NodeStart, prepare func([]byte) ([]byte, error)) (bool, error)
	// StartNodes inserts one ready wave and returns the created attempts in the same order.
	StartNodes(ctx context.Context, runID string, wave []NodeStart) ([]NodeRun, error)
	// CompleteNode marks a running attempt succeeded, merges keys into its payload and applies
	// fold to the run payload. false means the attempt was no longer running (a late callback).
	CompleteNode(ctx context.Context, runID, nodeRunID string, output *string, keys map[string]any, fold func(payload []byte, node NodeRun) ([]byte, error)) (bool, error)
	// FailNode marks a running attempt failed with the detail built for its attempt number and
	// fails the run when it is still running. false means the attempt was no longer running.
	FailNode(ctx context.Context, runID, nodeRunID, message string, output *string, detail func(attempt int, payload []byte) map[string]any) (bool, error)
	// AbandonNode closes a running attempt that has no session yet as cancelled when its run is no
	// longer running, without moving the run's failure anchor. false means the run is running and
	// the attempt must execute.
	AbandonNode(ctx context.Context, runID, nodeRunID string) (bool, error)
	// FinishRun marks a running run succeeded with its final output.
	FinishRun(ctx context.Context, runID string, output *string) error
	// UpdateRunInput replaces the kickoff input of an editable run and applies fold to its payload.
	UpdateRunInput(ctx context.Context, runID string, input *string, fold func([]byte) ([]byte, error)) error
}

// AgentDispatch is everything the execution bridge needs to drive one Agent attempt.
type AgentDispatch struct {
	RunID, NodeRunID, ProjectID, Locale string
	Node                                Node
	Graph                               *Graph
	Pool                                VariablePool
	RunInput                            *string
	NodeRuns                            []NodeRun
}

// Dispatcher drives Agent attempts in the background. Dispatch must return immediately; the
// result comes back through Engine.CompleteNode or Engine.FailNode.
type Dispatcher interface {
	Dispatch(AgentDispatch)
}

// Engine drives runs from pending to a terminal state, as desktop's WorkflowRunEngine: it is
// stateless and recomputes every wave from persistence. All commands and callbacks of one run
// pass through a per-run gate.
type Engine struct {
	repo    Repository
	agents  Dispatcher
	publish func(runID string)
	now     func() time.Time
	locale  string
	gates   sync.Map
}

// NewEngine assembles the engine. publish is called after every committed transition and must
// not block; it may be nil.
func NewEngine(repo Repository, agents Dispatcher, publish func(string), now func() time.Time) *Engine {
	if publish == nil {
		publish = func(string) {}
	}
	return &Engine{repo: repo, agents: agents, publish: publish, now: now, locale: LocaleZhCN}
}

func (e *Engine) gate(runID string) func() {
	m, _ := e.gates.LoadOrStore(runID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ErrNotStartable reports a run that is not pending with no started nodes.
var ErrNotStartable = errors.New("workflow run is not startable")

// runtime is one node kind's execution policy; the scheduling core never branches on kinds.
type runtime struct {
	swift func(n Node, c swiftCompletion) (string, error)
	async bool
	// rank orders run-output candidates (lowest wins); nil never contributes.
	rank       *int
	startInput func(n Node, run RunContext) *string
}

type swiftCompletion struct {
	run      NodeRun
	runInput *string
	pool     *VariablePool
	runs     []NodeRun
}

func rank(r int) *int { return &r }

// runtimes registers the kinds v2 executes. Kinds absent here are refused at start.
var runtimes = map[string]runtime{
	"start": {
		swift: func(_ Node, c swiftCompletion) (string, error) {
			if c.runInput == nil {
				return "", nil
			}
			return *c.runInput, nil
		},
		startInput: func(_ Node, run RunContext) *string { return run.Input },
	},
	"agent": {
		async: true,
		rank:  rank(1),
		startInput: func(n Node, _ RunContext) *string {
			prompt := n.Agent().Prompt
			return &prompt
		},
	},
	"output": {swift: completeOutput, rank: rank(0)},
	// Condition and Aggregator route and merge; neither contributes the run output.
	"condition":  {swift: completeCondition},
	"aggregator": {swift: completeAggregator},
}

// completeCondition is desktop's ConditionRuntime: its output is the selected branch id, which
// the completion fold records in conditionDecisions and never as a pool variable.
func completeCondition(n Node, c swiftCompletion) (string, error) {
	cases, err := n.conditionCases()
	if err != nil {
		return "", err
	}
	return evaluateCondition(cases, c.pool)
}

func completeOutput(n Node, c swiftCompletion) (string, error) {
	for _, other := range c.runs {
		if other.ID != c.run.ID && other.NodeType == "output" && other.Status == StatusSucceeded {
			return "", fmt.Errorf("multiple active output nodes: %s and %s", other.NodeID, n.ID)
		}
	}
	bindings := n.OutputBindings()
	if len(bindings) == 0 {
		return "", nil
	}
	result := map[string]any{}
	for _, b := range bindings {
		value, assigned, err := c.pool.Resolve(b.Selector)
		if err != nil {
			return "", err
		}
		if !assigned {
			return "", fmt.Errorf("result %s references unassigned variable %s", b.Name, qualified(b.Selector))
		}
		result[b.Name] = value
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// Start validates the frozen graph and inputs, then starts the run and its first wave.
func (e *Engine) Start(ctx context.Context, runID string) error {
	defer e.gate(runID)()
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return err
	}
	g, err := ParseGraph(run.Graph)
	if err != nil {
		return err
	}
	start, err := validateExecutable(g)
	if err != nil {
		return err
	}
	payload, err := e.preparePayload(g, run.Payload, run.Input)
	if err != nil {
		return err
	}
	if err := payload.ValidateStartInputs(start); err != nil {
		return err
	}
	entry := NodeStart{NodeID: start.ID, NodeType: start.Kind, Input: run.Input}
	started, err := e.repo.StartRun(ctx, runID, entry, func(raw []byte) ([]byte, error) {
		p, err := e.preparePayload(g, raw, run.Input)
		if err != nil {
			return nil, err
		}
		if err := p.SeedSystem(run.WorkflowID, e.now().UnixMilli()); err != nil {
			return nil, err
		}
		return p.Encode()
	})
	if err != nil {
		return err
	}
	if !started {
		return ErrNotStartable
	}
	e.publish(runID)
	return e.schedule(ctx, runID)
}

// preparePayload derives the run payload on first use (v2 creates runs before any graph-aware
// step) and mirrors the kickoff input, as desktop does at deploy time.
func (e *Engine) preparePayload(g *Graph, raw []byte, input *string) (RunPayload, error) {
	p, ok, err := DecodeRunPayload(raw)
	if err != nil {
		return RunPayload{}, err
	}
	if !ok {
		p = NewRunPayload(g, e.locale)
	}
	return p, p.MirrorInput(input)
}

// UpdateInput replaces the kickoff text and Start values of an editable run.
func (e *Engine) UpdateInput(ctx context.Context, runID string, input *string, values map[string]json.RawMessage) error {
	defer e.gate(runID)()
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil {
		return err
	}
	g, err := ParseGraph(run.Graph)
	if err != nil {
		return err
	}
	return e.repo.UpdateRunInput(ctx, runID, input, func(raw []byte) ([]byte, error) {
		p, err := e.preparePayload(g, raw, input)
		if err != nil {
			return nil, err
		}
		if err := p.ApplyStartValues(values); err != nil {
			return nil, err
		}
		return p.Encode()
	})
}

// Completion is an Agent attempt's settled result.
type Completion struct {
	Output     *string
	Structured any
	// Keys are merged into the attempt payload (stop_reason, the executor actually used...).
	Keys map[string]any
}

// CompleteNode records one attempt's success and continues scheduling. A late or duplicate
// callback is a no-op.
func (e *Engine) CompleteNode(ctx context.Context, runID, nodeRunID string, c Completion) error {
	defer e.gate(runID)()
	advanced, err := e.repo.CompleteNode(ctx, runID, nodeRunID, c.Output, c.Keys, foldCompletion(c.Output, c.Structured))
	if err != nil || !advanced {
		return err
	}
	e.publish(runID)
	return e.schedule(ctx, runID)
}

func foldCompletion(output *string, structured any) func([]byte, NodeRun) ([]byte, error) {
	return func(raw []byte, n NodeRun) ([]byte, error) {
		p, ok, err := DecodeRunPayload(raw)
		if err != nil || !ok {
			return raw, err
		}
		if err := p.Complete(n.NodeID, n.NodeType, n.Iteration, output, structured); err != nil {
			return nil, err
		}
		return p.Encode()
	}
}

// FailNode records one attempt's failure, which fails the run (v2 performs no automatic
// retry). A late callback is a no-op.
func (e *Engine) FailNode(ctx context.Context, runID, nodeRunID string, f Failure) error {
	defer e.gate(runID)()
	return e.fail(ctx, runID, nodeRunID, f)
}

func (e *Engine) fail(ctx context.Context, runID, nodeRunID string, f Failure) error {
	advanced, err := e.repo.FailNode(ctx, runID, nodeRunID, f.Message, f.Output, func(attempt int, payload []byte) map[string]any {
		inject := true
		if p, ok, _ := DecodeRunPayload(payload); ok {
			inject = p.InjectLastFailure
		}
		return f.Detail(attempt, inject, e.now())
	})
	if err != nil || !advanced {
		return err
	}
	e.publish(runID)
	return nil
}

// Abandon reports whether a dispatched Agent attempt must not execute because its run already
// ended, closing the attempt as cancelled. The execution bridge asks before every start: v2 runs a
// run's Agents one at a time, so a sibling queued behind a node that failed the run has not begun,
// and starting it could only produce a discarded late result.
func (e *Engine) Abandon(ctx context.Context, runID, nodeRunID string) (bool, error) {
	defer e.gate(runID)()
	abandoned, err := e.repo.AbandonNode(ctx, runID, nodeRunID)
	if err != nil || !abandoned {
		return false, err
	}
	e.publish(runID)
	return true, nil
}

// Resume reruns one scheduling pass for a running run, e.g. after a crash between a completion
// and its successor wave.
func (e *Engine) Resume(ctx context.Context, runID string) error {
	defer e.gate(runID)()
	return e.schedule(ctx, runID)
}

// schedule is desktop's run_schedule: complete in-flight swift nodes, dispatch the ready wave,
// and finish the run once nothing is ready or in flight. Callers hold the run gate.
func (e *Engine) schedule(ctx context.Context, runID string) error {
	for {
		run, err := e.repo.RunContext(ctx, runID)
		if err != nil {
			return err
		}
		if run.Status != StatusRunning {
			return nil
		}
		g, err := ParseGraph(run.Graph)
		if err != nil {
			return err
		}
		runs, err := e.repo.NodeRuns(ctx, runID)
		if err != nil {
			return err
		}
		payload, _, err := DecodeRunPayload(run.Payload)
		if err != nil {
			return err
		}
		completedSwift := false
		for _, r := range runs {
			if r.Status != StatusRunning || r.Iteration != nil {
				continue
			}
			n, ok := g.Node(r.NodeID)
			rt := runtimes[n.Kind]
			if !ok || rt.swift == nil {
				continue
			}
			output, swiftErr := rt.swift(n, swiftCompletion{run: r, runInput: run.Input, pool: &payload.VariablePool, runs: runs})
			if swiftErr != nil {
				return e.fail(ctx, runID, r.ID, runtimeFailure(n.Kind, swiftErr.Error()))
			}
			advanced, err := e.repo.CompleteNode(ctx, runID, r.ID, &output, nil, foldCompletion(&output, nil))
			if err != nil {
				return err
			}
			if advanced {
				e.publish(runID)
			}
			completedSwift = true
		}
		if completedSwift {
			continue // completions changed the pool and routing; reload before projecting
		}
		p := newProjection(g, runs, payload.ConditionDecisions)
		wave := p.ready()
		if len(wave) == 0 {
			if !p.inFlight() {
				if err := e.repo.FinishRun(ctx, runID, runOutput(runs)); err != nil {
					return err
				}
				e.publish(runID)
			}
			return nil
		}
		starts := make([]NodeStart, len(wave))
		for i, n := range wave {
			starts[i] = NodeStart{NodeID: n.ID, NodeType: n.Kind}
			if in := runtimes[n.Kind].startInput; in != nil {
				starts[i].Input = in(n, run)
			}
		}
		created, err := e.repo.StartNodes(ctx, runID, starts)
		if err != nil {
			return err
		}
		e.publish(runID)
		for i, n := range wave {
			if runtimes[n.Kind].async {
				e.agents.Dispatch(AgentDispatch{
					RunID: runID, NodeRunID: created[i].ID, ProjectID: run.ProjectID, Locale: payload.Locale,
					Node: n, Graph: g, Pool: payload.VariablePool, RunInput: run.Input, NodeRuns: runs,
				})
			}
		}
	}
}

// runOutput picks the succeeded outer attempt with the lowest runtime rank, latest finish first.
func runOutput(runs []NodeRun) *string {
	candidates := []NodeRun{}
	for _, r := range runs {
		if r.Iteration == nil && r.Status == StatusSucceeded && runtimes[r.NodeType].rank != nil {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	finished := func(r NodeRun) int64 {
		if r.FinishedAt == nil {
			return 0
		}
		return r.FinishedAt.UnixNano()
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := *runtimes[candidates[i].NodeType].rank, *runtimes[candidates[j].NodeType].rank
		if ri != rj {
			return ri < rj
		}
		return finished(candidates[i]) >= finished(candidates[j])
	})
	return candidates[0].Output
}

// validateConditionBranches rejects duplicate case ids and outgoing handles that name neither a
// case nor the else branch, which would leave a branch that can never be taken (desktop's
// DuplicateConditionCase and UnknownConditionBranch).
func validateConditionBranches(g *Graph, n Node) error {
	cases, err := n.conditionCases()
	if err != nil {
		return &StartError{Code: "workflow_run_invalid_condition", NodeID: n.ID, Detail: err.Error()}
	}
	ids := map[string]bool{}
	for _, c := range cases {
		if ids[c.id] {
			return &StartError{Code: "workflow_run_duplicate_condition_case", NodeID: n.ID, Detail: "duplicate case id " + c.id}
		}
		ids[c.id] = true
	}
	for _, e := range g.Outgoing(n.ID) {
		handle := e.SourceHandle
		if handle == "" {
			handle = ElseBranch
		}
		if handle != ElseBranch && !ids[handle] {
			return &StartError{Code: "workflow_run_unknown_condition_branch", NodeID: n.ID, Detail: "edge leaves through unknown branch " + handle}
		}
	}
	return nil
}

// validateExecutable applies desktop's start-time policy: an entry, only kinds v2 executes,
// terminal Outputs with unique result names, and well-formed Condition cases and handles.
func validateExecutable(g *Graph) (Node, error) {
	start, ok := g.Start()
	if !ok {
		return Node{}, &StartError{Code: "workflow_run_missing_start_node", Detail: "the workflow has no Start node"}
	}
	for _, n := range g.Nodes() {
		if _, supported := runtimes[n.Kind]; !supported {
			return Node{}, &StartError{Code: "workflow_run_unsupported_node_type", NodeID: n.ID, Detail: "v2 does not execute " + n.Kind + " nodes yet"}
		}
		if n.Kind == "condition" {
			if err := validateConditionBranches(g, n); err != nil {
				return Node{}, err
			}
		}
		if n.Kind == "output" {
			if len(g.Outgoing(n.ID)) != 0 {
				return Node{}, &StartError{Code: "workflow_run_output_has_successors", NodeID: n.ID, Detail: "an Output node must be terminal"}
			}
			seen := map[string]bool{}
			for _, b := range n.OutputBindings() {
				if seen[b.Name] {
					return Node{}, &StartError{Code: "workflow_run_duplicate_output_name", NodeID: n.ID, Detail: "duplicate result name " + b.Name}
				}
				seen[b.Name] = true
			}
		}
	}
	return start, nil
}
