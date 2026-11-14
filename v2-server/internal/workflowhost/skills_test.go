package workflowhost

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestWorkflowFreezesSelectedSkillAndClearsItForNextNode(t *testing.T) {
	f := newFixture(t)
	_, err := f.s.CreateSkill(context.Background(), f.scope.TenantID, f.scope.UserID, store.SkillInput{Name: "selected-review", Description: "Review", Content: "UNIQUE_REVIEW_INSTRUCTION"})
	if err != nil {
		t.Fatal(err)
	}
	graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, `"skills":[{"skillId":"selected-review","enabled":true},{"skillId":"missing-disabled","enabled":false}]`, 1)
	runID := f.run(t, graph, str("review"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	request := f.exec.request(a)
	if request.Skills == nil || len(request.Skills.Documents) != 1 || request.Skills.Documents[0].Content != "UNIQUE_REVIEW_INSTRUCTION" {
		t.Fatalf("not frozen: %+v", request.Skills)
	}
	if !strings.Contains(promptText(t, request.Prompt), "<required_skills>") {
		t.Fatal("missing required skill instructions")
	}
	f.exec.finish(a, completed("reviewed"))
	b := f.exec.awaitStart(t)
	next := f.exec.request(b)
	if next.Skills == nil || len(next.Skills.Documents) != 0 || len(next.Skills.Plugins) != 0 {
		t.Fatalf("next node inherited skills: %+v", next.Skills)
	}
	f.exec.finish(b, completed("done"))
	f.await(t, runID, terminal)
}

func TestWorkflowRejectsMissingSkillBeforeExecution(t *testing.T) {
	for _, config := range []string{`"skills":[{"skillId":"missing-skill","enabled":true}]`} {
		t.Run(config, func(t *testing.T) {
			f := newFixture(t)
			graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, config, 1)
			runID := f.run(t, graph, str("check"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "missing_agent_config" {
				t.Fatalf("not refused: %+v", run)
			}
			f.exec.mu.Lock()
			defer f.exec.mu.Unlock()
			if len(f.exec.started) != 0 {
				t.Fatal("started despite missing dependency")
			}
		})
	}
}

func TestWorkflowRunsWithMCPAndReportsOnlyEnabledBindingsUnapplied(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newFixture(t)
			config := fmt.Sprintf(`"mcps":[{"mcpId":"official/tavily","enabled":%t},{"mcpId":"demo/other","enabled":%t}]`, enabled, enabled)
			graph := strings.Replace(twoAgents, `"roleId":"reviewer"`, config, 1)
			runID := f.run(t, graph, str("check"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			a := f.exec.awaitStart(t)
			if strings.Contains(promptText(t, f.exec.request(a).Prompt), "official/tavily") {
				t.Fatal("MCP binding leaked into the Agent prompt")
			}
			f.exec.finish(a, completed("reviewed"))
			f.exec.finish(f.exec.awaitStart(t), completed("done"))
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"review":"done"}` {
				t.Fatalf("MCP configuration prevented completion: %+v", run)
			}
			for _, id := range []string{"a", "b"} {
				var payload struct {
					Unapplied []string `json:"unapplied"`
				}
				if err := json.Unmarshal(node(nodes, id).Payload, &payload); err != nil {
					t.Fatal(err)
				}
				want := enabled && id == "a"
				// Neither node sets a retry policy, so desktop's default retry is listed after MCP.
				if slices.Contains(payload.Unapplied, "mcp") != want || (want && !slices.Equal(payload.Unapplied, []string{"mcp", "retry"})) {
					t.Fatalf("node %s unapplied = %v, want MCP = %v", id, payload.Unapplied, want)
				}
			}
		})
	}
}
