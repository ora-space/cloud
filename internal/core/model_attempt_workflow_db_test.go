package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
)

// A WorkflowRun binding must be able to register its outbound attempt. The ledger's foreign keys
// originally required an IssueRun (0035), while 0040 made a WorkflowRun the owner of its bindings,
// so every WorkflowRun model request failed registration and the gateway answered 503 without
// contacting the provider. This drives the real grant and binding chain and the real PostgreSQL
// constraints; success here is what lets the gateway forward the request.
func TestWorkflowModelAttemptRegistersUnderWorkflowRun(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	// The runtime confirms its binding once the Node has the dispatched session.
	if _, err := store.Pool.Exec(`UPDATE runtime_controls SET binding_confirmed=true,
		bound_sandbox_id=(SELECT sandbox_instance_id FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL)
		WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	var bindingID, tenant string
	var generation int64
	if err := store.Pool.QueryRow(`SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name='default'`, runID).Scan(&bindingID); err != nil {
		t.Fatalf("read frozen binding: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT tenant_id, runtime_generation FROM workspaces WHERE id=$1`, wsID).Scan(&tenant, &generation); err != nil {
		t.Fatal(err)
	}
	scope := ModelRuntimeScope{TenantID: tenant, WorkspaceID: wsID, Generation: generation}
	digest := sha256.Sum256([]byte("synthetic workflow attempt"))
	grant, err := store.CreateModelGrant(context.Background(), scope, bindingID, execution, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	// The grant names the binding's owner, which for a WorkflowRun is its run id.
	if grant.RunID != runID || grant.BindingID != bindingID || grant.ExecutionID != execution {
		t.Fatalf("grant = %+v; want run %s binding %s execution %s", grant, runID, bindingID, execution)
	}
	attemptID := uuid.NewString()
	if err := store.BeginModelAttempt(context.Background(), &grant, attemptID, true); err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	var issueRun, workflowRun sql.NullString
	var storedBinding, storedExecution string
	var streamed bool
	if err := store.Pool.QueryRow(`SELECT run_id::text, workflow_run_id::text, binding_id, execution_id, streamed
		FROM model_request_attempts WHERE id=$1`, attemptID).Scan(&issueRun, &workflowRun, &storedBinding, &storedExecution, &streamed); err != nil {
		t.Fatalf("read attempt ledger: %v", err)
	}
	if issueRun.Valid || !workflowRun.Valid || workflowRun.String != runID {
		t.Fatalf("attempt owner = run_id %v workflow_run_id %v; want workflow_run_id %s", issueRun, workflowRun, runID)
	}
	if storedBinding != bindingID || storedExecution != execution || !streamed {
		t.Fatalf("attempt = binding %s execution %s streamed %v; want %s %s true", storedBinding, storedExecution, streamed, bindingID, execution)
	}
	// Finalization addresses the attempt by id and is independent of the owner discriminator.
	firstOutput := int64(1)
	if err := store.FinishModelAttempt(context.Background(), attemptID, &ModelAttemptFinish{
		Outcome: "succeeded", DurationMS: 2, FirstOutputMS: &firstOutput, TerminalObserved: true,
		Usage: ModelAttemptUsage{InputSemantics: "unknown", Availability: "unavailable"},
	}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	var outcome string
	var finished bool
	if err := store.Pool.QueryRow(`SELECT outcome, finished_at IS NOT NULL FROM model_request_attempts WHERE id=$1`, attemptID).Scan(&outcome, &finished); err != nil {
		t.Fatal(err)
	}
	if outcome != "succeeded" || !finished {
		t.Fatalf("attempt settled outcome=%q finished=%v; want succeeded/true", outcome, finished)
	}
}
