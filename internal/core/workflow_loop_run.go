package core

import (
	"errors"
	"fmt"
)

var errWorkflowLoopType = errors.New("loop_variable_type_mismatch")

// The loop round engine. A loop is a container node whose body is a separate DAG (specs
// decisions/cloud/workflow/20261025-loop-and-loop-exit-execution.md). The active round's member
// states live in the run's flat node_states map, which is what lets the existing session, retry,
// branch and viewer paths keep working untouched: a body agent is an ordinary agent node while it
// runs. Completed rounds are archived on the loop node's own state.
//
// The engine is a pure advance function over committed state: it evaluates carried values, drives the
// body projection, folds a completed round into feedback, until and outputs, and either starts the
// next round or settles the loop. Nothing is remembered between passes.

// workflowLoopRunState is the durable round progress of one loop node.
type workflowLoopRunState struct {
	Round         int64
	MaxIterations int64
	// Carried is the value set the active round entered with, keyed by the declared variable name.
	Carried map[string]any
	// Completed archives every finished round: {index, carried, states}. The final round is archived
	// too; its member states also stay in the flat map so the run viewer can show them.
	Completed []any
	invalid   bool
}

// object renders the loop progress in the shape the run viewer reads.
func (s *workflowLoopRunState) object() Object {
	carried := Object{}
	for name, value := range s.Carried {
		carried[name] = value
	}
	completed := s.Completed
	if completed == nil {
		completed = []any{}
	}
	out := Object{
		"round":         s.Round,
		"maxIterations": s.MaxIterations,
		"carried":       carried,
		"completed":     completed,
	}
	return out
}

// workflowLoopRunStateFromJSON preserves malformed progress for fail-closed recovery.
func workflowLoopRunStateFromJSON(raw Object) *workflowLoopRunState {
	state := &workflowLoopRunState{
		Round:         raw.N("round"),
		MaxIterations: raw.N("maxIterations"),
		Carried:       map[string]any{},
	}
	round, roundOK := workflowNumberOf(raw["round"])
	ceiling, maxOK := workflowNumberOf(raw["maxIterations"])
	_, carriedOK := asWorkflowObject(raw["carried"])
	_, completedOK := raw["completed"].([]any)
	state.invalid = !roundOK || round < 1 || round > 100 || round != float64(state.Round) || !maxOK || ceiling != float64(state.MaxIterations) || !carriedOK || !completedOK
	if carried, ok := asWorkflowObject(raw["carried"]); ok {
		for name, value := range carried {
			state.Carried[name] = value
		}
	}
	if completed, ok := raw["completed"].([]any); ok {
		state.Completed = completed
	}
	return state
}

// workflowLoopVariableConfig is one carried variable after parsing: its first-round initializer and
// the selector that supplies the next round's value.
type workflowLoopVariableConfig struct {
	Name            string
	ValueType       string
	InitialKind     string
	InitialValue    any
	InitialSelector workflowSelector
	Feedback        workflowSelector
}

// workflowLoopOutput is one named result the loop publishes when it succeeds.
type workflowLoopOutput struct {
	Name     string
	Selector workflowSelector
}

// workflowLoopConfig is the executable shape of `data.loopConfig`. It is parsed defensively: a
// snapshot that predates the publish gate must fail the node with an explanation, never panic or
// silently run with defaults.
type workflowLoopConfig struct {
	MaxIterations int64
	Behavior      string
	Variables     []workflowLoopVariableConfig
	UntilLogic    string
	UntilRules    []any
	Outputs       []workflowLoopOutput
}

