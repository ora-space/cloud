package core

// The WorkflowRun serial-executor evidence for specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md D1 条件②: the per-run exclusive transaction
// right is cross-process and released with the transaction, the Issue lock keeps one Issue's
// Timeline seq writes serial, and the Project lock keeps the one invariant that spans runs. Every
// scene is a real PostgreSQL database; "another replica" is a second Store with its own connection
// pool over the same database, so nothing but PostgreSQL is shared.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
)

// secondReplica returns a Store with its own connection pool over the same database. Cloud keeps no
// process-local scheduler state, so this is the control-plane-level stand-in for a second process:
// any exclusion observed between it and the first Store comes from the database alone.
func secondReplica(t *testing.T, store *Store) *Store {
	t.Helper()
	var name string
	if err := store.Pool.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("read current database: %v", err)
	}
	if coreFixtureState.template == nil {
		t.Fatal("core fixture template is not initialized")
	}
	config := coreFixtureState.template.config.Copy()
	config.Database = name
	replica := &Store{Pool: stdlib.OpenDB(*config)}
	BindBusinessHooks(replica)
	t.Cleanup(func() { _ = replica.Pool.Close() })
	return replica
}

// workspaceCount is the number of Workspace rows bound to a run, the witness for "the pass did not
// provision" and for "the run owns exactly one Workspace".
func workspaceCount(t *testing.T, store *Store, runID string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM workspaces WHERE workflow_run_id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("count run workspaces: %v", err)
	}
	return n
}

// holdRunRight makes a replica the run's serial executor and keeps the transaction open until the
// returned release is called. It reports when the right is held, so the test never races the setup.
func holdRunRight(t *testing.T, replica *Store, runID string) (release func(), done <-chan error) {
	t.Helper()
	return holdRunRightDuring(t, replica, runID, nil)
}

// holdRunRightDuring is holdRunRight with a step that runs while the right is held. It returns after
// that step and before the test releases the transaction, so the step's uncommitted writes and any
// lock it took are still open when the caller acts.
func holdRunRightDuring(t *testing.T, replica *Store, runID string, setup func(*transaction)) (release func(), done <-chan error) {
	t.Helper()
	held, unlock := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, ran, err := replica.transactWorkflowRun(context.Background(), runID, func(tx *transaction) Object {
			if setup != nil {
				setup(tx)
			}
			close(held)
			<-unlock
			return Object{}
		})
		if err == nil && !ran {
			err = fmt.Errorf("replica did not acquire the run right it was asked to hold")
		}
		finished <- err
	}()
	<-held
	return func() { close(unlock) }, finished
}

