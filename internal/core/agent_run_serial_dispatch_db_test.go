package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// A second real Agent can share an Issue and Project while keeping a distinct target identity.
func seedSecondTaskAgent(t *testing.T, store *Store, first dispSeed) string {
	t.Helper()
	agent, run := uuid.NewString(), uuid.NewString()
	if _, err := store.Pool.Exec(`
		INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status)
		SELECT $1,space_id,tenant_id,'official/second-agent','Second','active'
		FROM space_agents WHERE id=$2`, agent, first.agent); err != nil {
		t.Fatalf("seed second agent: %v", err)
	}
	if _, err := store.Pool.Exec(`
		INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,created_at)
		VALUES($1,$2,$3,'agent',$4,'queued',clock_timestamp()+interval '1 second')`,
		run, first.tenant, first.issue, agent); err != nil {
		t.Fatalf("seed second run: %v", err)
	}
	if _, err := store.Pool.Exec(`
		INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
		SELECT $1,tenant_id,issue_id,2,'user',actor_id,'run.enqueued',jsonb_build_object('runId',$2::text)
		FROM issue_activities WHERE issue_id=$3 AND action='run.enqueued'`,
		uuid.NewString(), run, first.issue); err != nil {
		t.Fatalf("seed second run actor: %v", err)
	}
	return run
}

func TestTaskAgentClaimWaitsForOlderRunAndConfirmedRelease(t *testing.T) {
	store := dispatcherDB(t)
	first := seedDispatchScene(t, store.Pool, false)
	second := seedSecondTaskAgent(t, store, first)
	ctx := context.Background()

	// Direct dispatch can arrive in any order; it must not bypass the older queue item.
	if err := store.agentRunDispatcher().Dispatch(ctx, second); err != nil {
		t.Fatal(err)
	}
	if status, phase := runClaim(t, store, first.tenant, second); status != "queued" || phase.Valid {
		t.Fatalf("newer Agent ran before the older one: %s/%v", status, phase)
	}
	if err := store.agentRunDispatcher().Dispatch(ctx, first.run); err != nil {
		t.Fatal(err)
	}
	if status, phase := runClaim(t, store, first.tenant, first.run); status != "dispatched" || phase.String != "provisioning" {
		t.Fatalf("older Agent failed to claim: %s/%v", status, phase)
	}
	if ids, err := store.scanQueuedAgentRuns(ctx, 100); err != nil || len(ids) != 0 {
		t.Fatalf("scanner offered a run while the first still owns the task: %v, %v", ids, err)
	}

	// A terminal business status alone is not proof that the workspace was released.
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='completed',phase='done' WHERE id=$1`, first.run); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.scanQueuedAgentRuns(ctx, 100); err != nil || len(ids) != 0 {
		t.Fatalf("scanner treated a residual workspace as released: %v, %v", ids, err)
	}
	if _, err := store.Pool.Exec(`UPDATE workspaces SET deleted_at=clock_timestamp() WHERE issue_run_id=$1`, first.run); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.scanQueuedAgentRuns(ctx, 100); err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatalf("scanner did not offer the next Agent after release: %v, %v", ids, err)
	}
}

func TestTaskAgentClaimPausesAfterFailure(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			store := dispatcherDB(t)
			first := seedDispatchScene(t, store.Pool, false)
			second := seedSecondTaskAgent(t, store, first)
			if _, err := store.Pool.Exec(`UPDATE issue_runs SET status=$2,phase='done' WHERE id=$1`, first.run, outcome); err != nil {
				t.Fatal(err)
			}
			if ids, err := store.scanQueuedAgentRuns(context.Background(), 100); err != nil || len(ids) != 0 {
				t.Fatalf("scanner advanced after %s without explicit recovery: %v, %v", outcome, ids, err)
			}
			if err := store.agentRunDispatcher().Dispatch(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			if status, phase := runClaim(t, store, first.tenant, second); status != "queued" || phase.Valid {
				t.Fatalf("direct dispatch advanced after %s: %s/%v", outcome, status, phase)
			}
		})
	}
}