// parseWorkflowLoopConfig compiles the wire config the publish gate accepted. Every field the engine
// needs is required here as well, because a frozen snapshot may predate the gate.
func parseWorkflowLoopConfig(data Object) (workflowLoopConfig, error) {
	raw, present := data["loopConfig"]
	if !present || raw == nil {
		return workflowLoopConfig{}, fmt.Errorf("loop node has no loopConfig")
	}
	config, ok := asWorkflowObject(raw)
	if !ok {
		return workflowLoopConfig{}, fmt.Errorf("loopConfig must be an object")
	}
	maxIterations, numeric := workflowNumberOf(config["maxIterations"])
	if !numeric || maxIterations < workflowLoopMinIterations || maxIterations > workflowLoopMaxIterations {
		return workflowLoopConfig{}, fmt.Errorf("maxIterations must be between %d and %d", workflowLoopMinIterations, workflowLoopMaxIterations)
	}
	parsed := workflowLoopConfig{MaxIterations: int64(maxIterations), Behavior: "fail"}
	switch behavior := config.S("maxIterationsBehavior"); behavior {
	case "":
	case "fail", "succeed":
		parsed.Behavior = behavior
	default:
		return workflowLoopConfig{}, fmt.Errorf("unknown maxIterationsBehavior %q", behavior)
	}
	variables, ok := config["variables"].([]any)
	if !ok {
		return workflowLoopConfig{}, fmt.Errorf("loop variables must be a list")
	}
	for _, entry := range variables {
		variable, err := parseWorkflowLoopVariable(entry)
		if err != nil {
			return workflowLoopConfig{}, err
		}
		parsed.Variables = append(parsed.Variables, variable)
	}
	until, ok := asWorkflowObject(config["until"])
	if !ok {
		return workflowLoopConfig{}, fmt.Errorf("loop until must be an object")
	}
	switch until.S("logic") {
	case "and", "or":
		parsed.UntilLogic = until.S("logic")
	default:
		return workflowLoopConfig{}, fmt.Errorf("loop until logic must be and or or")
	}
	rules, ok := until["conditions"].([]any)
	if !ok {
		return workflowLoopConfig{}, fmt.Errorf("loop until conditions must be a list")
	}
	parsed.UntilRules = rules
	outputs, ok := config["outputs"].([]any)
	if !ok {
		return workflowLoopConfig{}, fmt.Errorf("loop outputs must be a list")
	}
	for _, entry := range outputs {
		output, ok := asWorkflowObject(entry)
		if !ok {
			return workflowLoopConfig{}, fmt.Errorf("loop output must be an object")
		}
		selector, ok := parseWorkflowSelector(output["variableSelector"])
		if !ok {
			return workflowLoopConfig{}, fmt.Errorf("loop output %q has an invalid selector", output.S("name"))
		}
		parsed.Outputs = append(parsed.Outputs, workflowLoopOutput{Name: output.S("name"), Selector: selector})
	}
	return parsed, nil
}

// parseWorkflowLoopVariable compiles one carried variable.
func parseWorkflowLoopVariable(raw any) (workflowLoopVariableConfig, error) {
	entry, ok := asWorkflowObject(raw)
	if !ok {
		return workflowLoopVariableConfig{}, fmt.Errorf("loop variable must be an object")
	}
	variable := workflowLoopVariableConfig{Name: entry.S("name"), ValueType: entry.S("valueType")}
	if variable.Name == "" || variable.ValueType == "" {
		return workflowLoopVariableConfig{}, fmt.Errorf("loop variable needs a name and a value type")
	}
	initial, ok := asWorkflowObject(entry["initial"])
	if !ok {
		return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q has no initial value", variable.Name)
	}
	switch variable.InitialKind = initial.S("kind"); variable.InitialKind {
	case "constant":
		value, present := initial["value"]
		if !present {
			return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q has no initial constant", variable.Name)
		}
		normalized, ok := normalizeWorkflowValue(value, variable.ValueType)
		if !ok {
			return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q initial value does not match %s", variable.Name, variable.ValueType)
		}
		variable.InitialValue = normalized
	case "variable":
		selector, ok := parseWorkflowSelector(initial["selector"])
		if !ok {
			return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q has an invalid initial selector", variable.Name)
		}
		variable.InitialSelector = selector
	default:
		return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q has an unknown initial kind", variable.Name)
	}
	feedback, ok := parseWorkflowSelector(entry["feedback"])
	if !ok {
		return workflowLoopVariableConfig{}, fmt.Errorf("loop variable %q has an invalid feedback selector", variable.Name)
	}
	variable.Feedback = feedback
	return variable, nil
}

