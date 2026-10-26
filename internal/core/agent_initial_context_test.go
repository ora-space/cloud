package core

import (
	"strings"
	"testing"
)

func TestInitialTurnIncludesFrozenTaskContext(t *testing.T) {
	input := Object{
		"task":              "implement the agreed change",
		"issue":             Object{"title": "Login", "description": "Use the existing identity service"},
		"recentComments":    []string{"Preserve keyboard navigation", "Include error recovery"},
		"interactionValues": Object{},
		"target":            Object{"type": "agent", "id": "agent-a"},
		"contextRefs":       []Object{{"refType": "project", "refId": "project-a"}},
	}
	resume := Object{"revisionId": "revision-a"}
	actual := renderAgentInitialTurn(input, resume)
	for _, required := range []string{"implement the agreed change", "Login", "Use the existing identity service", "Preserve keyboard navigation", "Include error recovery", "revision-a", "project-a"} {
		if !strings.Contains(actual, required) {
			t.Errorf("frozen context %q missing from initial turn", required)
		}
	}
	if again := renderAgentInitialTurn(input, resume); actual != again {
		t.Fatal("rendering the same frozen context must be deterministic")
	}
}

func TestInitialTurnDoesNotInventAbsentHistoricalContext(t *testing.T) {
	actual := renderAgentInitialTurn(Object{"task": "legacy task"}, nil)
	if strings.Contains(actual, `"issue"`) || strings.Contains(actual, `"recentComments"`) {
		t.Fatal("legacy snapshots must not pretend to contain missing context")
	}
}
