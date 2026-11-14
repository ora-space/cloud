package workflowhost

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

func TestResolveExecutorRequiresRequestedModel(t *testing.T) {
	for _, tc := range []struct {
		name, agent, model, executor string
		failure                      *workflow.Failure
	}{
		{name: "ID", agent: "official/ora-space.opencode", model: "other", executor: "opencode:other"},
		{name: "name", agent: "official/ora-space.opencode", model: "Other Model", executor: "opencode:other"},
		{name: "default explicitly requested", agent: "official/ora-space.opencode", model: "fixture", executor: "opencode:fixture"},
		{name: "not advertised", agent: "official/ora-space.opencode", model: "missing"},
		{name: "not selected", agent: "official/ora-space.opencode", model: "", failure: &workflow.Failure{Kind: workflow.FailureWorkflowModelNotFound, Message: "model is not advertised by agent official/ora-space.opencode"}},
		{name: "another Agent's model", agent: "official/ora-space.opencode", model: "messages:fixture"},
		{name: "case sensitive", agent: "official/ora-space.opencode", model: "OTHER"},
		{name: "no executor", agent: "uninstalled", model: "fixture", failure: &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "agent uninstalled is not available in v2"}},
		{name: "no Agent", model: "fixture", failure: &workflow.Failure{Kind: workflow.FailureMissingAgentRef, Message: "the node selects no Agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Host{ctx: context.Background(), catalog: newFakeExecution()}
			got, failure := h.resolveExecutor(execution.Scope{}, workflow.AgentConfig{AgentCLI: tc.agent, ModelID: tc.model})
			wantFailure := tc.failure
			if tc.executor == "" && wantFailure == nil {
				wantFailure = &workflow.Failure{Kind: workflow.FailureWorkflowModelNotFound, Message: fmt.Sprintf("model %s is not advertised by agent %s", tc.model, tc.agent)}
			}
			if !reflect.DeepEqual(failure, wantFailure) || got.executor.ID != tc.executor {
				t.Fatalf("resolve = %+v, %+v; want executor %q, failure %+v, no fallback", got, failure, tc.executor, wantFailure)
			}
		})
	}
}

func TestMissingModelFailsNodeWithoutStartingFallback(t *testing.T) {
	f := newFixture(t)
	graph := strings.Replace(twoAgents, `"modelId":"fixture"`, `"modelId":"missing-model"`, 1)
	runID := f.run(t, graph, str("input"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("draft"))
	run, nodes := f.await(t, runID, terminal)
	b := node(nodes, "b")
	var payload struct {
		ErrorDetail struct {
			Kind, Message string
			AutoRetryable bool `json:"auto_retryable"`
			Resumable     bool
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(b.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	want := "model missing-model is not advertised by agent official/ora-space.opencode"
	if run.Status != store.WorkflowStatusFailed || b.Status != store.WorkflowStatusFailed || payload.ErrorDetail.Kind != "workflow_model_not_found" || payload.ErrorDetail.Message != want || payload.ErrorDetail.AutoRetryable || !payload.ErrorDetail.Resumable || b.SessionID != nil {
		t.Fatalf("missing model: run=%+v node=%+v payload=%s", run, b, b.Payload)
	}
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	if len(f.exec.started) != 1 {
		t.Fatalf("started %d sessions, want only the preceding Agent", len(f.exec.started))
	}
}

func TestMissingModelFailureFlags(t *testing.T) {
	detail := (workflow.Failure{Kind: workflow.FailureWorkflowModelNotFound}).Detail(1, true, time.Time{})
	for key, want := range map[string]bool{"resumable": true, "auto_retryable": false, "injects_previous_failure": false} {
		if detail[key] != want {
			t.Errorf("%s = %v, want %v", key, detail[key], want)
		}
	}
}
