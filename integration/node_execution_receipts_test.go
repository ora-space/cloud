package integration

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

// A queried plugin result settles its business fact without acknowledging the Node's outbox.
// Its later real terminal event must establish exactly one receipt, rather than block reconnects.
func TestPluginQueriedResultThenTerminalReceipt(t *testing.T) {
	for _, kind := range []string{"install_plugin", "remove_plugin"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			repo, _ := marketplaceFixture(t, f.root)
			f.syncMarketplace(t, repo)
			sid := f.defaultSpaceID()
			created := f.spaceProject(t, sid, "queried-plugin-receipt")
			wid := created.O("workspace").S("id")
			f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-for-receipt", 200)
			if kind == "remove_plugin" {
				f.completeNextPlugin(t, "install_plugin")
				f.call("DELETE", f.pluginSpacePath(sid)+"/plugins", core.Object{
					"identifier": "official/hello-world", "version": f.spacePlugin(sid, "official/hello-world").N("version"),
				}, "remove-for-receipt", 200)
			}
			op := f.claimPluginOp(t, kind)
			const execution = "queried-plugin-execution"
			f.dispatchPlugin(t, op, execution)
			node, incarnation := f.workspaceNode(t, wid)
			result := &controlpb.ExecutionResult{
				Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation},
				Outcome: &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{
					Items: successPluginItems(op),
				}},
			}
			ctx := asController(f.client.Subject)
			queried, err := f.executions.RecordQueriedResult(ctx, &controlpb.RecordQueriedResultRequest{
				SubmissionId: "query-plugin-terminal", Epoch: f.controller.Epoch,
				OperationId: op.S("id"), ExecutionId: execution, Result: result,
			})
			must(t, err)
			if !proto.Equal(queried.GetRecord().GetResult(), result) ||
				f.scalar("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", execution) != 0 ||
				f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", execution) != 0 {
				t.Fatal("queried terminal must settle the result without fabricating an ACK receipt")
			}
			version := f.scalar("SELECT version FROM workspace_plugin_instances WHERE workspace_id=$1", wid)
			terminal := &controlpb.TakeOverNodeEventRequest{
				SubmissionId: "plugin-terminal-event", Epoch: f.controller.Epoch,
				OperationId: op.S("id"), ExecutionId: execution, Sequence: 1,
				Result: result, Event: []byte("original-plugin-terminal-event"),
			}
			for _, invalid := range []struct {
				name   string
				change func(*controlpb.TakeOverNodeEventRequest)
			}{
				{name: "gap", change: func(req *controlpb.TakeOverNodeEventRequest) { req.Sequence = 2 }},
				{name: "foreign_node", change: func(req *controlpb.TakeOverNodeEventRequest) { req.Result.Node.NodeId = "other-node" }},
				{name: "changed_result", change: func(req *controlpb.TakeOverNodeEventRequest) {
					req.Result.Outcome = &controlpb.ExecutionResult_PluginsFailed{PluginsFailed: &controlpb.PluginsFailed{Reason: controlpb.PluginsFailureReason_PLUGINS_FAILURE_REASON_INTERRUPTED}}
				}},
				{name: "wrong_kind", change: func(req *controlpb.TakeOverNodeEventRequest) {
					req.Result.Outcome = &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED}}
				}},
			} {
				req := proto.Clone(terminal).(*controlpb.TakeOverNodeEventRequest)
				req.SubmissionId = "invalid-" + invalid.name
				invalid.change(req)
				_, err := f.executions.TakeOverNodeEvent(ctx, req)
				expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			}
			accepted, err := f.executions.TakeOverNodeEvent(ctx, terminal)
			must(t, err)
			if !proto.Equal(accepted.GetRecord().GetResult(), result) ||
				f.scalar("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", execution) != 1 ||
				f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", execution) != 1 ||
				f.scalar("SELECT version FROM workspace_plugin_instances WHERE workspace_id=$1", wid) != version {
				t.Fatal("first real terminal must create one receipt without repeating plugin writeback")
			}
			for _, submission := range []string{terminal.SubmissionId, "replay-same-terminal"} {
				req := proto.Clone(terminal).(*controlpb.TakeOverNodeEventRequest)
				req.SubmissionId = submission
				replay, err := f.executions.TakeOverNodeEvent(ctx, req)
				must(t, err)
				if !proto.Equal(replay, accepted) {
					t.Fatal("same terminal receipt must replay the complete response")
				}
			}
			for _, invalid := range []struct {
				name   string
				change func(*controlpb.TakeOverNodeEventRequest)
			}{
				{name: "changed_event", change: func(req *controlpb.TakeOverNodeEventRequest) { req.Event = []byte("different-terminal-event") }},
				{name: "extra_terminal", change: func(req *controlpb.TakeOverNodeEventRequest) { req.Sequence = 2 }},
				{name: "changed_submission", change: func(req *controlpb.TakeOverNodeEventRequest) {
					req.SubmissionId = terminal.SubmissionId
					req.Event = []byte("different-terminal-event")
				}},
			} {
				req := proto.Clone(terminal).(*controlpb.TakeOverNodeEventRequest)
				req.SubmissionId = "invalid-replay-" + invalid.name
				invalid.change(req)
				_, err := f.executions.TakeOverNodeEvent(ctx, req)
				expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			}
			if f.scalar("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", execution) != 1 ||
				f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", execution) != 1 ||
				f.scalar("SELECT version FROM workspace_plugin_instances WHERE workspace_id=$1", wid) != version {
				t.Fatal("replay or refused evidence changed the receipt or repeated plugin writeback")
			}
		})
	}
}

