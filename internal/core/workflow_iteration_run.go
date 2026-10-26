package core

import "fmt"

// The iteration node's round engine: the iterator array's length is the round count, each round
// binds `{iteration}.item` / `{iteration}.index`, runs the region once, and collects one value from
// `collectSelector`. Rounds settle into a ledger; `output` keeps only successful collections,
// `entries` keeps every round's `{item, status, output, error}` and `failed_count` counts failures.
//
// The node's own failures (iterator is not an array, over the ceiling, exposed-variable writes) always
// fail the run; only a region failure can be absorbed by `errorStrategy = continue`.

// workflowIterationConfig is the parsed executable configuration of one iteration node.
type workflowIterationConfig struct {
	Iterator workflowSelector
	Collect  workflowSelector
	Strategy string
	Ceiling  int
}

// workflowIterationRunState is the node's persisted round progress. Rounds are contiguous from zero,
// so the ledger stays aligned with the input array without storing positions twice.
type workflowIterationRunState struct {
	Round     int
	Items     []any
	Ledger    []any
	Output    []any
	Failed    int
	Strategy  string
	Completed []any
	invalid   bool
}

// object is the wire shape of the iteration progress: `round`, `items`, `ledger`, `output`,
// `failedCount`, `strategy` and `completed`.
func (s *workflowIterationRunState) object() Object {
	out := Object{
		"round":       s.Round,
		"items":       s.Items,
		"ledger":      s.Ledger,
		"output":      s.Output,
		"failedCount": s.Failed,
		"strategy":    s.Strategy,
		"completed":   s.Completed,
	}
	if out["completed"] == nil {
		out["completed"] = []any{}
	}
	return out
}

// parseWorkflowIterationConfig reads the frozen config; the publish gate already refused every shape
// this rejects, so a failure here means an old snapshot the runtime must not guess about.
func parseWorkflowIterationConfig(data Object) (workflowIterationConfig, error) {
	raw, ok := asWorkflowObject(data["iterationConfig"])
	if !ok {
		return workflowIterationConfig{}, errString("iteration node has no config")
	}
	iterator, ok := workflowIterationSelector(raw["iteratorSelector"])
	if !ok {
		return workflowIterationConfig{}, errString("iteration iterator must be a root selector")
	}
	collect, ok := workflowIterationSelector(raw["collectSelector"])
	if !ok {
		return workflowIterationConfig{}, errString("iteration collect must be a root selector")
	}
	strategy := "fail"
	if value, present := raw["errorStrategy"]; present && value != nil {
		text, isString := value.(string)
		if !isString || (text != "fail" && text != "continue") {
			return workflowIterationConfig{}, errString("iteration error strategy must be fail or continue")
		}
		strategy = text
	}
	ceiling := 50
	if value, present := raw["maxIterations"]; present && value != nil {
		number, ok := workflowNumberOf(value)
		if !ok || !workflowIterationCeilingValid(value) {
			return workflowIterationConfig{}, errString("iteration maxIterations must be a whole number between 1 and 100")
		}
		ceiling = int(number)
	}
	return workflowIterationConfig{Iterator: iterator, Collect: collect, Strategy: strategy, Ceiling: ceiling}, nil
}

// workflowIterationReadStates is the round's pool: ambient variables, outer ancestors, the active
// round's member states, and `{iteration}.item` / `.index`. The exposed trio is not visible inside the
// region — the node has not produced it yet — matching desktop's scope rules.
func workflowIterationReadStates(
	graph workflowGraphView,
	iterationID string,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
	item any,
	index int,
) map[string]workflowNodeState {
	round := workflowLoopOuterStates(graph, iterationID, states, ambient)
	for _, node := range graph.Nodes {
		if node.Container != iterationID {
			continue
		}
		if state, ok := states[node.ID]; ok {
			round[node.ID] = state
		}
	}
	round[iterationID] = workflowNodeState{Status: "succeeded", Output: Object{"item": item, "index": index}}
	return round
}

// runningWorkflowIteration finds an iteration whose state says it is mid-flight.
func runningWorkflowIteration(graph workflowGraphView, states map[string]workflowNodeState) (workflowNodeView, bool) {
	for _, node := range graph.Nodes {
		state, ok := states[node.ID]
		if !ok || node.Kind != "iteration" || state.Status != "running" {
			continue
		}
		return node, true
	}
	return workflowNodeView{}, false
}

