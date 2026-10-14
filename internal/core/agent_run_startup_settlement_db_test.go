package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A declared first prompt is not evidence that a Node accepted the session. Ending before
// dispatch must retire that intent and reach cleanup instead of waiting for an unreachable command.
func TestUndispatchedThreadEndReachesSettlement(t *testing.T) {
	store := commandStore(t)
	scene, _ := seedDispatchableWork(t, store)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		return endThreadByUser(tx, store, &PublicRequest{
			TenantID: scene.seed.seed.tenant,
			IssueID:  scene.seed.seed.issue,
			RunID:    scene.run,
		})
	}); err != nil {
		t.Fatalf("request ending before dispatch: %v", err)
	}
	if err := store.StartQueuedAgentSessionsOnce(context.Background()); err != nil {
		t.Fatalf("reconcile starting run: %v", err)
	}
	phase, status, _, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "releasing" {
		t.Fatalf("undispatched end must reach cleanup, got phase=%q status=%q thread=%v",
			phase.String, status, runThreadState(t, store, scene.run))
	}
	if got := countDeleteOperations(t, store, scene.run); got != 1 {
		t.Fatalf("cleanup requires one owned delete operation, got %d", got)
	}
}

func requestStartupEnd(t *testing.T, store *Store, scene dispatchableScene) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		return endThreadByUser(tx, store, &PublicRequest{TenantID: scene.seed.seed.tenant, IssueID: scene.seed.seed.issue, RunID: scene.run})
	}); err != nil {
		t.Fatalf("request startup end: %v", err)
	}
}

func startupPass(t *testing.T, store *Store) {
	t.Helper()
	if err := store.StartQueuedAgentSessionsOnce(context.Background()); err != nil {
		t.Fatalf("startup reconciliation: %v", err)
	}
}

// The old work identity cannot be registered once withdrawal and cleanup have committed.
func TestUndispatchedStartupWithdrawalRejectsLateDispatchAndRepeats(t *testing.T) {
	store := commandStore(t)
	scene, claims := seedDispatchableWork(t, store)
	seedQueuedUserTurn(t, store, scene.run, "queued before ending")
	requestStartupEnd(t, store, scene)
	startupPass(t, store)
	startupPass(t, store)
	if _, err := dispatchAgentWork(t, store, claims, scene.work, execOf(scene.work, "late"), "", nil); err == nil {
		t.Fatal("withdrawn session intent accepted a late dispatch")
	}
	if got := countDeleteOperations(t, store, scene.run); got != 1 {
		t.Fatalf("repeated reconciliation created %d deletes", got)
	}
	if got := countDiscardedTurns(t, store, scene.run); got != 1 {
		t.Fatalf("unexecuted user turn was not discarded: %d", got)
	}
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 0 {
		t.Fatalf("undeliverable commands survived withdrawal: %v", reasons)
	}
}

// Missing capability is definitive only before registration, and records a safe failure.
func TestModelProxyRefusalSettlesBeforeDispatch(t *testing.T) {
	store := commandStore(t)
	input := sessionStartInput()
	input["agentPluginId"] = "official/ora-space.opencode"
	input["modelBindingId"] = newID()
	scene := seedStartingRun(t, store, input)
	bindConnectedNode(t, store, scene.ws) // This incarnation explicitly registered modelProxy=false.
	startupPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if phase.String != "releasing" || status != "failed" || reason != "model_proxy_unavailable" {
		t.Fatalf("capability refusal did not settle: %s/%s/%s", phase.String, status, reason)
	}
	if _, _, _, _, present := firstTurn(t, store, scene.seed.run); present {
		t.Fatal("capability refusal declared a prompt despite having no executable target")
	}
}

// An unknown Node cannot supply a deterministic capability refusal. The connected flag can outlive
// its heartbeat, so the business loop must use the same freshness boundary as dispatch admission.
func TestUnknownNodeCapabilityDoesNotRejectStartup(t *testing.T) {
	for _, test := range []struct {
		name        string
		connection  string
		initialized bool
		stale       bool
	}{
		{name: "expired heartbeat", connection: "connected", initialized: true, stale: true},
		{name: "disconnected", connection: "disconnected", initialized: true},
		{name: "not initialized", connection: "connected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := commandStore(t)
			input := sessionStartInput()
			input["modelBindingId"] = newID()
			scene := seedStartingRun(t, store, input)
			bindConnectedNode(t, store, scene.ws)
			if _, err := store.Pool.Exec(`UPDATE node_instances SET connection_state=$2,initialized=$3,
				last_seen_at=CASE WHEN $4 THEN clock_timestamp()-interval '1 minute' ELSE clock_timestamp() END
				WHERE workspace_id=$1`, scene.ws, test.connection, test.initialized, test.stale); err != nil {
				t.Fatal(err)
			}
			startupPass(t, store)
			startupPass(t, store)
			phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
			if phase.String != "starting" || status != "dispatched" || reason != "" {
				t.Fatalf("unknown Node was classified as a capability refusal: %s/%s/%s", phase.String, status, reason)
			}
			if got := countDeleteOperations(t, store, scene.seed.run); got != 0 {
				t.Fatalf("unknown capability caused %d cleanup declarations", got)
			}
		})
	}
}

