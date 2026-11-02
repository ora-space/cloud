package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

func (e *Engine) containerRepo() (ContainerRepository, error) {
	r, ok := e.repo.(ContainerRepository)
	if !ok {
		return nil, fmt.Errorf("workflow repository does not support containers")
	}
	return r, nil
}

func startFor(n Node, run RunContext, scope string, round *int) NodeStart {
	start := NodeStart{NodeID: n.ID, NodeType: n.Kind, ScopeID: scope, Iteration: round}
	if input := runtimes[n.Kind].startInput; input != nil {
		start.Input = input(n, run)
	}
	return start
}

func (e *Engine) dispatchCreated(run RunContext, g *Graph, created, rows []NodeRun, pool VariablePool, locale string) {
	for _, row := range created {
		n, ok := g.Node(row.NodeID)
		if !ok || !runtimes[n.Kind].async {
			continue
		}
		e.agents.Dispatch(AgentDispatch{RunID: run.RunID, ProjectID: run.ProjectID, NodeRunID: row.ID,
			ScopeID: row.ScopeID, Iteration: row.Iteration, Locale: locale, Node: n, Graph: g,
			Pool: pool, RunInput: run.Input, NodeRuns: rows})
	}
}

func (e *Engine) startWave(ctx context.Context, run RunContext, g *Graph, starts []NodeStart, pool VariablePool, locale string, rows []NodeRun) (bool, error) {
	created, err := e.repo.StartNodes(ctx, run.RunID, starts)
	if err != nil {
		return false, err
	}
	if len(created) == 0 {
		return false, nil
	}
	e.publish(run.RunID)
	e.dispatchCreated(run, g, created, rows, pool, locale)
	return true, nil
}

func (e *Engine) applyContainer(ctx context.Context, run RunContext, g *Graph, parentID string, advance ContainerAdvance) (bool, error) {
	repo, err := e.containerRepo()
	if err != nil {
		return false, err
	}
	created, changed, err := repo.AdvanceContainer(ctx, run.RunID, parentID, advance)
	if err != nil || !changed {
		return changed, err
	}
	e.publish(run.RunID)
	if len(created) == 0 {
		return true, nil
	}
	latest, err := e.repo.RunContext(ctx, run.RunID)
	if err != nil {
		return true, err
	}
	p, _, err := DecodeRunPayload(latest.Payload)
	if err != nil {
		return true, err
	}
	pool := p.VariablePool
	var rows []NodeRun
	if created[0].ScopeID == "" {
		rows, err = e.repo.NodeRuns(ctx, run.RunID)
	} else {
		var rounds []LoopRound
		rounds, err = repo.LoopRounds(ctx, run.RunID)
		if err == nil {
			for _, round := range rounds {
				if round.ID == created[0].ScopeID {
					var state loopState
					state, err = decodeLoopState(round.State)
					pool = state.VariablePool
					break
				}
			}
		}
		if err == nil {
			rows, err = repo.ScopeNodes(ctx, run.RunID, created[0].ScopeID)
		}
	}
	if err != nil {
		return true, err
	}
	e.dispatchCreated(latest, g, created, rows, pool, p.Locale)
	return true, nil
}

func (e *Engine) containerFailure(message string) *ContainerResult {
	f := Failure{Kind: FailureInvalidRunPayload, Message: message}
	return &ContainerResult{Status: StatusFailed, Error: &message, Keys: map[string]any{"error_detail": f.Detail(1, true, e.now())}}
}

func (e *Engine) failContainer(ctx context.Context, run RunContext, parent NodeRun, scopeID, message string) (bool, error) {
	a := ContainerAdvance{ScopeID: scopeID, Result: e.containerFailure(message)}
	if scopeID != "" {
		a.ScopeStatus = StatusFailed
	}
	return e.applyContainer(ctx, run, nil, parent.ID, a)
}