// advanceWorkflowIteration drives one iteration step: start, dispatch the next ready member, settle a
// round, or finish. It returns false when it started an agent session, because that pass must end.
func advanceWorkflowIteration(
	t *transaction,
	run Object,
	node workflowNodeView,
	version int64,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
	graph workflowGraphView,
) bool {
	config, err := parseWorkflowIterationConfig(node.Data)
	if err != nil {
		failWorkflowIteration(states, node, "iteration_invalid_config", err.Error())
		return true
	}
	state, committed := states[node.ID]
	if !committed {
		outer := workflowLoopOuterStates(graph, node.ID, states, ambient)
		value, present, err := resolveWorkflowSelector(config.Iterator, outer)
		if err != nil || !present {
			failWorkflowIteration(states, node, "iteration_iterator_unset",
				fmt.Sprintf("iteration source %s has no value", config.Iterator.qualified()))
			return true
		}
		items, ok := value.([]any)
		if !ok {
			failWorkflowIteration(states, node, "iteration_iterator_not_a_list", "iteration source is not an array")
			return true
		}
		if len(items) > config.Ceiling {
			failWorkflowIteration(states, node, "iteration_over_limit",
				fmt.Sprintf("iteration source has %d items, above the %d limit", len(items), config.Ceiling))
			return true
		}
		state = workflowNodeState{
			Status:      "running",
			DisplayName: workflowNodeDisplayName(node),
			StartedAt:   nowRFC3339(),
			Iteration: &workflowIterationRunState{
				Items:     items,
				Ledger:    []any{},
				Output:    []any{},
				Strategy:  config.Strategy,
				Completed: []any{},
			},
		}
		states[node.ID] = state
		if len(items) == 0 {
			finishWorkflowIteration(states, node, &state)
			return true
		}
		return true
	}
	if state.Status != "running" {
		return true
	}
	iteration := state.Iteration
	if !validWorkflowIterationProgress(iteration, &config, graph, node.ID) {
		failWorkflowIteration(states, node, "iteration_invalid_round", "iteration progress is inconsistent with its frozen configuration")
		return true
	}
	item := iteration.Items[iteration.Round]
	body := workflowBodyGraph(graph, node.ID)
	if len(body.Nodes) == 0 {
		failWorkflowIteration(states, node, "iteration_body_empty", "iteration region has no members")
		return true
	}
	projection := projectWorkflowNodes(body, states)
	if projection.hasFailure() {
		reason, detail := workflowBodyFailure(body, states)
		settleWorkflowIterationRound(states, node, item, false, nil, reason+": "+detail, graph)
		if iteration.Strategy == "fail" {
			failWorkflowIteration(states, node, reason, detail)
		}
		return true
	}
	if ready, ok := projection.readyNode(); ok {
		if ready.Kind == "agent" {
			startWorkflowAgentSession(t, run, ready, version, states)
			return false
		}
		readStates := workflowIterationReadStates(graph, node.ID, states, ambient, item, iteration.Round)
		states[ready.ID] = executeWorkflowNode(ready, run.O("input"), workflowPredecessorStates(graph, ready, readStates), ambient)
		return true
	}
	if projection.hasPending() || workflowBodyInFlight(body, states) {
		return false
	}
	readStates := workflowIterationReadStates(graph, node.ID, states, ambient, item, iteration.Round)
	collected, present, err := resolveWorkflowSelector(config.Collect, readStates)
	if err != nil || !present || states[config.Collect.nodeID].Status != "succeeded" {
		// A collect target that did not run makes the round a failure; `continue` can absorb it, while
		// `fail` ends the node. Either way the ledger keeps the reason instead of dropping the round.
		settleWorkflowIterationRound(states, node, item, false, nil, "collect target did not run this round", graph)
		if iteration.Strategy == "fail" {
			failWorkflowIteration(states, node, "iteration_collect_unset",
				fmt.Sprintf("collect target %s did not run this round", config.Collect.qualified()))
		}
		return true
	}
	settleWorkflowIterationRound(states, node, item, true, collected, "", graph)
	return true
}