// workflowOuterGraph is the graph the run's wave executes: every loop body is driven by its own
// container engine, so members are not nodes of the outer ready set.
func workflowOuterGraph(graph workflowGraphView) workflowGraphView {
	out := workflowGraphView{ByNode: map[string]workflowNodeView{}, Start: graph.Start}
	for _, node := range graph.Nodes {
		if node.Container != "" {
			continue
		}
		out.Nodes = append(out.Nodes, node)
		out.ByNode[node.ID] = node
	}
	for _, edge := range graph.Edges {
		if _, ok := out.ByNode[edge.S("source")]; !ok {
			continue
		}
		if _, ok := out.ByNode[edge.S("target")]; !ok {
			continue
		}
		out.Edges = append(out.Edges, edge)
	}
	return out
}

// workflowBodyGraph is one loop's body as a standalone graph: members, internal edges and the body
// start. An empty Start means the document has no defined place to begin and the loop cannot run.
func workflowBodyGraph(graph workflowGraphView, loopID string) workflowGraphView {
	out := workflowGraphView{ByNode: map[string]workflowNodeView{}}
	for _, node := range graph.Nodes {
		if node.Container != loopID {
			continue
		}
		out.Nodes = append(out.Nodes, node)
		out.ByNode[node.ID] = node
		if node.Kind == "start" && out.Start == "" {
			out.Start = node.ID
		}
	}
	for _, edge := range graph.Edges {
		if _, ok := out.ByNode[edge.S("source")]; !ok {
			continue
		}
		if _, ok := out.ByNode[edge.S("target")]; !ok {
			continue
		}
		out.Edges = append(out.Edges, edge)
	}
	return out
}

// runningWorkflowLoop finds a loop whose state says it is mid-flight. A committed running loop is not
// "ready" in the projection, so the wave has to drive it explicitly before looking for new work.
func runningWorkflowLoop(graph workflowGraphView, states map[string]workflowNodeState) (workflowNodeView, bool) {
	for _, node := range graph.Nodes {
		if node.Kind == "loop" && states[node.ID].Status == "running" && states[node.ID].Loop != nil {
			return node, true
		}
	}
	return workflowNodeView{}, false
}