func loopRoundStart(g *Graph, pool VariablePool, index int) *LoopRoundStart {
	entry, _ := g.Start()
	state, _ := json.Marshal(loopState{VariablePool: pool, ConditionDecisions: map[string]string{}})
	return &LoopRoundStart{Index: index, State: state, Entry: NodeStart{NodeID: entry.ID, NodeType: entry.Kind}}
}

func loopSuccess(n Node, scopeID, reason string, outputs map[string]any) ContainerAdvance {
	raw, _ := json.Marshal(outputs)
	text := string(raw)
	return ContainerAdvance{ScopeID: scopeID, ScopeStatus: StatusSucceeded,
		Result: &ContainerResult{Status: StatusSucceeded, Output: &text, Keys: map[string]any{"stop_reason": reason}},
		Fold: func(raw []byte) ([]byte, error) {
			p, _, err := DecodeRunPayload(raw)
			if err != nil {
				return nil, err
			}
			if err := p.Complete(n.ID, "loop", nil, &text, nil); err != nil {
				return nil, err
			}
			for name, value := range outputs {
				if err := p.VariablePool.Set(n.ID+"."+name, n.ID, value); err != nil {
					return nil, err
				}
			}
			return p.Encode()
		}}
}

func (e *Engine) advanceLoop(ctx context.Context, run RunContext, g *Graph, parent NodeRun, n Node, payload RunPayload) (bool, error) {
	repo, err := e.containerRepo()
	if err != nil {
		return false, err
	}
	cfg, err := n.loopConfig()
	if err != nil {
		return e.failContainer(ctx, run, parent, "", err.Error())
	}
	body, _ := g.LoopBody(n.ID)
	rounds, err := repo.LoopRounds(ctx, run.RunID)
	if err != nil {
		return false, err
	}
	var current *LoopRound
	for i := range rounds {
		if rounds[i].ParentID == parent.ID && (current == nil || rounds[i].Index > current.Index) {
			current = &rounds[i]
		}
	}
	if current == nil {
		carried, err := cfg.initial(&payload.VariablePool)
		if err != nil {
			return e.failContainer(ctx, run, parent, "", err.Error())
		}
		pool, err := g.loopPool(n, cfg, payload.VariablePool, carried)
		if err != nil {
			return e.failContainer(ctx, run, parent, "", err.Error())
		}
		return e.applyContainer(ctx, run, body, parent.ID, ContainerAdvance{Next: loopRoundStart(body, pool, 1)})
	}
	if current.Status != StatusRunning {
		return e.failContainer(ctx, run, parent, "", "Loop parent has no active round")
	}
	state, err := decodeLoopState(current.State)
	if err != nil {
		return e.failContainer(ctx, run, parent, current.ID, err.Error())
	}
	if state.Exit != nil {
		if cleanup, ok := e.agents.(LoopExitDispatcher); ok {
			cleanup.CleanupLoopExit(run.RunID, parent.ID, current.ID)
			return false, nil
		}
		return e.failContainer(ctx, run, parent, current.ID, "execution host cannot drain LoopExit")
	}
	rows, err := repo.ScopeNodes(ctx, run.RunID, current.ID)
	if err != nil {
		return false, err
	}
	// A committed break takes precedence over other controls in this scope.
	for _, row := range rows {
		if row.Status == StatusRunning && row.NodeType == "loopExit" {
			outputs, outputErr := cfg.outputs(&state.VariablePool)
			result := map[string]any{"Ok": outputs}
			if outputErr != nil {
				result = map[string]any{"Err": outputErr.Error()}
			}
			state.Exit = &loopExit{NodeRunID: row.ID, RequestedAt: e.now().UnixMilli(), Result: result}
			raw, _ := json.Marshal(state)
			return e.applyContainer(ctx, run, body, parent.ID, ContainerAdvance{ScopeID: current.ID, ScopeState: raw, CancelScope: true, ExitNodeID: row.ID})
		}
	}
	for _, row := range rows {
		if row.Status != StatusRunning {
			continue
		}
		child, found := body.Node(row.NodeID)
		if !found || runtimes[child.Kind].swift == nil {
			continue
		}
		output, swiftErr := runtimes[child.Kind].swift(child, swiftCompletion{run: row, pool: &state.VariablePool, runs: rows})
		if swiftErr != nil {
			err := e.fail(ctx, run.RunID, row.ID, runtimeFailure(child.Kind, swiftErr.Error()))
			return true, err
		}
		changed, err := e.repo.CompleteNode(ctx, run.RunID, row.ID, &output, nil, foldCompletion(&output, nil))
		if changed {
			e.publish(run.RunID)
		}
		return changed, err // reload the private pool before completing another control
	}
	projection := newProjection(body, rows, state.ConditionDecisions)
	wave := projection.ready()
	for _, child := range wave {
		if child.Kind == "loopExit" {
			wave = []Node{child}
			break
		}
	}
	if len(wave) != 0 {
		starts := make([]NodeStart, len(wave))
		for i, child := range wave {
			starts[i] = startFor(child, run, current.ID, nil)
		}
		return e.startWave(ctx, run, body, starts, state.VariablePool, payload.Locale, rows)
	}
	if projection.inFlight() {
		return false, nil
	}
	carried, outputs, reason, err := cfg.complete(current.Index, &state.VariablePool)
	if err != nil {
		return e.failContainer(ctx, run, parent, current.ID, err.Error())
	}
	if reason != "" {
		return e.applyContainer(ctx, run, body, parent.ID, loopSuccess(n, current.ID, reason, outputs))
	}
	pool, err := g.loopPool(n, cfg, payload.VariablePool, carried)
	if err != nil {
		return e.failContainer(ctx, run, parent, current.ID, err.Error())
	}
	return e.applyContainer(ctx, run, body, parent.ID, ContainerAdvance{ScopeID: current.ID, ScopeStatus: StatusSucceeded, Next: loopRoundStart(body, pool, current.Index+1)})
}