// settleWorkflowIterationRound records the round's outcome, archives its member states, clears them
// for the next round and finishes the node once the input array is exhausted.
func settleWorkflowIterationRound(
	states map[string]workflowNodeState,
	node workflowNodeView,
	item any,
	succeeded bool,
	value any,
	detail string,
	graph workflowGraphView,
) {
	state := states[node.ID]
	iteration := state.Iteration
	entry := Object{"item": item, "status": "failed", "output": nil, "error": detail}
	if succeeded {
		entry = Object{"item": item, "status": "succeeded", "output": value, "error": nil}
		iteration.Output = append(iteration.Output, value)
	} else {
		iteration.Failed++
	}
	iteration.Ledger = append(iteration.Ledger, entry)
	workflowInactiveStates(workflowBodyGraph(graph, node.ID), states)
	archiveWorkflowIterationRound(iteration, workflowBodyGraph(graph, node.ID), states)
	states[node.ID] = state
	if !succeeded && iteration.Strategy == "fail" {
		return
	}
	if iteration.Round+1 == len(iteration.Items) {
		finishWorkflowIteration(states, node, &state)
		return
	}
	for _, member := range workflowBodyGraph(graph, node.ID).Nodes {
		delete(states, member.ID)
	}
	iteration.Round++
}

// archiveWorkflowIterationRound retains the active projection. Only the next-round transition
// clears members, so terminal details and the archived last round describe the same execution.
func archiveWorkflowIterationRound(iteration *workflowIterationRunState, body workflowGraphView, states map[string]workflowNodeState) {
	archived := Object{}
	for _, member := range body.Nodes {
		if state, ok := states[member.ID]; ok {
			archived[member.ID] = state.object()
		}
	}
	iteration.Completed = append(iteration.Completed, Object{"index": iteration.Round, "states": archived})
}

// finishWorkflowIteration publishes the exposed variables and settles the node succeeded.
func finishWorkflowIteration(states map[string]workflowNodeState, node workflowNodeView, state *workflowNodeState) {
	iteration := state.Iteration
	state.Status = "succeeded"
	state.FinishedAt = nowRFC3339()
	state.Output = Object{
		"output":       iteration.Output,
		"entries":      iteration.Ledger,
		"failed_count": iteration.Failed,
	}
	states[node.ID] = *state
}

// failWorkflowIteration settles the node and the run as failed with a stable reason.
func failWorkflowIteration(states map[string]workflowNodeState, node workflowNodeView, reason, detail string) {
	state := states[node.ID]
	state.Status = "failed"
	state.DisplayName = workflowNodeDisplayName(node)
	state.FinishedAt = nowRFC3339()
	state.Error = detail
	state.Output = Object{"reason": reason}
	states[node.ID] = state
}

// cancelWorkflowIterations marks every running iteration cancelled when the run is cancelled.
func cancelWorkflowIterations(states map[string]workflowNodeState) {
	for id := range states {
		state := states[id]
		if state.Status != "running" || state.Iteration == nil {
			continue
		}
		state.Status = "cancelled"
		state.FinishedAt = nowRFC3339()
		states[id] = state
	}
}

// workflowIterationRunStateFromJSON retains malformed progress as invalid. Recovery fails it
// explicitly instead of treating a damaged document as a new iteration.
func workflowIterationRunStateFromJSON(raw Object) *workflowIterationRunState {
	if raw == nil {
		return nil
	}
	items, _ := raw["items"].([]any)
	ledger, _ := raw["ledger"].([]any)
	output, _ := raw["output"].([]any)
	completed, _ := raw["completed"].([]any)
	round := -1
	if number, ok := workflowNumberOf(raw["round"]); ok && number >= 0 && number <= 100 && number == float64(int(number)) {
		round = int(number)
	}
	failed := -1
	if number, ok := workflowNumberOf(raw["failedCount"]); ok && number >= 0 && number <= 100 && number == float64(int(number)) {
		failed = int(number)
	}
	return &workflowIterationRunState{
		Round:     round,
		Items:     items,
		Ledger:    ledger,
		Output:    output,
		Failed:    failed,
		Strategy:  raw.S("strategy"),
		Completed: completed,
		invalid:   items == nil || ledger == nil || output == nil || completed == nil || round < 0 || failed < 0,
	}
}