// Sessions and deliveries cannot use a query as a substitute for their ordered terminal receipt.
// A replay at a new sequence must remain a conflict even after the terminal result was committed.
func TestSessionAndDeliveryTerminalsKeepReceiptAuthority(t *testing.T) {
	for _, kind := range []string{"agent_session", "deliver_revision"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			var terminal *controlpb.TakeOverNodeEventRequest
			if kind == "agent_session" {
				run, execution, node, incarnation := f.registeredSession(t, "session-receipt-authority")
				terminal = &controlpb.TakeOverNodeEventRequest{
					SubmissionId: "strict-session-terminal", Epoch: f.controller.Epoch,
					OperationId: run, ExecutionId: execution, Sequence: 1, Event: []byte("session-terminal-event"),
					Result: &controlpb.ExecutionResult{
						Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation},
						Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{
							Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED,
						}},
					},
				}
			} else {
				// Only RevisionFailed is submitted, so this valid-shaped fixture never contacts S3.
				cfg := &objectstore.Config{Endpoint: "http://objects.invalid:9000", Region: "us-east-1", Bucket: "revisions", AccessKeyID: "fixture", SecretAccessKey: "fixture", PathStyle: true}
				_, terminal = f.registeredDelivery(t, cfg)
				terminal.Result.Outcome = &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{
					Reason: controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_SNAPSHOT_FAILED,
				}}
				terminal.Event = []byte("failed-delivery-terminal-event")
			}
			ctx := asController(f.client.Subject)
			query := &controlpb.RecordQueriedResultRequest{
				SubmissionId: "query-without-receipt", Epoch: terminal.Epoch,
				OperationId: terminal.OperationId, ExecutionId: terminal.ExecutionId, Result: terminal.Result,
			}
			_, err := f.executions.RecordQueriedResult(ctx, query)
			expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
			if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result IS NULL AND last_event_sequence=0", terminal.ExecutionId) != 1 {
				t.Fatal("receipt-free query changed a session or delivery terminal fact")
			}
			gap := proto.Clone(terminal).(*controlpb.TakeOverNodeEventRequest)
			gap.SubmissionId, gap.Sequence = "terminal-before-predecessor", 2
			_, err = f.executions.TakeOverNodeEvent(ctx, gap)
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			accepted, err := f.executions.TakeOverNodeEvent(ctx, terminal)
			must(t, err)
			replay := proto.Clone(terminal).(*controlpb.TakeOverNodeEventRequest)
			replay.SubmissionId = "same-terminal-receipt"
			again, err := f.executions.TakeOverNodeEvent(ctx, replay)
			must(t, err)
			if !proto.Equal(again, accepted) {
				t.Fatal("terminal receipt replay changed the response")
			}
			gap.SubmissionId = "new-sequence-after-terminal"
			_, err = f.executions.TakeOverNodeEvent(ctx, gap)
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			_, err = f.executions.RecordQueriedResult(ctx, query)
			expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
			if f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", terminal.ExecutionId) != 1 ||
				f.scalar("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", terminal.ExecutionId) != 1 {
				t.Fatal("terminal query or new sequence altered receipt authority")
			}
		})
	}
}