// waitForAdvisoryWait blocks until a transaction in this database is waiting for the two-integer
// advisory lock (class, key). The test observes the wait instead of inferring it from elapsed time.
func waitForAdvisoryWait(t *testing.T, store *Store, class, key int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := store.Pool.QueryRow(`
			SELECT count(*) FROM pg_locks
			WHERE locktype='advisory' AND objsubid=2 AND NOT granted
			  AND classid::bigint=$1 AND objid::bigint=$2
			  AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`,
			int64(uint32(class)), int64(uint32(key))).Scan(&waiting); err != nil {
			t.Fatalf("read advisory lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no transaction waited for the advisory lock within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedWorkflowRunInProject adds a second pending run to the same Project as sourceRunID, freezing
// the same per-slot bindings the creation transaction would, so it is one advance away from
// provisioning too.
func seedWorkflowRunInProject(t *testing.T, store *Store, sourceRunID, uid string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_by,project_id,plugin_version)
			SELECT $1,tenant_id,workflow_id,snapshot_id,'disp-2','pending',input,created_by,project_id,plugin_version
			FROM workflow_runs WHERE id=$2`, id, sourceRunID)
		freezeWorkflowRunExecutors(tx, id, uid, workflowSessionGraph())
		return Object{}
	}); err != nil {
		t.Fatalf("seed second run in the Project: %v", err)
	}
	return id
}

// seedLinkedRunsOnOneIssue adds a second pending run in the same tenant and workflow as
// sourceRunID, creates one Issue, and links both runs to it through issue_runs. Both runs carry the
// same linear start -> output graph, which one pass settles, so the scheduler projects each run's
// Timeline entries onto the shared Issue.
//
// The source run is staged already claimed (running, with its IssueRun started), the state a replica
// leaves behind between ticks. That is also what lets both of them be linked at once: the pending
// dedup index allows only one queued IssueRun per (issue, executor), while a running run and a
// queued sibling are a normal pair. The pass below therefore projects run.completed for the source
// run and run.started + run.completed for the second, all into one Issue Timeline.
func seedLinkedRunsOnOneIssue(t *testing.T, store *Store, sourceRunID string) (issueID, secondRunID string) {
	t.Helper()
	issueID, secondRunID = uuid.NewString(), uuid.NewString()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input)
			SELECT $1,tenant_id,workflow_id,snapshot_id,'disp-2','pending',input
			FROM workflow_runs WHERE id=$2`, secondRunID, sourceRunID)
		user := tx.one(`SELECT user_id FROM tenant_memberships
			WHERE tenant_id=(SELECT tenant_id FROM workflow_runs WHERE id=$1) LIMIT 1`, sourceRunID)
		tx.exec(`INSERT INTO issues(id,tenant_id,creator_user_id,title,number)
			SELECT $1,w.tenant_id,$2,'linked runs',
			       COALESCE((SELECT max(i.number) FROM issues i WHERE i.tenant_id=w.tenant_id),0)+1
			FROM workflow_runs w WHERE w.id=$3`, issueID, user.S("userId"), sourceRunID)
		tx.exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,workflow_invocation_ref)
			SELECT gen_random_uuid(),w.tenant_id,$1,'workflow',w.workflow_id,'queued',w.id
			FROM workflow_runs w WHERE w.id=$2`, issueID, sourceRunID)
		tx.exec(`UPDATE workflow_runs SET status='running', started_at=now(), version=version+1 WHERE id=$1`, sourceRunID)
		tx.exec(`UPDATE issue_runs SET status='running', started_at=now(), version=version+1
			WHERE workflow_invocation_ref=$1 AND deleted_at IS NULL`, sourceRunID)
		// The sibling is queued only after the source run's IssueRun left the queued/dispatched
		// states the pending dedup index covers.
		tx.exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,workflow_invocation_ref)
			SELECT gen_random_uuid(),w.tenant_id,$1,'workflow',w.workflow_id,'queued',w.id
			FROM workflow_runs w WHERE w.id=$2`, issueID, secondRunID)
		return Object{}
	}); err != nil {
		t.Fatalf("seed two runs linked to one Issue: %v", err)
	}
	return issueID, secondRunID
}

