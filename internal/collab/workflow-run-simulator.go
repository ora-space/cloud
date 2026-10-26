package collab

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// MockWorkflowRunSimulator produces a deterministic execution trace for one workflow run
// without performing any real work. Dev/demo only: WireDevelopmentFixtures installs it, a
// production Store leaves runs pending. Every output carries an explicit "(simulated)" so
// nothing the viewer shows can be read as a real engine's result.
type MockWorkflowRunSimulator struct{}

func (MockWorkflowRunSimulator) SimulateWorkflowRun(graph, input core.Object) (nodeStates core.Object, rounds []core.Object, status string) {
	byID := map[string]core.Object{}
	nodes, _ := graph["nodes"].([]any)
	for _, raw := range nodes {
		if node, ok := raw.(map[string]any); ok {
			o := core.Object(node)
			byID[o.S("id")] = o
		}
	}
	edges, _ := graph["edges"].([]any)
	order := reachableOrder(edges, byID)

	clock := time.Now().UTC()
	nodeStates = core.Object{}
	for _, id := range order {
		node := byID[id]
		data := node.O("data")
		title := data.S("title")
		if title == "" {
			title = node.S("kind")
		}
		isStart := node.O("data").S("kind") == "start"
		startedAt := clock.Add(time.Duration(len(nodeStates)) * time.Second)
		finishedAt := startedAt.Add(time.Second)
		state := core.Object{
			"status":      "succeeded",
			"displayName": title,
			"startedAt":   startedAt.UTC().Format(time.RFC3339),
			"finishedAt":  finishedAt.UTC().Format(time.RFC3339),
		}
		if isStart {
			// The one thing the sim can truthfully report: what it was kicked off with. Match the
			// real Start output shape — `input` is the run's kickoff (the submitted value, else the
			// authored prompt) and each run-form variable is a root — so a UI negative case reads the
			// same pool the scheduler writes.
			output := core.Object{}
			for key, value := range input {
				if key != "input" {
					output[key] = value
				}
			}
			if kickoff, ok := core.WorkflowStartKickoff(data, input); ok {
				output["input"] = kickoff
			}
			state["output"] = output
		} else {
			state["output"] = core.Object{"message": "Simulated " + title + " result (simulated; no real work was performed)."}
		}
		nodeStates[id] = state
	}
	rounds = []core.Object{{
		"id":         uuid.NewString(),
		"number":     1,
		"input":      input,
		"nodeStates": nodeStates,
	}}
	return nodeStates, rounds, "succeeded"
}

// reachableOrder returns node ids in a deterministic order: every start node first
// (sorted), then breadth-first over data edges, each node's followers sorted so two runs
// of the same graph always produce the same trace. Nodes no data edge ever reaches are
// left out, which the viewer renders as idle — exactly how an unreached branch looks.
func reachableOrder(edges []any, byID map[string]core.Object) []string {
	roots := []string{}
	for id, node := range byID {
		if node.O("data").S("kind") == "start" {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)
	adj := map[string][]string{}
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		e := core.Object(edge)
		source, target := e.S("source"), e.S("target")
		if source == "" || target == "" || source == target || byID[target] == nil {
			continue
		}
		adj[source] = append(adj[source], target)
	}
	order := []string{}
	seen := map[string]bool{}
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		order = append(order, id)
		next := append([]string{}, adj[id]...)
		sort.Strings(next)
		queue = append(queue, next...)
	}
	return order
}
