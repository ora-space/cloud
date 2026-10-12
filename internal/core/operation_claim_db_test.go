package core

// The operation claim is the one place Cloud decides which Operation a Controller works on next, and
// the ordering inside it is the whole of Cloud's scheduling policy. These tests drive the real `claim`
// control action against a real schema; they assert which Operation came back and what Cloud wrote
// about it, never how long anything took.
//
// The ordering under test is a scheduling order and nothing else. An Operation changes state here only
// because a claim is what moves it to `running`, in the one UPDATE that always did; no deadline is
// inferred, no Operation is failed on its own behalf, and no state transition is added, removed or
// reordered. Where a test pins a limitation rather than a guarantee, it says so in its own name.

import (
	"context"
	"testing"
	"time"
)

// claimScene is the identity every Project in one claim scene belongs to.
type claimScene struct {
	tenant, user, space string
}

// seedClaimScene seeds the tenant a claim scene's Projects belong to. One transaction, because an
// active tenant requires an active administrator and the schema's integrity triggers are deferred.
func seedClaimScene(t *testing.T, store *Store) claimScene {
	t.Helper()
	s := claimScene{tenant: newID(), user: newID(), space: newID()}
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'claim','active')`, s.user)
		t.exec(`INSERT INTO tenants(id,name,status) VALUES($1,'claim','active')`, s.tenant)
		t.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, s.tenant, s.user)
		t.exec(`INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'claim','claim-scene',$3)`, s.space, s.tenant, s.user)
		return Object{}
	}); err != nil {
		t.Fatalf("seed claim scene: %v", err)
	}
	seedControllerLease(t, store)
	return s
}

// seedClaimOperation offers one Operation to the claim, on a Project of its own: the partial unique
// index `one_project_operation` allows a Project one live Operation, so two waiting Operations are
// necessarily two Projects — which is also the shape the starvation question is asked in.
//
// `age` is how long ago the Operation was created, and `retryAt` is when a `retry_wait` Operation
// becomes eligible again, relative to now. Both are computed in Go and bound as instants, so the test
// states them rather than depending on the database's clock.
func seedClaimOperation(t *testing.T, store *Store, s claimScene, state string, age time.Duration, retryAt *time.Duration) string {
	t.Helper()
	project, workspace, operation := newID(), newID(), newID()
	var retry any
	if retryAt != nil {
		retry = time.Now().Add(*retryAt)
	}
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle)
			VALUES($1,$2,$3,$4,'claim','https://git.example.invalid/claim.git','main','active')`, project, s.tenant, s.user, s.space)
		// A live Project must carry exactly one main Workspace, whatever else the scene does with it.
		// `requested_ref` has no default (0016 dropped it), so it is stated rather than left out.
		t.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready','main')`, workspace, s.tenant, s.user, project)
		t.exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash,retry_at,created_at)
			VALUES($1,$2,$3,$4,'start',$5,'clone','{}',$6,$6,$7,$8)`,
			operation, s.tenant, s.user, project, state, "claim-"+operation, retry, time.Now().Add(-age))
		return Object{}
	}); err != nil {
		t.Fatalf("seed claim operation: %v", err)
	}
	return operation
}

// operationState reads the two columns the scheduling order must not have changed on an Operation it
// did not choose.
func operationState(t *testing.T, store *Store, id string) (string, int64) {
	t.Helper()
	var state string
	var version int64
	if err := store.Pool.QueryRow(`SELECT state,version FROM operations WHERE id=$1`, id).Scan(&state, &version); err != nil {
		t.Fatalf("read operation: %v", err)
	}
	return state, version
}

// TestClaimPrefersWorkThatIsNotAlreadyRunning is the starvation fix itself.
//
// An Operation that is already running used to win the claim on age alone, so one Operation whose
// round kept failing held every later Operation behind it: the later work was queued, claimable and
// never claimed. The ordering must prefer a waiting Operation over a running one while leaving the
// running one a candidate, which is what the assertion after it checks.
func TestClaimPrefersWorkThatIsNotAlreadyRunning(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedClaimScene(t, store)
	running := seedClaimOperation(t, store, scene, "running", 2*time.Hour, nil)
	queued := seedClaimOperation(t, store, scene, "queued", time.Minute, nil)

	claimed, ok := claimOperation(t, store, 1)
	if !ok {
		t.Fatal("a queued Operation was waiting and the claim offered nothing")
	}
	if claimed.S("id") != queued {
		t.Fatalf("the queued Operation must be claimed before the older running one, got %s", claimed.S("id"))
	}
	if claimed.S("state") != "running" {
		t.Fatalf("the claim must still move the Operation to running, got %q", claimed.S("state"))
	}
	if operationVersion(t, store, running) != 1 {
		t.Fatal("the running Operation must not have been written by a claim that chose another one")
	}
}

// TestClaimStillRecoversARunningOperationWhenNothingElseWaits pins the half of the ordering that must
// not change: `state='running'` stays a candidate, so an Operation whose Controller died mid-round is
// still picked up by its successor. Recovery here is re-claiming, and nothing else — no timeout is
// inferred from the Operation's age, and its state is written only by the claim's existing UPDATE.
func TestClaimStillRecoversARunningOperationWhenNothingElseWaits(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedClaimScene(t, store)
	running := seedClaimOperation(t, store, scene, "running", 30*time.Hour, nil)

	claimed, ok := claimOperation(t, store, 1)
	if !ok {
		t.Fatal("an abandoned running Operation must still be claimable")
	}
	if claimed.S("id") != running {
		t.Fatalf("the only candidate must be claimed, got %s", claimed.S("id"))
	}
	if claimed.S("state") != "running" || claimed.N("controllerEpoch") != 1 || claimed.N("version") != 2 {
		t.Fatalf("recovery must be the ordinary claim: %v", claimed)
	}
	if claimed.S("errorCode") != "" {
		t.Fatalf("recovery must not have failed the Operation on its own behalf, got %q", claimed.S("errorCode"))
	}
}

// TestClaimHonoursTheRetryDelay pins the two sides of `retry_wait`: before its instant an Operation is
// not a candidate, and after it the Operation is one again. Both sides matter — an ordering that
// promoted a waiting retry over a running Operation but ignored the delay would retry a park the
// Controller deliberately asked for.
func TestClaimHonoursTheRetryDelay(t *testing.T) {
	t.Run("not yet due", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := seedClaimScene(t, store)
		waiting := 5 * time.Minute
		id := seedClaimOperation(t, store, scene, "retry_wait", time.Minute, &waiting)

		if claimed, ok := claimOperation(t, store, 1); ok {
			t.Fatalf("a retry that is not yet due must not be claimed, got %s", claimed.S("id"))
		}
		if operationVersion(t, store, id) != 1 {
			t.Fatal("the parked Operation must be untouched")
		}
	})

	t.Run("due", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := seedClaimScene(t, store)
		overdue := -time.Minute
		id := seedClaimOperation(t, store, scene, "retry_wait", 10*time.Minute, &overdue)

		claimed, ok := claimOperation(t, store, 1)
		if !ok {
			t.Fatal("a retry whose delay elapsed must be claimable again")
		}
		if claimed.S("id") != id {
			t.Fatalf("the due retry must be claimed, got %s", claimed.S("id"))
		}
		if claimed.S("state") != "running" {
			t.Fatalf("a due retry is claimed back into running, got %q", claimed.S("state"))
		}
	})

	t.Run("due retries are preferred over a running Operation", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := seedClaimScene(t, store)
		running := seedClaimOperation(t, store, scene, "running", 3*time.Hour, nil)
		overdue := -time.Minute
		id := seedClaimOperation(t, store, scene, "retry_wait", 3*time.Hour, &overdue)

		claimed, ok := claimOperation(t, store, 1)
		if !ok || claimed.S("id") != id {
			t.Fatalf("a due retry is waiting work and must be preferred over the running Operation, got %v (running=%s)", ok, running)
		}
	})
}

// TestClaimRejectsAStaleControllerEpoch pins the fencing the ordering sits inside and must not weaken.
// A claim carries the Controller's lease epoch, and an epoch that is not the live one is refused
// outright — the ordering cannot be reached at all, and nothing is written.
func TestClaimRejectsAStaleControllerEpoch(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedClaimScene(t, store)
	queued := seedClaimOperation(t, store, scene, "queued", time.Minute, nil)

	_, err := store.Control(context.Background(), &ControlRequest{
		Action: "claim", Body: Object{"epoch": 2}, Service: controllerClaims(),
	})
	if err == nil {
		t.Fatal("a claim under a superseded lease epoch must be refused")
	}
	if got := faultCode(t, err); got != "stale_controller" {
		t.Fatalf("the refusal must stay %q, got %q", "stale_controller", got)
	}
	if operationVersion(t, store, queued) != 1 {
		t.Fatal("a refused claim wrote to the Operation it refused to hand over")
	}
}

// TestClaimDoesNotLetOneRunningOperationHoldEveryProjectBack is the multi-Project form of the fix, and
// the shape the defect was reported in: one Project's Operation that never finishes must not stop other
// Projects' Operations from being claimed. Each of the waiting Operations is reached while the stuck
// one is still running and still undeleted.
func TestClaimDoesNotLetOneRunningOperationHoldEveryProjectBack(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedClaimScene(t, store)
	stuck := seedClaimOperation(t, store, scene, "running", 5*time.Hour, nil)
	first := seedClaimOperation(t, store, scene, "queued", 4*time.Hour, nil)
	second := seedClaimOperation(t, store, scene, "queued", 3*time.Hour, nil)

	reached := []string{}
	for range 2 {
		claimed, ok := claimOperation(t, store, 1)
		if !ok {
			t.Fatalf("work was still queued and the claim offered nothing after %v", reached)
		}
		reached = append(reached, claimed.S("id"))
	}
	if reached[0] != first || reached[1] != second {
		t.Fatalf("both queued Operations must be reached in age order while one stays running, got %v (first=%s second=%s)", reached, first, second)
	}
	// The stuck Operation is still running, still a candidate and still untouched by the two claims
	// that passed it: the fix reorders the queue, it does not conclude anything about the work.
	if state, version := operationState(t, store, stuck); state != "running" || version != 1 {
		t.Fatalf("the running Operation must be left exactly as it was, got %s v%d", state, version)
	}
	claimed, ok := claimOperation(t, store, 1)
	if !ok || claimed.S("id") != stuck {
		t.Fatalf("once nothing else waits the running Operation must be claimed again, got %v", claimed)
	}
}

// TestClaimDefersRunningRecoveryUnderContinuousArrivals pins a limitation rather than a guarantee, so
// that it is a recorded property of the scheduling order instead of a surprise.
//
// Preferring waiting work over running work means recovery of a running Operation is deferred for as
// long as new work keeps arriving: every arrival is waiting, so every arrival is claimed first. The
// deferral is bounded by the arrivals and not by the Operation — the last step shows recovery resuming
// the moment the queue drains — but under a genuinely continuous arrival rate it is unbounded, and a
// running Operation can be starved of the claim it needs to make progress. That is an unresolved
// scheduling risk, not a property this change establishes or fixes; it is the price of the starvation
// fix and it is cheaper than the alternative, which was starving everything else indefinitely.
func TestClaimDefersRunningRecoveryUnderContinuousArrivals(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedClaimScene(t, store)
	recovering := seedClaimOperation(t, store, scene, "running", 6*time.Hour, nil)
	before := operationVersion(t, store, recovering)

	for round := range 5 {
		arrival := seedClaimOperation(t, store, scene, "queued", time.Duration(5-round)*time.Minute, nil)
		claimed, ok := claimOperation(t, store, 1)
		if !ok {
			t.Fatalf("round %d: the arrival must be claimed", round)
		}
		if claimed.S("id") != arrival {
			t.Fatalf("round %d: an arrival must be preferred over the running Operation, got %s", round, claimed.S("id"))
		}
	}
	if operationVersion(t, store, recovering) != before {
		t.Fatal("the running Operation must not have been claimed while arrivals kept coming")
	}

	// Nothing arrives, and the deferred recovery happens at once. The deferral is therefore a
	// consequence of the arrival rate and not a state the Operation is left in.
	claimed, ok := claimOperation(t, store, 1)
	if !ok || claimed.S("id") != recovering {
		t.Fatalf("recovery must resume once nothing is waiting, got %v", claimed)
	}
}
