// Package workflowhost runs v2 workflow runs in this process: it adapts the store to the
// workflow engine, drives Agent nodes through the shared execution service and implements the
// HTTP layer's run lifecycle. Runs never outlive the process; startup interrupts what a previous
// process left executing.
package workflowhost

import (
	"context"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// repository adapts the store's engine transactions to workflow.Repository. It only converts
// types: every transition keeps the store's single-transaction semantics.
type repository struct{ s *store.Store }

var _ workflow.Repository = repository{}

func (r repository) RunContext(ctx context.Context, runID string) (workflow.RunContext, error) {
	c, err := r.s.WorkflowEngineContext(ctx, runID)
	if err != nil {
		return workflow.RunContext{}, err
	}
	return workflow.RunContext{
		RunID: c.Run.ID, WorkflowID: c.Run.WorkflowID, ProjectID: c.Run.ProjectID, Status: c.Run.Status,
		Input: c.Run.Input, Payload: c.Run.RuntimePayload, Graph: c.Graph,
	}, nil
}

func nodeRun(n store.WorkflowNodeRun) workflow.NodeRun {
	return workflow.NodeRun{
		ID: n.ID, NodeID: n.NodeID, NodeType: n.NodeType, Status: n.Status,
		Iteration: n.Iteration, Output: n.Output, SessionID: n.SessionID, FinishedAt: n.FinishedAt,
	}
}

func (r repository) NodeRuns(ctx context.Context, runID string) ([]workflow.NodeRun, error) {
	rows, err := r.s.WorkflowEngineNodeRuns(ctx, runID)
	if err != nil {
		return nil, err
	}
	runs := make([]workflow.NodeRun, len(rows))
	for i, row := range rows {
		runs[i] = nodeRun(row)
	}
	return runs, nil
}

func engineStart(in workflow.NodeStart) store.WorkflowEngineStart {
	return store.WorkflowEngineStart{NodeID: in.NodeID, NodeType: in.NodeType, Input: in.Input}
}

func (r repository) StartRun(ctx context.Context, runID string, entry workflow.NodeStart, prepare func([]byte) ([]byte, error)) (bool, error) {
	return r.s.StartWorkflowEngineRun(ctx, runID, engineStart(entry), prepare)
}

func (r repository) StartNodes(ctx context.Context, runID string, wave []workflow.NodeStart) ([]workflow.NodeRun, error) {
	starts := make([]store.WorkflowEngineStart, len(wave))
	for i, in := range wave {
		starts[i] = engineStart(in)
	}
	rows, err := r.s.StartWorkflowEngineNodes(ctx, runID, starts)
	if err != nil {
		return nil, err
	}
	created := make([]workflow.NodeRun, len(rows))
	for i, row := range rows {
		created[i] = nodeRun(row)
	}
	return created, nil
}

func (r repository) CompleteNode(ctx context.Context, runID, nodeRunID string, output *string, keys map[string]any, fold func([]byte, workflow.NodeRun) ([]byte, error)) (bool, error) {
	return r.s.CompleteWorkflowEngineNode(ctx, runID, nodeRunID, output,
		func(node []byte) ([]byte, error) {
			if len(keys) == 0 {
				return node, nil
			}
			return workflow.MergePayload(node, keys)
		},
		func(run []byte, node store.WorkflowNodeRun) ([]byte, error) { return fold(run, nodeRun(node)) })
}

// FailNode stores the failure detail under the attempt payload's error_detail key, as desktop's
// fail_node_run, keeping every other key the attempt already carried.
func (r repository) FailNode(ctx context.Context, runID, nodeRunID, message string, output *string, detail func(int, []byte) map[string]any) (bool, error) {
	return r.s.FailWorkflowEngineNode(ctx, runID, nodeRunID, message, output, func(attempt int, node, run []byte) ([]byte, error) {
		return workflow.MergePayload(node, map[string]any{"error_detail": detail(attempt, run)})
	})
}

func (r repository) AbandonNode(ctx context.Context, runID, nodeRunID string) (bool, error) {
	return r.s.AbandonWorkflowEngineNode(ctx, runID, nodeRunID)
}

func (r repository) FinishRun(ctx context.Context, runID string, output *string) error {
	return r.s.FinishWorkflowEngineRun(ctx, runID, output)
}

func (r repository) UpdateRunInput(ctx context.Context, runID string, input *string, fold func([]byte) ([]byte, error)) error {
	return r.s.UpdateWorkflowEngineRunInput(ctx, runID, input, fold)
}
