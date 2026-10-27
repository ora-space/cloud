package core

import (
	"context"
	"testing"
)

// Both first dispatch and retry must render from the body's imported variables, not
// the flat persisted state where the running container has no assigned output yet.
func TestWorkflowAgentPromptUsesLoopScopeOnRetry(t *testing.T) {
	store := dispatcherDB(t)
	config := validLoopConfig()
	objectsOf(config["variables"])[0].O("initial")["value"] = "draft"
	graph := loopTestGraph(config)
	for _, node := range objectsOf(graph["nodes"]) {
		if node.S("id") == "worker" {
			node.O("data").O("agentConfig")["prompt"] = "Improve {{#loop-1.draft#}}"
		}
	}
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, graph)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := settleSessionWorkspace(t, store, runID)
	node, spec := startSessionNode(t, store, runID, ws, "worker")
	if got := objectsOf(spec.O("initialTurn")["content"])[0].S("text"); got != "Improve draft" {
		t.Fatalf("first prompt = %q", got)
	}
	dispatchSessionAttempt(t, store, runID, "worker", node, "loop-first", spec)
	reportSessionEnd(t, store, runID, "loop-first", node, "agent_failed")
	makeWorkflowRetryDue(t, store, runID, "worker")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry := sessionWorkSpec(t, store, runID, "worker")
	if got := objectsOf(retry.O("initialTurn")["content"])[0].S("text"); got != "Improve draft" {
		t.Fatalf("retry prompt = %q", got)
	}
}