// assertLinkedRunsSettled checks the shared Issue Timeline at the storage boundary after both linked
// runs reached their terminal state: every activity has its own position, dense from 1, and both
// linked IssueRuns completed.
func assertLinkedRunsSettled(t *testing.T, store *Store, issueID string, runIDs ...string) {
	t.Helper()
	for _, runID := range runIDs {
		status, runErr, states, _ := readWorkflowRun(t, store, runID)
		if status != "succeeded" || runErr != "" {
			t.Fatalf("run %s: status=%q err=%q states=%v; want succeeded", runID, status, runErr, states)
		}
	}
	rows, err := store.Pool.Query(`SELECT seq FROM issue_activities WHERE issue_id=$1 ORDER BY seq`, issueID)
	if err != nil {
		t.Fatalf("read Issue Timeline: %v", err)
	}
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("read Issue Timeline seq: %v", err)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read Issue Timeline: %v", err)
	}
	// The source run completes and the second run starts and completes, all onto this one Issue.
	if len(seqs) != 3 {
		t.Fatalf("Issue Timeline has seq %v; want 3 activities (completed for the running run, started+completed for the queued one)", seqs)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("Issue Timeline seq = %v; want a dense 1..3 range with no duplicate or gap", seqs)
		}
	}
	var completed int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE issue_id=$1 AND status='completed'`, issueID).Scan(&completed); err != nil {
		t.Fatalf("count completed IssueRuns: %v", err)
	}
	if completed != 2 {
		t.Fatalf("%d linked IssueRuns completed; want 2", completed)
	}
}

// A replica cannot advance a run while another replica holds that run's exclusive transaction right:
// the pass declines the run, writes nothing, and advances it on the next tick once the right is free.
func TestWorkflowAdvanceSkipsARunHeldByAnotherReplica(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	_, _, _, versionBefore := readWorkflowRun(t, store, runID)

	release, done := holdRunRight(t, replica, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance while another replica holds the run: %v", err)
	}
	status, _, states, version := readWorkflowRun(t, store, runID)
	if status != "pending" || len(states) != 0 || version != versionBefore {
		t.Fatalf("held run changed: status=%q states=%v version=%d (want pending, no states, version %d)",
			status, states, version, versionBefore)
	}
	if n := workspaceCount(t, store, runID); n != 0 {
		t.Fatalf("held run provisioned %d Workspace(s); the losing replica must write nothing", n)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("release the run right: %v", err)
	}

	// The same pass, one tick later, is now the executor: it claims the run and provisions its
	// Workspace. This is the "idempotent exit, retried next tick" half of the obligation.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the right was released: %v", err)
	}
	status, _, states, _ = readWorkflowRun(t, store, runID)
	if status != "running" {
		t.Fatalf("run after release: status=%q states=%v; want running", status, states)
	}
	if n := workspaceCount(t, store, runID); n != 1 {
		t.Fatalf("run after release owns %d Workspace(s); want exactly 1", n)
	}
}

// The right is per run, not global: while one replica holds one run, another replica still advances
// a different run in the same pass. A global exclusive transaction lock cannot produce this result —
// the pass would block until the held transaction ended.
func TestWorkflowAdvanceAdvancesAnotherRunWhileOneRunIsHeld(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	heldRun, _, _, _, _ := seedWorkflowSessionScene(t, store)
	// A linear start -> output run: one pass executes and settles it, so the assertion does not
	// depend on any other seeding or on a second user's model configuration.
	otherRun := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "done")},
		[][3]string{{"s", "o", ""}},
	))

	release, done := holdRunRight(t, replica, heldRun)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance while another run is held: %v", err)
	}
	if status, _, states, _ := readWorkflowRun(t, store, heldRun); status != "pending" || len(states) != 0 {
		t.Fatalf("held run changed: status=%q states=%v", status, states)
	}
	if status, runErr, states, _ := readWorkflowRun(t, store, otherRun); status != "succeeded" || runErr != "" {
		t.Fatalf("unheld run: status=%q err=%q states=%v; want succeeded", status, runErr, states)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("release the run right: %v", err)
	}
}

// The ordinary "a Project has at most one live operation" decision used to ride on the global lock.
// The right is per run now, so two runs of one Project can reach provisioning together; the Project
// lock is what keeps their shared invariant. The second run's decision must wait for the first
// run's, read its committed operation, and decline instead of provisioning beside it.
func TestWorkflowRunProvisioningWaitsForAnotherRunsProjectDecision(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	first, _, uid, _, project := seedWorkflowSessionScene(t, store)
	second := seedWorkflowRunInProject(t, store, first, uid)

	release, done := holdRunRightDuring(t, replica, first, func(tx *transaction) {
		if createWorkflowRunWorkspace(tx, first).B("busy") {
			panic(databaseFailure{errors.New("first run could not provision its Workspace")})
		}
	})
	advance := make(chan error, 1)
	go func() { advance <- store.AdvanceWorkflowRunsOnce(context.Background()) }()

	waitForAdvisoryWait(t, store, workflowProjectLockClass, advisoryKey(project))
	release()
	if err := <-done; err != nil {
		t.Fatalf("first replica's Project decision: %v", err)
	}
	if err := <-advance; err != nil {
		t.Fatalf("advance while the Project decision was open: %v", err)
	}

	if n := workspaceCount(t, store, first); n != 1 {
		t.Fatalf("first run owns %d Workspace(s); want exactly 1", n)
	}
	if n := workspaceCount(t, store, second); n != 0 {
		t.Fatalf("second run provisioned %d Workspace(s) beside the Project's live operation", n)
	}
	var operations int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations o
		JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.project_id=$1 AND o.kind='create_workspace'`, project).Scan(&operations); err != nil {
		t.Fatalf("count the Project's create operations: %v", err)
	}
	if operations != 1 {
		t.Fatalf("Project has %d create operations; the second run must not declare one", operations)
	}
}

