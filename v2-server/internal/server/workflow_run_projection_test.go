package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// pendingProjectionRun creates a pending run of a freshly published workflow.
func pendingProjectionRun(t *testing.T, call workflowCall, s *store.Store) (tenant, runID string) {
	t.Helper()
	tenant = workflowTenant(t, call)
	_, me := call("GET", "/api/v1/me", nil)
	project := workflowRunProject(t, s, me["id"].(string), tenant)
	base := "/api/v2/tenants/" + tenant + "/workflows"
	_, created := call("POST", base, map[string]any{"name": "投影", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)
	call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	status, run := call("POST", base+"/"+id+"/runs", map[string]any{"projectId": project.ID})
	if status != 201 {
		t.Fatalf("create run = %d %v", status, run)
	}
	return tenant, run["run"].(map[string]any)["id"].(string)
}

func TestWorkflowRunDetailProjectsTheVariablePool(t *testing.T) {
	call, s := workflowServerWithStore(t, nil)
	tenant, runID := pendingProjectionRun(t, call, s)
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID

	// A run that has not started has no pool yet.
	_, detail := call("GET", path, nil)
	if vars := detail["variables"].([]any); len(vars) != 0 {
		t.Fatalf("variables before start = %v", vars)
	}

	pool := `{"variablePool":{"revision":3,
		"catalog":{"start.topic":{"valueType":"string","writer":"start"},"draft.output":{"valueType":"string","writer":"draft"},
			"start.count":{"valueType":"number","writer":"start"},"start.note":{"valueType":"string","writer":"start"},
			"cond.selected_branch_id":{"valueType":"string","writer":"cond"},"broken":{"valueType":"string","writer":"x"}},
		"values":{"start.topic":"对账","start.count":0,"start.note":null}},
		"conditionDecisions":{}}`
	input := "做个方案"
	if err := s.UpdateWorkflowEngineRunInput(context.Background(), runID, &input, func([]byte) ([]byte, error) { return []byte(pool), nil }); err != nil {
		t.Fatal(err)
	}
	_, detail = call("GET", path, nil)
	got, _ := json.Marshal(detail["variables"])
	// Catalog order, routing entries and malformed keys left out; an unassigned variable has no
	// value, while an assigned null and zero keep theirs.
	want := `[{"selector":["draft","output"],"sourceNodeId":"draft","valueType":"string"},` +
		`{"selector":["start","count"],"sourceNodeId":"start","value":0,"valueType":"number"},` +
		`{"selector":["start","note"],"sourceNodeId":"start","value":null,"valueType":"string"},` +
		`{"selector":["start","topic"],"sourceNodeId":"start","value":"对账","valueType":"string"}]`
	if string(got) != want {
		t.Fatalf("variables = %s\nwant %s", got, want)
	}
}

func TestWorkflowRunDetailListsReplacedFailedAttempts(t *testing.T) {
	call, s := workflowServerWithStore(t, nil)
	tenant, runID := pendingProjectionRun(t, call, s)
	ctx := context.Background()
	path := "/api/v2/tenants/" + tenant + "/workflow-runs/" + runID
	if _, err := s.StartWorkflowRun(ctx, tenant, runID); err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateWorkflowNodeRun(ctx, tenant, runID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, runID, first.ID, "s-1"); err != nil {
		t.Fatal(err)
	}

	// Nothing has been replaced yet: a node's current attempt, failed or not, is its own row.
	_, detail := call("GET", path, nil)
	if attempts := detail["failedAttempts"].([]any); len(attempts) != 0 {
		t.Fatalf("failed attempts before any retry = %v", attempts)
	}

	detailOf := func(attempt int) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"error_detail":{"kind":"session","message":"the Agent turn did not complete","source_chain":["turn failed","boom"],"attempt":%d,"resumable":true,"recorded_at":1700000000000}}`, attempt)), nil
	}
	second, err := s.RetryWorkflowNodeRun(ctx, tenant, runID, first.ID, store.WorkflowNodeRetry{Failure: store.WorkflowNodeFailure{Error: "boom"}, FailurePayload: detailOf})
	if err != nil {
		t.Fatal(err)
	}
	// A replaced row without an error_detail is not one desktop can show; it is left out.
	third, err := s.RetryWorkflowNodeRun(ctx, tenant, runID, second.ID, store.WorkflowNodeRetry{Failure: store.WorkflowNodeFailure{Error: "again"}})
	if err != nil {
		t.Fatal(err)
	}

	_, detail = call("GET", path, nil)
	attempts := detail["failedAttempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("failed attempts = %v", attempts)
	}
	attempt := attempts[0].(map[string]any)
	want := map[string]any{
		"nodeRunId": first.ID, "nodeId": "agent-1", "scopeId": first.ScopeID, "iteration": nil, "sessionId": "s-1",
		"attempt": float64(1), "kind": "session", "message": "the Agent turn did not complete",
		"sourceChain": []any{"turn failed", "boom"}, "recordedAt": float64(1700000000000),
		"startedAt": attempt["startedAt"], "finishedAt": attempt["finishedAt"],
	}
	if !reflect.DeepEqual(attempt, want) || attempt["finishedAt"] == nil {
		t.Fatalf("failed attempt = %v", attempt)
	}
	// nodes still holds only the latest attempt.
	nodes := detail["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["id"] != third.ID {
		t.Fatalf("nodes = %v", nodes)
	}
}