// advanceWorkflowLoop drives one loop as far as committed state allows. It returns true when the pass
// may continue the outer wave (progress was made or the loop is terminal), and false when the pass
// must return — a body agent session was started, or the loop is waiting for one.
func advanceWorkflowLoop(
	t *transaction,
	run Object,
	node workflowNodeView,
	version int64,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
	graph workflowGraphView,
) bool {
	config, err := parseWorkflowLoopConfig(node.Data)
	if err != nil {
		failWorkflowLoop(states, node, "loop_invalid_config", err.Error())
		return true
	}
	state, committed := states[node.ID]
	if !committed {
		carried, err := initializeWorkflowLoopCarried(&config, graph, node.ID, states, ambient)
		if err != nil {
			reason := "loop_value_unset"
			if errors.Is(err, errWorkflowLoopType) {
				reason = "loop_variable_type_mismatch"
			}
			failWorkflowLoop(states, node, reason, err.Error())
			return true
		}
		state = workflowNodeState{
			Status:      "running",
			DisplayName: workflowNodeDisplayName(node),
			StartedAt:   nowRFC3339(),
			Loop: &workflowLoopRunState{
				Round:         1,
				MaxIterations: config.MaxIterations,
				Carried:       carried,
				Completed:     []any{},
			},
		}
	} else if state.Loop == nil || state.Status != "running" {
		return true
	}
	states[node.ID] = state
	if !validWorkflowLoopProgress(state.Loop, &config, graph, node.ID) {
		failWorkflowLoop(states, node, "loop_invalid_round", "invalid persisted loop progress")
		return true
	}
	body := workflowBodyGraph(graph, node.ID)
	if body.Start == "" {
		failWorkflowLoop(states, node, "loop_body_start_count", "loop body has no start node")
		return true
	}
	projection := projectWorkflowNodes(body, states)
	if projection.hasFailure() {
		reason, detail := workflowBodyFailure(body, states)
		archiveWorkflowLoopRound(state.Loop, body, states, true)
		failWorkflowLoop(states, node, reason, detail)
		return true
	}
	if workflowLoopExitSucceeded(body, states) && !workflowBodyInFlight(body, states) {
		for _, member := range body.Nodes {
			if _, executed := states[member.ID]; !executed {
				states[member.ID] = workflowNodeState{Status: "inactive", DisplayName: workflowNodeDisplayName(member)}
			}
		}
		outputs, err := resolveWorkflowLoopOutputs(&config, workflowLoopReadStates(graph, node.ID, states, ambient))
		if err != nil {
			failWorkflowLoop(states, node, "loop_exit_output_unset", err.Error())
			return true
		}
		finishWorkflowLoop(states, node, &state, body, outputs)
		return true
	}
	if ready, ok := projection.readyNode(); ok {
		readStates := workflowLoopReadStates(graph, node.ID, states, ambient)
		if ready.Kind == "agent" {
			startWorkflowAgentSession(t, run, ready, version, states, workflowConditionStates(workflowPredecessorStates(graph, ready, readStates), ambient))
			return false
		}
		states[ready.ID] = executeWorkflowNode(ready, run.O("input"), workflowPredecessorStates(graph, ready, readStates), ambient)
		return true
	}
	if projection.hasPending() {
		return false
	}
	if workflowBodyInFlight(body, states) {
		// A committed running or retrying member owns the serial slot; the pass settles it through the
		// session or retry path before the round can be judged complete.
		return false
	}
	roundStates := workflowLoopReadStates(graph, node.ID, states, ambient)
	feedback, err := resolveWorkflowLoopFeedback(&config, roundStates)
	if err != nil {
		reason := "loop_value_unset"
		if errors.Is(err, errWorkflowLoopType) {
			reason = "loop_variable_type_mismatch"
		}
		failWorkflowLoop(states, node, reason, err.Error())
		return true
	}
	satisfied, err := workflowLoopUntilSatisfied(&config, roundStates)
	if err != nil {
		failWorkflowLoop(states, node, "loop_value_unset", err.Error())
		return true
	}
	if satisfied {
		outputs, err := resolveWorkflowLoopOutputs(&config, roundStates)
		if err != nil {
			failWorkflowLoop(states, node, "loop_value_unset", err.Error())
			return true
		}
		finishWorkflowLoop(states, node, &state, body, outputs)
		return true
	}
	if state.Loop.Round >= config.MaxIterations {
		if config.Behavior == "succeed" {
			outputs, err := resolveWorkflowLoopOutputs(&config, roundStates)
			if err != nil {
				failWorkflowLoop(states, node, "loop_value_unset", err.Error())
				return true
			}
			finishWorkflowLoop(states, node, &state, body, outputs)
			return true
		}
		// The round that exhausted the budget is a finished round: archive it before failing so the
		// run's evidence contains the values the condition was judged against.
		archiveWorkflowLoopRound(state.Loop, body, states, true)
		failWorkflowLoop(states, node, "loop_max_iterations_reached",
			fmt.Sprintf("loop did not terminate within %d rounds", config.MaxIterations))
		return true
	}
	archiveWorkflowLoopRound(state.Loop, body, states, false)
	state.Loop.Round++
	state.Loop.Carried = feedback
	states[node.ID] = state
	return true
}

// initializeWorkflowLoopCarried evaluates every carried variable once, before the first round: a
// constant is normalized against its declared type and a selector is read from the loop's outer
// scope. A missing or mistyped value fails the loop instead of starting it with a hole.
func initializeWorkflowLoopCarried(
	config *workflowLoopConfig,
	graph workflowGraphView,
	loopID string,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
) (map[string]any, error) {
	outer := workflowLoopOuterStates(graph, loopID, states, ambient)
	carried := map[string]any{}
	for i := range config.Variables {
		variable := &config.Variables[i]
		value := variable.InitialValue
		if variable.InitialKind == "variable" {
			resolved, present, err := resolveWorkflowSelector(variable.InitialSelector, outer)
			if err != nil || !present {
				return nil, fmt.Errorf("initial value of %s is not available", variable.Name)
			}
			normalized, ok := normalizeWorkflowValue(resolved, variable.ValueType)
			if !ok {
				return nil, fmt.Errorf("%w: initial value of %s", errWorkflowLoopType, variable.Name)
			}
			value = normalized
		}
		carried[variable.Name] = value
	}
	return carried, nil
}