// Inputs queued before this reconciliation policy retain their frozen intent when the target's
// capability evidence expires. A stale Node cannot turn an existing pending Thread into failure.
func TestStaleNodeCannotWithdrawDeclaredStartup(t *testing.T) {
	store := commandStore(t)
	input := sessionStartInput()
	input["modelBindingId"] = newID()
	scene := seedStartingRun(t, store, input)
	bindConnectedNode(t, store, scene.ws)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if err := store.startAgentSession(tx, scene.seed.run); err != nil {
			panic(databaseFailure{err})
		}
		return Object{}
	}); err != nil {
		t.Fatalf("declare the existing session intent: %v", err)
	}
	if _, err := store.Pool.Exec(`UPDATE node_instances SET last_seen_at=clock_timestamp()-interval '1 minute'
		WHERE workspace_id=$1`, scene.ws); err != nil {
		t.Fatal(err)
	}
	startupPass(t, store)
	startupPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if phase.String != "starting" || status != "dispatched" || reason != "" || runThreadState(t, store, scene.seed.run).String != "pending" {
		t.Fatalf("stale Node withdrew pending startup: %s/%s/%s thread=%v", phase.String, status, reason, runThreadState(t, store, scene.seed.run))
	}
	var remaining int64
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE run_id=$1
		AND kind='agent_session' AND execution_id IS NULL AND input->>'modelBindingId'=$2`,
		scene.seed.run, input.S("modelBindingId")).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 || countDeleteOperations(t, store, scene.seed.run) != 0 {
		t.Fatalf("stale capability lost frozen work or queued cleanup: remaining=%d", remaining)
	}
	ids, err := store.scanStartingAgentRuns(context.Background(), agentSessionStartBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == scene.seed.run {
			t.Fatal("stale capability repeatedly occupies the bounded startup scan")
		}
	}
}

// A queued start can expire without ever becoming an external execution.
func TestUndispatchedStartupTimeoutReleasesItsWorkspace(t *testing.T) {
	store := commandStore(t)
	store.AgentStartTimeout = time.Minute
	scene, _ := seedDispatchableWork(t, store)
	expireStartupWindow(t, store, scene.run)
	startupPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "releasing" || status != "failed" || reason != "agent_start_failed" {
		t.Fatalf("expired undispatched start was not released: %s/%s/%s", phase.String, status, reason)
	}
}

// Registration means the Node may have acted. Timeout requests ending and retains all evidence.
func TestRegisteredStartupTimeoutRetainsReconciliationResponsibility(t *testing.T) {
	store := commandStore(t)
	store.AgentStartTimeout = time.Minute
	scene := seedTakeoverScene(t, store)
	expireStartupWindow(t, store, scene.run)
	startupPass(t, store)
	startupPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "starting" || status != "dispatched" || reason != "agent_start_failed" || runThreadState(t, store, scene.run).String != "ending" {
		t.Fatalf("unknown execution lacks its recoverable ending state: %s/%s/%s", phase.String, status, reason)
	}
	if countDeleteOperations(t, store, scene.run) != 0 {
		t.Fatal("unknown execution was treated as proof of absence and deleted")
	}
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("timeout did not retain exactly one termination request: %v", reasons)
	}
	var resultIsNull bool
	if err := store.Pool.QueryRow("SELECT result IS NULL FROM node_executions WHERE execution_id=$1", scene.execution).Scan(&resultIsNull); err != nil {
		t.Fatal(err)
	}
	if !resultIsNull {
		t.Fatal("startup timeout fabricated a Node result")
	}
}

// The startup pass may run before the cancellation pass; an accepted cancellation still wins.
func TestStartupTimeoutHonorsCancellationBeforeEndingPass(t *testing.T) {
	store := commandStore(t)
	store.AgentStartTimeout = time.Minute
	scene := seedTakeoverScene(t, store)
	requestCancel(t, store, scene.run)
	expireStartupWindow(t, store, scene.run)
	startupPass(t, store)
	var policyCancellation bool
	if err := store.Pool.QueryRow(`SELECT (result->'startup'->>'policyCancellation')::boolean FROM issue_runs WHERE id=$1`, scene.run).Scan(&policyCancellation); err != nil {
		t.Fatal(err)
	}
	if policyCancellation || runThreadState(t, store, scene.run).String != "ending" {
		t.Fatal("startup timeout overwrote an accepted user cancellation before its ending pass")
	}
	cancelPass(t, store)
	startupPass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("reordered cancellation/startup passes created duplicate ending: %v", reasons)
	}
}

// Dispatch and withdrawal serialize on the same lock: either registration retains responsibility,
// or withdrawal rejects the late registration. Neither outcome permits cleanup of a registered start.
func TestStartupEndingAndDispatchRaceKeepsOneOwner(t *testing.T) {
	store := commandStore(t)
	scene, claims := seedDispatchableWork(t, store)
	requestStartupEnd(t, store, scene)
	var registrationErr, reconcileErr error
	var group sync.WaitGroup
	start := make(chan struct{})
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		_, registrationErr = dispatchAgentWork(t, store, claims, scene.work, execOf(scene.work, "race"), "", nil)
	}()
	go func() {
		defer group.Done()
		<-start
		reconcileErr = store.StartQueuedAgentSessionsOnce(context.Background())
	}()
	close(start)
	group.Wait()
	if reconcileErr != nil {
		t.Fatal(reconcileErr)
	}
	phase, _, _, _, _, _ := runFields(t, store, scene.run)
	if registrationErr == nil {
		if phase.String != "starting" || countDeleteOperations(t, store, scene.run) != 0 {
			t.Fatal("successful registration lost its reconciliation responsibility")
		}
	} else if phase.String != "releasing" || countDeleteOperations(t, store, scene.run) != 1 {
		t.Fatalf("withdrawal did not own cleanup: phase=%s registration=%v", phase.String, registrationErr)
	}
}

// Losing cleanup declaration must restore the intent, commands and business state together.
func TestUndispatchedStartupSettlementRollsBackWhenCleanupFails(t *testing.T) {
	store := commandStore(t)
	scene, _ := seedDispatchableWork(t, store)
	seedQueuedUserTurn(t, store, scene.run, "queued before ending")
	requestStartupEnd(t, store, scene)
	before := runVersion(t, store, scene.run)
	if _, err := store.Pool.Exec(`CREATE FUNCTION reject_startup_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.kind='delete_workspace' THEN RAISE EXCEPTION 'injected cleanup failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_startup_cleanup BEFORE INSERT ON operations FOR EACH ROW EXECUTE FUNCTION reject_startup_cleanup()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		store.reconcileAgentStartup(tx, scene.run)
		return Object{}
	}); err == nil {
		t.Fatal("failed cleanup declaration committed the startup withdrawal")
	}
	phase, status, reason, _, _, _ := runFields(t, store, scene.run)
	var workCount int
	if err := store.Pool.QueryRow("SELECT count(*) FROM execution_work WHERE run_id=$1 AND kind='agent_session'", scene.run).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if phase.String != "starting" || status != "dispatched" || reason != "" || runThreadState(t, store, scene.run).String != "ending" || runVersion(t, store, scene.run) != before ||
		workCount != 1 || len(endSessionRequests(t, store, scene.run)) != 1 || countDiscardedTurns(t, store, scene.run) != 0 || countDeleteOperations(t, store, scene.run) != 0 ||
		countRunActivities(t, store, scene.seed.seed.issue, "run.cancelled") != 0 {
		t.Fatal("cleanup failure did not roll back work, commands, turns, activity and run state together")
	}
	if _, err := store.Pool.Exec("DROP TRIGGER reject_startup_cleanup ON operations; DROP FUNCTION reject_startup_cleanup()"); err != nil {
		t.Fatal(err)
	}
	startupPass(t, store)
	if countDeleteOperations(t, store, scene.run) != 1 || countDiscardedTurns(t, store, scene.run) != 1 {
		t.Fatal("retry after rollback did not settle once")
	}
}

