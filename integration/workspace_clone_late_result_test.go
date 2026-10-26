package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// A definitive original result changes knowledge, not execution identity or the creation budget.
func TestKnownCloneResultCannotRemainParkedAsUnknown(t *testing.T) {
	for _, order := range []string{"result_after_defer", "result_before_defer", "query_after_defer"} {
		t.Run(order, func(t *testing.T) {
			f, c, run, workspace, op, result := dispatchedCloneReceiptScene(t)
			result.Result.Outcome = &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: "/checkout", Commit: f.commit}}
			result.Event = []byte("original clone ready")
			accept := func() {
				t.Helper()
				if order == "query_after_defer" {
					_, err := f.executions.RecordQueriedResult(c.ctx, &controlpb.RecordQueriedResultRequest{
						SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), ExecutionId: result.GetExecutionId(), Result: result.GetResult(),
					})
					must(t, err)
					return
				}
				_, err := f.executions.TakeOverNodeEvent(c.ctx, result)
				must(t, err)
			}
			if order == "result_before_defer" {
				accept()
			}
			_, err := c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{
				SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(),
				State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_CLONE_RESULT_UNKNOWN, RetrySeconds: 1,
			})
			must(t, err)
			var before string
			must(t, f.store.Pool.QueryRow("SELECT coalesce(workspace_deadline_at::text,'') FROM issue_runs WHERE id=$1", run).Scan(&before))
			if order != "result_before_defer" {
				accept()
			}
			var state, after string
			must(t, f.store.Pool.QueryRow("SELECT state FROM operations WHERE id=$1", op.GetId()).Scan(&state))
			must(t, f.store.Pool.QueryRow("SELECT coalesce(workspace_deadline_at::text,'') FROM issue_runs WHERE id=$1", run).Scan(&after))
			if state != "queued" || before != after || f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", op.GetId()) != 1 {
				t.Fatalf("known clone stayed parked, changed deadline or duplicated execution: state=%s deadline=%q -> %q", state, before, after)
			}
			if f.scalar("SELECT count(*) FROM workspaces WHERE id=$1 AND admission_open", workspace) != 0 {
				t.Fatal("receipt opened admission before lifecycle advancement")
			}
			snapshot := c.claim()
			if snapshot.GetOperation().GetId() != op.GetId() || len(snapshot.GetClones()) != 1 || !proto.Equal(snapshot.GetClones()[0].GetResult(), result.GetResult()) {
				t.Fatalf("claim lost original known execution: %v", snapshot)
			}
			_, err = c.advance(snapshot.GetOperation())
			must(t, err)
			if f.scalar("SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2", workspace, f.commit) != 1 {
				t.Fatal("same result did not advance the frozen checkout baseline")
			}
		})
	}
}

// Expiry and business abandonment remain authoritative even if the original clone later finishes.
func TestLateCloneResultDoesNotRenewOrReopenExpiredCreation(t *testing.T) {
	for _, state := range []string{"expired", "abandoned", "force_stopping", "other_block_reason"} {
		t.Run(state, func(t *testing.T) {
			f, c, run, workspace, op, result := dispatchedCloneReceiptScene(t)
			_, err := c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{
				SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(),
				State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_CLONE_RESULT_UNKNOWN, RetrySeconds: 1,
			})
			must(t, err)
			switch state {
			case "expired", "abandoned":
				_, err = f.store.Pool.Exec("UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1", run)
				must(t, err)
				if state == "abandoned" {
					must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
				}
			case "force_stopping":
				// This is the pre-existing independent force-stop intent, not a new authorization.
				_, err = f.store.Pool.Exec(`INSERT INTO runtime_force_stops(id,tenant_id,workspace_id,actor_user_id,
 reason,state,control_epoch,runtime_generation)
 SELECT $2::uuid,w.tenant_id,w.id,w.creator_user_id,'late clone fixture','registered',c.control_epoch,w.runtime_generation
 FROM workspaces w JOIN runtime_controls c ON c.workspace_id=w.id WHERE w.id=$1`, workspace, uuid.NewString())
				must(t, err)
			case "other_block_reason":
				_, err = f.store.Pool.Exec("UPDATE operations SET error_code='node_unavailable' WHERE id=$1", op.GetId())
				must(t, err)
			}
			result.Result.Outcome = &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: "/checkout", Commit: f.commit}}
			result.Event = []byte("late original clone ready")
			_, err = f.executions.TakeOverNodeEvent(c.ctx, result)
			must(t, err)
			var actual string
			must(t, f.store.Pool.QueryRow("SELECT state FROM operations WHERE id=$1", op.GetId()).Scan(&actual))
			want := "blocked"
			if state == "abandoned" {
				want = "failed"
			}
			if actual != want {
				t.Fatalf("late result reopened %s creation: got %s want %s", state, actual, want)
			}
		})
	}
}
