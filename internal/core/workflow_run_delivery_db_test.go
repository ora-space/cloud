package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

type workflowDeliveryScene struct {
	run, workspace, node, first, second string
	work                                Object
}

// A real PostgreSQL scene uses the production scheduler, dispatch, terminal and delivery paths.
func seedWorkflowDelivery(t *testing.T, s *Store) workflowDeliveryScene {
	t.Helper()
	s.ObjectStore = &objectstore.Config{}
	s.GitServer = &GitServer{RepositoryBaseURL: "https://git.ora.test"}
	run, _, _, _, _ := seedWorkflowSessionSceneGraph(t, s, workflowTwoAgentGraph())
	if err := s.AdvanceWorkflowRunsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	workspace := settleSessionWorkspace(t, s, run)
	node, spec := startSessionNode(t, s, run, workspace, "a1")
	if _, err := s.Pool.Exec("UPDATE node_instances SET workflow_history_delivery=true WHERE workspace_id=$1", workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE clone_executions SET input=input||jsonb_build_object('workspaceRef',$2::text) WHERE workspace_id=$1", workspace, GitWorkspaceRefPrefix(workspace)+"main"); err != nil {
		t.Fatal(err)
	}
	first := dispatchSession(t, s, run, "a1", node, spec)
	reportSessionEnd(t, s, run, first, node, "user_ended")
	for range 3 {
		if err := s.AdvanceWorkflowRunsOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	secondSpec := sessionWorkSpec(t, s, run, "a2")
	if secondSpec.S("checkoutExecutionId") != spec.S("checkoutExecutionId") || len(secondSpec.O("priorRevision")) != 0 {
		t.Fatal("workflow changed its own checkout or restored a task predecessor")
	}
	second := dispatchSession(t, s, run, "a2", node, secondSpec)
	reportSessionEnd(t, s, run, second, node, "user_ended")
	for range 3 {
		if err := s.AdvanceWorkflowRunsOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var work Object
	if err := s.view(t.Context(), func(tx *transaction) {
		work = tx.one("SELECT * FROM execution_work WHERE workflow_run_id=$1 AND kind='deliver_revision' AND execution_id IS NULL", run)
	}); err != nil {
		t.Fatal(err)
	}
	if work == nil {
		t.Fatal("whole-run delivery was not declared")
	}
	return workflowDeliveryScene{run: run, workspace: workspace, node: node, first: first, second: second, work: work}
}

// Whole-run ownership and release commit with the single verification/receipt transaction.
func TestWorkflowDeliveryRegistersOneRealOwnerBeforeRelease(t *testing.T) {
	s := dispatcherDB(t)
	scene := seedWorkflowDelivery(t, s)
	input := scene.work.O("input")
	sessions := objectRows(input.O("workflow")["sessions"])
	if len(sessions) != 2 || sessions[0].S("executionId") != scene.first || sessions[1].S("executionId") != scene.second || input.S("sessionExecutionId") != "" || len(input.O("priorRevision")) != 0 {
		t.Fatalf("inventory=%v", input)
	}
	if n := countWorkflowDeleteOperations(t, s, scene.run); n != 0 {
		t.Fatalf("%d premature workspace deletes", n)
	}
	execution := "whole-" + uuid.NewString()
	result := Object{"outcome": "revision_delivered", "node": Object{"nodeId": scene.node}, "baseCommit": input.S("baseCommit"), "finalCommit": strings.Repeat("c", 40), "revisionRef": input.S("revisionRef"), "history": Object{"key": input.S("historyKey"), "size": int64(100), "sha256": strings.Repeat("a", 64)}}
	if _, err := s.transact(t.Context(), func(tx *transaction) Object {
		nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "nodeId": scene.node, "input": input, "epoch": int64(1)}})
		return Object{}
	}); err != nil {
		t.Fatal(err)
	}
	request := &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "result": result, "sequence": int64(1), "event": "whole-run-terminal"}, verification: &revisionVerification{input: jsonText(input), valid: true}}
	for range 2 {
		if _, err := s.transact(t.Context(), func(tx *transaction) Object { return nodeExecutionResult(tx, request, true) }); err != nil {
			t.Fatal(err)
		}
	}
	var revision Object
	if err := s.view(t.Context(), func(tx *transaction) {
		revision = tx.one("SELECT * FROM revisions WHERE workflow_run_id=$1", scene.run)
	}); err != nil {
		t.Fatal(err)
	}
	if revision == nil || revision["runId"] != nil || revision.S("workspaceId") != scene.workspace || revision.S("historyKind") != "workflow_manifest_v1" {
		t.Fatalf("wrong owner: %v", revision)
	}
	var state, release, status string
	var revisions, receipts int
	if err := s.Pool.QueryRow("SELECT delivery_state,workspace_release_state,status,(SELECT count(*) FROM revisions WHERE workflow_run_id=$1),(SELECT count(*) FROM node_event_receipts WHERE execution_id=$2) FROM workflow_runs WHERE id=$1", scene.run, execution).Scan(&state, &release, &status, &revisions, &receipts); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]any{state, release, status, revisions, receipts}, []any{"saved", "releasing", "succeeded", 1, 1}) {
		t.Fatalf("state=%s release=%s status=%s revisions=%d receipts=%d", state, release, status, revisions, receipts)
	}
	if n := countWorkflowDeleteOperations(t, s, scene.run); n != 1 {
		t.Fatalf("%d deletes after verified registration", n)
	}
	// The composite FK must reject a genuine run ID paired with the wrong Workspace.
	if _, err := s.Pool.Exec("UPDATE revisions SET workspace_id=(SELECT id FROM workspaces WHERE project_id=revisions.project_id AND kind='main') WHERE id=$1", revision.S("id")); err == nil {
		t.Fatal("wrong-workspace Revision passed PostgreSQL")
	}
	if _, err := s.Pool.Exec("UPDATE revisions SET run_id=$2 WHERE id=$1", revision.S("id"), uuid.NewString()); err == nil {
		t.Fatal("two-owner Revision passed PostgreSQL")
	}
}

