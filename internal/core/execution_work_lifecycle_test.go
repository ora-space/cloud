package core

import (
	"context"
	"testing"
)

// A stale work row must neither block another tenant nor authorize a new execution. Terminal
// lifecycle state is the fixture precondition; declaration, claim and dispatch use production code.
func TestRetiredRunWorkCannotBlockOrRegister(t *testing.T) {
	for _, phase := range []string{"releasing", "done"} {
		t.Run(phase, func(t *testing.T) {
			store := dispatcherDB(t)
			old, claims := seedDispatchableWork(t, store)
			healthy := seedStartingRun(t, store, sessionStartInput())
			bindConnectedNode(t, store, healthy.ws)
			runSessionStartPass(t, store)
			if old.seed.seed.tenant == healthy.seed.tenant {
				t.Fatal("the successor must belong to another tenant")
			}
			if _, err := store.Pool.Exec(`UPDATE issue_runs SET phase=$2,status='failed',
				thread_state='ended',completed_at=now(),version=version+1 WHERE id=$1`, old.run, phase); err != nil {
				t.Fatalf("stage the terminal business state: %v", err)
			}
			var healthyWork Object
			for attempt := range 3 {
				picked, err := store.Control(context.Background(), &ControlRequest{
					Action: "clone_claim", Body: Object{"epoch": 1}, Service: claims,
				})
				if err != nil {
					t.Fatalf("claim %d: %v", attempt+1, err)
				}
				healthyWork = picked.O("work")
				if healthyWork.S("runId") != healthy.seed.run {
					t.Errorf("claim %d: terminal Run work still occupies the head; another tenant's live work was not offered", attempt+1)
				}
			}
			if _, err := dispatchAgentWork(t, store, claims, old.work, execOf(old.work, "late"), "", nil); err == nil || ErrorCode(err).Code != "dispatch_conflict" {
				t.Errorf("a pre-settlement claim must not register new work after %s: %v", phase, err)
			}
			if healthyWork.S("runId") == healthy.seed.run {
				if _, err := dispatchAgentWork(t, store, claims, healthyWork, execOf(healthyWork, "healthy"), "", nil); err != nil {
					t.Fatalf("the other tenant must still register its valid work: %v", err)
				}
			}
		})
	}
}