// resolveWorkflowLoopFeedback reads every feedback selector from the completed round in one snapshot
// and normalizes it against the declared type. All variables are read before any is assigned, so two
// variables that swap values receive each other's value.
func resolveWorkflowLoopFeedback(config *workflowLoopConfig, roundStates map[string]workflowNodeState) (map[string]any, error) {
	carried := map[string]any{}
	for i := range config.Variables {
		variable := &config.Variables[i]
		value, present, err := resolveWorkflowSelector(variable.Feedback, roundStates)
		if err != nil || !present {
			return nil, fmt.Errorf("feedback selector %s has no value", variable.Feedback.qualified())
		}
		normalized, ok := normalizeWorkflowValue(value, variable.ValueType)
		if !ok {
			return nil, fmt.Errorf("%w: feedback selector %s does not match %s", errWorkflowLoopType, variable.Feedback.qualified(), variable.ValueType)
		}
		carried[variable.Name] = normalized
	}
	return carried, nil
}

// workflowLoopUntilSatisfied evaluates the round-end condition over the completed round. An until with
// no rules never matches, which is desktop's reading: only a case with conditions can select.
func workflowLoopUntilSatisfied(config *workflowLoopConfig, roundStates map[string]workflowNodeState) (bool, error) {
	until := workflowNodeView{
		ID:   "until",
		Kind: "condition",
		Data: Object{"cases": []any{Object{"id": "until", "logic": config.UntilLogic, "conditions": config.UntilRules}}},
	}
	branch, err := evaluateWorkflowCondition(until, roundStates)
	if err != nil {
		return false, err
	}
	return branch != workflowBranchElse, nil
}

// resolveWorkflowLoopOutputs reads every named output from the round that produced it.
func resolveWorkflowLoopOutputs(config *workflowLoopConfig, roundStates map[string]workflowNodeState) (Object, error) {
	outputs := Object{}
	for _, output := range config.Outputs {
		value, present, err := resolveWorkflowSelector(output.Selector, roundStates)
		if err != nil || !present {
			return nil, fmt.Errorf("loop output %s has no value", output.Name)
		}
		outputs[output.Name] = value
	}
	return outputs, nil
}

// workflowLoopReadStates is the round's variable pool as committed states: ambient variables, the
// loop's outer ancestors, the active round's member states, and the carried values under the loop's
// own node id. It is what feedback, until, outputs and body conditions read.
func workflowLoopReadStates(
	graph workflowGraphView,
	loopID string,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
) map[string]workflowNodeState {
	round := workflowLoopOuterStates(graph, loopID, states, ambient)
	for _, node := range graph.Nodes {
		if node.Container != loopID {
			continue
		}
		if state, ok := states[node.ID]; ok {
			round[node.ID] = state
		}
	}
	loop := states[loopID].Loop
	if loop != nil {
		carried := Object{}
		for name, value := range loop.Carried {
			carried[name] = value
		}
		round[loopID] = workflowNodeState{Status: "succeeded", Output: carried}
	}
	return round
}

// workflowLoopOuterStates is the pool visible before any round: ambient variables overlaid with the
// loop's transitive predecessors. Body members are never ancestors of their own loop.
func workflowLoopOuterStates(
	graph workflowGraphView,
	loopID string,
	states map[string]workflowNodeState,
	ambient map[string]workflowNodeState,
) map[string]workflowNodeState {
	outer := map[string]workflowNodeState{}
	for id := range ambient {
		outer[id] = ambient[id]
	}
	for id := range workflowAncestorIDs(graph, loopID) {
		if state, ok := states[id]; ok {
			outer[id] = state
		}
	}
	return outer
}

// workflowAncestorIDs walks the reverse edges of one node and returns its transitive predecessors.
func workflowAncestorIDs(graph workflowGraphView, nodeID string) map[string]bool {
	ancestors := map[string]bool{}
	queue := []string{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, edge := range graph.Edges {
			if edge.S("target") != current {
				continue
			}
			source := edge.S("source")
			producer, known := graph.ByNode[source]
			if !known || producer.Container != graph.ByNode[nodeID].Container {
				continue
			}
			if source == nodeID || ancestors[source] {
				continue
			}
			ancestors[source] = true
			queue = append(queue, source)
		}
	}
	return ancestors
}

