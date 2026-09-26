package simulator

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/wanglongan587/cloud/internal/controlgrpc"
	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// clone drives the clone step the way a Controller does: register the execution before the Node
// sees it, let the Workspace's Node run it, record the Node's outcome, and park the operation on a
// failure. An execution already registered without a result is resumed, never registered twice.
func (c *Controller) clone(ctx context.Context, snap core.Object, workspaces, nodes []core.Object) error {
	if c.Executions == nil {
		return fmt.Errorf("simulator controller has no execution client for the clone step")
	}
	wid := c.Operation.S("workspaceId")
	var workspace, node core.Object
	for _, w := range workspaces {
		if w.S("id") == wid {
			workspace = w
		}
	}
	for _, n := range nodes {
		if n.S("workspaceId") == wid && n["endedAt"] == nil {
			node = n
		}
	}
	if workspace == nil || node == nil {
		return fmt.Errorf("clone step without workspace or node")
	}
	clones, e := objects(snap, "clones")
	if e != nil {
		return e
	}
	input := core.Object{"workspaceId": wid, "repositoryUrl": snap.O("project").S("repositoryUrl"), "branch": workspace.S("requestedRef")}
	ctx = metadata.AppendToOutgoingContext(ctx, controlgrpc.HolderMetadata, c.Client.Subject)
	execution := ""
	for _, clone := range clones {
		if clone["result"] == nil {
			execution = clone.S("executionId")
		}
	}
	if execution == "" {
		execution = uuid.NewString()
		spec := &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: &controlpb.CloneSpec{Repository: input.S("repositoryUrl"), Branch: input.S("branch")}}}
		if _, e = c.Executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "dispatch-" + execution, Epoch: c.Epoch, OperationId: c.Operation.S("id"), ExecutionId: execution, NodeId: node.S("nodeId"), Input: spec}); e != nil {
			return fmt.Errorf("record clone dispatch: %w", e)
		}
	}
	outcome, status, e := c.external(ctx, "PUT", "/clones/"+execution, input)
	if e != nil || status != http.StatusOK {
		return errors.Join(fmt.Errorf("simulated node clone: HTTP %d", status), e)
	}
	result := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node.S("nodeId"), NodeIncarnationId: node.S("nodeIncarnationId")}}
	if outcome.S("outcome") == "clone_ready" {
		result.Outcome = &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: outcome.S("path"), Commit: outcome.S("commit")}}
	} else {
		reason := controlpb.CloneFailureReason(controlpb.CloneFailureReason_value[outcome.S("reason")])
		result.Outcome = &controlpb.ExecutionResult_CloneFailed{CloneFailed: &controlpb.CloneFailed{Reason: reason}}
	}
	if _, e = c.Executions.RecordQueriedResult(ctx, &controlpb.RecordQueriedResultRequest{SubmissionId: "result-" + execution, Epoch: c.Epoch, OperationId: c.Operation.S("id"), ExecutionId: execution, Result: result}); e != nil {
		return fmt.Errorf("record clone result: %w", e)
	}
	if outcome.S("outcome") == "clone_ready" {
		return nil
	}
	_, deferErr := c.command(ctx, "/defer", core.Object{"state": "retry_wait", "errorCode": "clone_failed", "retrySeconds": 1})
	if deferErr == nil {
		c.Operation = nil
	}
	return errors.Join(fmt.Errorf("simulated node clone failed: %s", outcome.S("reason")), deferErr)
}
