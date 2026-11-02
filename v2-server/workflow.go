package main

import (
	"fmt"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
	"github.com/wanglongan587/cloud/v2-server/internal/workflowhost"
)

// withWorkflowDomain injects the desktop-equivalent workflow rules into the HTTP layer, which
// must not import the domain package itself, and the run host that executes runs together with
// the hub its committed changes are announced on. A nil host keeps run start, cancel and input,
// and the run events stream, unavailable.
func withWorkflowDomain(o server.Options, runs *workflowhost.Host) server.Options {
	o.AnalyzeWorkflow = analyzeWorkflow
	// Publish already reports a refusal as store.ErrWorkflowDocumentInvalid.
	o.ValidateWorkflow = workflow.ValidateWorkflow
	if runs != nil {
		o.WorkflowRuns = runs
		o.WorkflowRunEvents = runs.Hub()
	}
	return o
}

// analyzeWorkflow reports a document the domain cannot parse as store.ErrWorkflowDocumentInvalid,
// so analyze answers workflow_run_graph_parse like desktop instead of an internal error.
func analyzeWorkflow(graph string) (server.WorkflowAnalysis, error) {
	analysis, err := workflow.AnalyzeWorkflow(graph)
	if err != nil {
		return server.WorkflowAnalysis{}, fmt.Errorf("%w: %w", store.ErrWorkflowDocumentInvalid, err)
	}
	unrecognized := make([]server.UnrecognizedWorkflowNode, 0, len(analysis.UnrecognizedNodes))
	for _, node := range analysis.UnrecognizedNodes {
		unrecognized = append(unrecognized, server.UnrecognizedWorkflowNode{NodeID: node.NodeID, Kind: node.Kind})
	}
	return server.WorkflowAnalysis{UnusedNodeIDs: analysis.UnusedNodeIDs, UnrecognizedNodes: unrecognized}, nil
}