// Failures keep execution outcome intact and either retry or retain an explicit unsaved residual.
func TestWorkflowDeliveryFailureRetriesWithoutReleasingThenGivesUp(t *testing.T) {
	s := dispatcherDB(t)
	scene := seedWorkflowDelivery(t, s)
	input := scene.work.O("input")
	execution := "failed-" + uuid.NewString()
	if _, err := s.transact(t.Context(), func(tx *transaction) Object {
		nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "nodeId": scene.node, "input": input, "epoch": int64(1)}})
		return nodeExecutionResult(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "result": Object{"outcome": "revision_failed", "reason": "upload_failed", "node": Object{"nodeId": scene.node}}, "sequence": int64(1), "event": "failed"}}, true)
	}); err != nil {
		t.Fatal(err)
	}
	var retry []byte
	if err := s.Pool.QueryRow("SELECT input FROM execution_work WHERE workflow_run_id=$1 AND kind='deliver_revision' AND execution_id IS NULL", scene.run).Scan(&retry); err != nil {
		t.Fatal(err)
	}
	var next Object
	if err := json.Unmarshal(retry, &next); err != nil {
		t.Fatal(err)
	}
	if next.S("checkoutExecutionId") != input.S("checkoutExecutionId") || next.S("deliveryRef") == input.S("deliveryRef") {
		t.Fatalf("bad retry: %v", next)
	}
	if n := countWorkflowDeleteOperations(t, s, scene.run); n != 0 {
		t.Fatal("failed save deleted workspace")
	}
	s.DeliveryGiveUpAfter = time.Minute
	if _, err := s.Pool.Exec("UPDATE workflow_runs SET delivery_started_at=now()-interval '2 hours' WHERE id=$1", scene.run); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceWorkflowDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state, release, status string
	if err := s.Pool.QueryRow("SELECT delivery_state,workspace_release_state,status FROM workflow_runs WHERE id=$1", scene.run).Scan(&state, &release, &status); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || release != "abandoned" || status != "succeeded" {
		t.Fatalf("%s/%s/%s", state, release, status)
	}
	if n := countWorkflowDeleteOperations(t, s, scene.run); n != 0 {
		t.Fatal("give-up claimed deletion")
	}
}

// A late result retains the original attempt's fact but cannot revive a given-up save or delete.
func TestWorkflowDeliveryLateSuccessIsReceiptOnlyAfterGiveUp(t *testing.T) {
	s := dispatcherDB(t)
	scene := seedWorkflowDelivery(t, s)
	input := scene.work.O("input")
	execution := "late-" + uuid.NewString()
	if _, err := s.transact(t.Context(), func(tx *transaction) Object {
		return nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "nodeId": scene.node, "input": input, "epoch": int64(1)}})
	}); err != nil {
		t.Fatal(err)
	}
	s.DeliveryGiveUpAfter = time.Minute
	if _, err := s.Pool.Exec("UPDATE workflow_runs SET delivery_started_at=now()-interval '2 hours' WHERE id=$1", scene.run); err != nil {
		t.Fatal(err)
	}
	if err := s.advanceWorkflowDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := Object{"outcome": "revision_unchanged", "node": Object{"nodeId": scene.node}, "baseCommit": input.S("baseCommit"), "finalCommit": input.S("baseCommit"), "revisionRef": input.S("revisionRef"), "history": Object{"key": input.S("historyKey"), "size": int64(100), "sha256": strings.Repeat("a", 64)}}
	if _, err := s.transact(t.Context(), func(tx *transaction) Object {
		return nodeExecutionResult(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "result": result, "sequence": int64(1), "event": "late-original-fact"}, verification: &revisionVerification{input: jsonText(input), valid: true}}, true)
	}); err != nil {
		t.Fatal(err)
	}
	var state, release string
	var revisions, receipts int
	if err := s.Pool.QueryRow("SELECT delivery_state,workspace_release_state,(SELECT count(*) FROM revisions WHERE workflow_run_id=$1),(SELECT count(*) FROM node_event_receipts WHERE execution_id=$2) FROM workflow_runs WHERE id=$1", scene.run, execution).Scan(&state, &release, &revisions, &receipts); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || release != "abandoned" || revisions != 0 || receipts != 1 {
		t.Fatalf("%s/%s revisions=%d receipts=%d", state, release, revisions, receipts)
	}
	if n := countWorkflowDeleteOperations(t, s, scene.run); n != 0 {
		t.Fatal("late fact triggered a delete")
	}
}