// An occupied Project slot retains the releasing run for the existing delete redeclaration loop.
func TestUndispatchedStartupSettlementRetriesCleanupAfterProjectBusy(t *testing.T) {
	store := commandStore(t)
	scene, _ := seedDispatchableWork(t, store)
	requestStartupEnd(t, store, scene)
	busyID := newID()
	if _, err := store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash)
		VALUES($1,$2,(SELECT owner_user_id FROM projects WHERE id=$3),$3,'start','running','ready','{}','startup-busy','startup-busy-hash')`, busyID, scene.seed.seed.tenant, scene.seed.seed.project); err != nil {
		t.Fatal(err)
	}
	startupPass(t, store)
	phase, status, _, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "releasing" || status != "cancelled" || countDeleteOperations(t, store, scene.run) != 0 {
		t.Fatalf("busy cleanup lost its releasing responsibility: %s/%s", phase.String, status)
	}
	if _, err := store.Pool.Exec("UPDATE operations SET state='succeeded',step='done' WHERE id=$1", busyID); err != nil {
		t.Fatal(err)
	}
	if err := store.RedeclareRunWorkspaceDeletesOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.RedeclareRunWorkspaceDeletesOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countDeleteOperations(t, store, scene.run) != 1 {
		t.Fatal("cleanup redeclaration did not create exactly one owned delete")
	}
}