// workflowBodyFailure names the first failed body member in document order with its committed reason.
func workflowBodyFailure(body workflowGraphView, states map[string]workflowNodeState) (reason, detail string) {
	for _, member := range body.Nodes {
		state := states[member.ID]
		if state.Status != "failed" {
			continue
		}
		reason := state.Output.S("reason")
		if member.Kind == "agent" {
			reason = state.Error
		}
		if reason == "" {
			reason = "node failed"
		}
		return reason, state.Error
	}
	return "node failed", ""
}

// workflowBodyInFlight reports whether the active round still owns a member the pass must wait for: a
// running body agent session or a retrying one. The round cannot be judged complete until it settles.
func workflowBodyInFlight(body workflowGraphView, states map[string]workflowNodeState) bool {
	for _, member := range body.Nodes {
		switch states[member.ID].Status {
		case "running", "retrying":
			return true
		}
	}
	return false
}

// workflowLoopExitSucceeded reports whether the round selected and completed a loopExit member.
func workflowLoopExitSucceeded(body workflowGraphView, states map[string]workflowNodeState) bool {
	for _, member := range body.Nodes {
		if member.Kind == "loopExit" && states[member.ID].Status == "succeeded" {
			return true
		}
	}
	return false
}

// cancelWorkflowLoops marks every in-flight loop cancelled when the run itself is being cancelled.
// A cancelled run is terminal, so a loop left `running` would misreport a container that will never
// advance; the loop state keeps its rounds as the audit trail of what did run.
func cancelWorkflowLoops(states map[string]workflowNodeState) {
	for id := range states {
		state := states[id]
		if state.Status != "running" || state.Loop == nil {
			continue
		}
		state.Status = "cancelled"
		state.FinishedAt = nowRFC3339()
		states[id] = state
	}
}

// failWorkflowLoop commits one loop failure: the node's stable reason, a readable detail, and the
// round archive as it stood. The run settles failed from this committed state, never from the pass.
func failWorkflowLoop(states map[string]workflowNodeState, node workflowNodeView, reason, detail string) {
	state := states[node.ID]
	if state.DisplayName == "" {
		state.DisplayName = workflowNodeDisplayName(node)
	}
	if state.StartedAt == "" {
		state.StartedAt = nowRFC3339()
	}
	state.Status = "failed"
	state.FinishedAt = nowRFC3339()
	state.Error = detail
	state.Output = Object{"reason": reason}
	states[node.ID] = state
}

// finishWorkflowLoop archives the final round, keeps its member states visible, and publishes the
// loop's named outputs without extending the durable D1 progress shape.
func finishWorkflowLoop(
	states map[string]workflowNodeState,
	node workflowNodeView,
	state *workflowNodeState,
	body workflowGraphView,
	outputs Object,
) {
	if state.Loop != nil {
		archiveWorkflowLoopRound(state.Loop, body, states, true)
	}
	state.Status = "succeeded"
	state.FinishedAt = nowRFC3339()
	state.Error = ""
	state.Output = outputs
	states[node.ID] = *state
}

// archiveWorkflowLoopRound records the active round's member states as one durable entry. When the
// loop continues, the members are removed from the flat map so the next round starts from nothing;
// when the loop finishes they stay, because they are the round the viewer should see.
func archiveWorkflowLoopRound(loop *workflowLoopRunState, body workflowGraphView, states map[string]workflowNodeState, keep bool) {
	workflowInactiveStates(body, states)
	archived := Object{}
	for _, member := range body.Nodes {
		state, ok := states[member.ID]
		if !ok {
			continue
		}
		archived[member.ID] = state.object()
		if !keep {
			delete(states, member.ID)
		}
	}
	carried := Object{}
	for name, value := range loop.Carried {
		carried[name] = value
	}
	loop.Completed = append(loop.Completed, Object{
		"index":   loop.Round,
		"carried": carried,
		"states":  archived,
	})
}