// The right is the transaction, not a lease: a transaction that aborts (the shape of a replica
// crashing mid-pass) releases it with no renewal or expiry window, and the next replica takes it at
// once and advances the run from committed state.
func TestWorkflowRunRightIsReleasedWhenTheTransactionAborts(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)

	broke := errors.New("simulated replica crash")
	_, _, err := store.transactWorkflowRun(context.Background(), runID, func(*transaction) Object {
		panic(databaseFailure{broke})
	})
	if !errors.Is(err, broke) {
		t.Fatalf("aborted transaction returned %v; want the simulated crash", err)
	}

	if _, ran, err := replica.transactWorkflowRun(context.Background(), runID, func(*transaction) Object {
		return Object{}
	}); err != nil || !ran {
		t.Fatalf("second replica could not take the aborted run's right: ran=%v err=%v", ran, err)
	}
	if err := replica.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the crash: %v", err)
	}
	if status, _, states, _ := readWorkflowRun(t, replica, runID); status != "running" {
		t.Fatalf("run after crash takeover: status=%q states=%v; want running", status, states)
	}
}

// linearRunGraph is the smallest graph a single pass settles without a Node or a Workspace.
func linearRunGraph() Object {
	return wfGraph([]Object{wfNode("s", "start", ""), wfNode("o", "output", "done")},
		[][3]string{{"s", "o", ""}})
}

// A run's transaction takes the lock of the Issue it is linked to before running fn. The observable
// consequence is that a sibling run of the same Issue waits: while one replica keeps a run's right
// (and therefore its Issue lock) open, another replica's advance of the other run stops at the Issue
// lock instead of appending to the same Timeline concurrently.
func TestWorkflowRunWaitsForItsLinkedIssuesTimelineLock(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	first := seedWorkflowRunScene(t, store, linearRunGraph())
	issueID, second := seedLinkedRunsOnOneIssue(t, store, first)

	release, done := holdRunRight(t, replica, first)
	advance := make(chan error, 1)
	go func() { advance <- store.AdvanceWorkflowRunsOnce(context.Background()) }()

	waitForAdvisoryWait(t, store, workflowIssueLockClass, advisoryKey(issueID))
	release()
	if err := <-done; err != nil {
		t.Fatalf("release the held run: %v", err)
	}
	if err := <-advance; err != nil {
		t.Fatalf("advance once the Issue lock was free: %v", err)
	}
	// The pass could not touch the held run; the sibling run advanced. A later pass settles the
	// held run from committed state exactly as any tick after a lock release does.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance the formerly held run: %v", err)
	}
	assertLinkedRunsSettled(t, store, issueID, first, second)
}

// Two replicas advance the two runs of one Issue at the same time. Each run still has its own
// executor, but both project onto one Timeline, so the Issue lock must order the two transactions:
// both runs reach succeeded, both IssueRuns complete, and seq stays dense and unique. The passes are
// allowed to interleave differently from run to run; the deterministic witness that the Issue lock is
// what orders them is TestWorkflowRunWaitsForItsLinkedIssuesTimelineLock, and this test pins the end
// state that ordering exists for.
func TestWorkflowRunsOfOneIssueAdvanceConcurrentlyWithUniqueTimelineSeq(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	first := seedWorkflowRunScene(t, store, linearRunGraph())
	issueID, second := seedLinkedRunsOnOneIssue(t, store, first)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, s := range []*Store{store, replica} {
		go func(s *Store) {
			<-start
			errs <- s.AdvanceWorkflowRunsOnce(context.Background())
		}(s)
	}
	close(start)
	for range []*Store{store, replica} {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent advance pass: %v", err)
		}
	}
	assertLinkedRunsSettled(t, store, issueID, first, second)
}
