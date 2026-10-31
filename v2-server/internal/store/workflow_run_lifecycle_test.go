package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func startedRun(t *testing.T, s *store.Store, user, tenant, workflowName string) (store.WorkflowRun, string) {
	t.Helper()
	ctx := context.Background()
	project := workflowProject(t, s, user, tenant, "project-"+workflowName)
	w, _ := publishedWorkflow(t, s, tenant, workflowName, "v1")
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, "", store.WorkflowRunInput{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowRun(ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	return run, project.ID
}

func TestWorkflowNodeAttemptLifecycle(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "attempt-owner")
	ctx := context.Background()
	run, _ := startedRun(t, s, user, tenant, "尝试")
	node, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}

	// Session binding is one-time while the attempt runs.
	bound, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, node.ID, "session-1")
	if err != nil || bound.SessionID == nil || *bound.SessionID != "session-1" {
		t.Fatalf("bind = %+v %v", bound, err)
	}
	if again, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, node.ID, "session-1"); err != nil || again.SessionID == nil || *again.SessionID != "session-1" {
		t.Fatalf("idempotent bind = %+v %v", again, err)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, node.ID, "session-2"); !errors.Is(err, store.ErrWorkflowNodeRunState) {
		t.Fatalf("rebind = %v", err)
	}
	if _, err := s.BindWorkflowNodeSession(ctx, tenant, run.ID, "00000000-0000-0000-0000-000000000000", "session-1"); !errors.Is(err, store.ErrWorkflowNodeRunNotFound) {
		t.Fatalf("bind missing node = %v", err)
	}

	output := "最终回答"
	payload := []byte(`{"stop_reason": "end_turn"}`)
	completed, err := s.CompleteWorkflowNodeRun(ctx, tenant, run.ID, node.ID, store.WorkflowNodeResult{Output: &output, Payload: payload})
	if err != nil || completed.Status != store.WorkflowStatusSucceeded || completed.FinishedAt == nil {
		t.Fatalf("complete = %+v %v", completed, err)
	}
	if completed.Output == nil || *completed.Output != output || string(completed.Payload) != string(payload) {
		t.Fatalf("completed row = %+v", completed)
	}
	// A late duplicate completion cannot rewrite the answer.
	other := "迟到的回答"
	if late, err := s.CompleteWorkflowNodeRun(ctx, tenant, run.ID, node.ID, store.WorkflowNodeResult{Output: &other}); err != nil {
		t.Fatal(err)
	} else if late.Output == nil || *late.Output != output {
		t.Fatalf("late completion rewrote output: %+v", late)
	}

	// A second attempt on another identity fails with the attempt number the domain writes into
	// its failure detail.
	failing, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-2", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	var builtAttempt int
	failed, err := s.FailWorkflowNodeRun(ctx, tenant, run.ID, failing.ID, store.WorkflowNodeFailure{Error: "boom"},
		func(attempt int) ([]byte, error) {
			builtAttempt = attempt
			return []byte(`{"error_detail":{"attempt":1}}`), nil
		})
	if err != nil || failed.Status != store.WorkflowStatusFailed || failed.FinishedAt == nil {
		t.Fatalf("fail = %+v %v", failed, err)
	}
	if builtAttempt != 1 {
		t.Fatalf("attempt number = %d", builtAttempt)
	}
	if _, err := s.FailWorkflowNodeRun(ctx, tenant, run.ID, failing.ID, store.WorkflowNodeFailure{Error: "again"}, nil); !errors.Is(err, store.ErrWorkflowNodeRunState) {
		t.Fatalf("double fail = %v", err)
	}
}