// FinishLoopExit acknowledges cleanup for exactly the still-active parent and scope. Frozen
// outputs win over late callbacks; an unconfirmed stop fails the run and cannot launch successors.
func (e *Engine) FinishLoopExit(ctx context.Context, runID, parentID, scopeID string, cleanupErr error) error {
	defer e.gate(runID)()
	repo, err := e.containerRepo()
	if err != nil {
		return err
	}
	run, err := e.repo.RunContext(ctx, runID)
	if err != nil || run.Status != StatusRunning {
		return err
	}
	rounds, err := repo.LoopRounds(ctx, runID)
	if err != nil {
		return err
	}
	for _, round := range rounds {
		if round.ID != scopeID || round.ParentID != parentID || round.Status != StatusRunning {
			continue
		}
		state, err := decodeLoopState(round.State)
		if err != nil {
			return err
		}
		if state.Exit == nil {
			return nil
		}
		rows, err := e.repo.NodeRuns(ctx, runID)
		if err != nil {
			return err
		}
		for _, parent := range rows {
			if parent.ID != parentID || parent.Status != StatusRunning {
				continue
			}
			if message, ok := state.Exit.Result["Err"].(string); ok && cleanupErr == nil {
				cleanupErr = fmt.Errorf("%s", message)
			}
			if cleanupErr != nil {
				_, err = e.failContainer(ctx, run, parent, scopeID, cleanupErr.Error())
			} else {
				g, parseErr := ParseGraph(run.Graph)
				if parseErr != nil {
					return parseErr
				}
				n, _ := g.Node(parent.NodeID)
				outputs, ok := state.Exit.Result["Ok"].(map[string]any)
				if !ok {
					return fmt.Errorf("LoopExit has no frozen result")
				}
				_, err = e.applyContainer(ctx, run, g, parentID, loopSuccess(n, scopeID, "loop_exit", outputs))
			}
			if err != nil {
				return err
			}
			return e.schedule(ctx, runID)
		}
	}
	return nil
}