func TestWorkflowRunTerminalTransitions(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "terminal-owner")
	ctx := context.Background()

	run, _ := startedRun(t, s, user, tenant, "终态")
	if _, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"}); err != nil {
		t.Fatal(err)
	}
	output := "完成"
	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, "succeeded", &output, nil); err != nil {
		t.Fatal(err)
	}
	finished, err := s.WorkflowRun(ctx, tenant, run.ID)
	if err != nil || finished.Status != store.WorkflowStatusSucceeded || finished.FinishedAt == nil {
		t.Fatalf("finished run = %+v %v", finished, err)
	}
	if finished.State == nil || string(finished.State) != `{"current_nodes": []}` {
		t.Fatalf("terminal anchor = %s", finished.State)
	}
	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, "succeeded", &output, nil); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("double finish = %v", err)
	}
	if _, err := s.FinishWorkflowRun(ctx, tenant, run.ID, "cancelled", nil, nil); err == nil {
		t.Fatal("finish accepted cancelled")
	}

	// Cancelling closes the run and its non-terminal attempt.
	cancelledRun, _ := startedRun(t, s, user, tenant, "取消")
	cancelledNode, err := s.CreateWorkflowNodeRun(ctx, tenant, cancelledRun.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelWorkflowRun(ctx, tenant, cancelledRun.ID)
	if err != nil || cancelled.Status != store.WorkflowStatusCancelled || cancelled.FinishedAt == nil {
		t.Fatalf("cancel = %+v %v", cancelled, err)
	}
	nodes, err := s.WorkflowRunNodes(ctx, tenant, cancelledRun.ID)
	if err != nil || len(nodes) != 1 || nodes[0].Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancelled nodes = %+v %v", nodes, err)
	}
	if nodes[0].ID != cancelledNode.ID {
		t.Fatalf("wrong node cancelled")
	}
	if again, err := s.CancelWorkflowRun(ctx, tenant, cancelledRun.ID); err != nil || again.Status != store.WorkflowStatusCancelled {
		t.Fatalf("idempotent cancel = %+v %v", again, err)
	}
	if _, err := s.CancelWorkflowRun(ctx, tenant, run.ID); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("cancel finished run = %v", err)
	}
}

func TestWorkflowLoopRoundScopes(t *testing.T) {
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "loop-owner")
	ctx := context.Background()
	run, _ := startedRun(t, s, user, tenant, "轮次")
	loop, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "loop-1", NodeType: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"conditionDecisions":{}}`)
	round, err := s.StartWorkflowLoopRound(ctx, tenant, run.ID, loop.ID, 1, state)
	if err != nil || round.Status == nil || *round.Status != store.WorkflowStatusRunning {
		t.Fatalf("start round = %+v %v", round, err)
	}
	if round.ParentLoopNodeRunID == nil || *round.ParentLoopNodeRunID != loop.ID || round.RoundIndex == nil || *round.RoundIndex != 1 {
		t.Fatalf("round identity = %+v", round)
	}
	// One active round per Loop, even when the next index differs.
	if _, err := s.StartWorkflowLoopRound(ctx, tenant, run.ID, loop.ID, 2, state); !errors.Is(err, store.ErrWorkflowScopeExists) {
		t.Fatalf("second active round = %v", err)
	}
	if _, err := s.StartWorkflowLoopRound(ctx, tenant, run.ID, "00000000-0000-0000-0000-000000000000", 1, state); !errors.Is(err, store.ErrWorkflowNodeRunNotFound) {
		t.Fatalf("missing parent = %v", err)
	}
	agent, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartWorkflowLoopRound(ctx, tenant, run.ID, agent.ID, 1, state); !errors.Is(err, store.ErrWorkflowNodeRunState) {
		t.Fatalf("non-loop parent = %v", err)
	}

	settled, err := s.SettleWorkflowLoopRound(ctx, tenant, run.ID, round.ID, store.WorkflowStatusSucceeded, []byte(`{"rounds":[1]}`))
	if err != nil || settled.Status == nil || *settled.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("settle round = %+v %v", settled, err)
	}
	if again, err := s.SettleWorkflowLoopRound(ctx, tenant, run.ID, round.ID, store.WorkflowStatusSucceeded, []byte(`{}`)); err != nil {
		t.Fatalf("idempotent settle = %v", err)
	} else if again.State == nil || string(again.State) != `{"rounds": [1]}` {
		t.Fatalf("duplicate settle rewrote state: %s", again.State)
	}
	if _, err := s.SettleWorkflowLoopRound(ctx, tenant, run.ID, round.ID, store.WorkflowStatusFailed, nil); !errors.Is(err, store.ErrWorkflowScopeState) {
		t.Fatalf("conflicting settle = %v", err)
	}

	// After settlement the next round may open.
	next, err := s.StartWorkflowLoopRound(ctx, tenant, run.ID, loop.ID, 2, state)
	if err != nil {
		t.Fatalf("start next round = %v", err)
	}
	if next.RoundIndex == nil || *next.RoundIndex != 2 {
		t.Fatalf("next round = %+v", next)
	}
	scopes, err := s.WorkflowRunScopes(ctx, tenant, run.ID)
	if err != nil || len(scopes) != 2 {
		t.Fatalf("round list = %+v %v", scopes, err)
	}
}
