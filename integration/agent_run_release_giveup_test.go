package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// The acceptance suite for IssueRun D8: a run whose Workspace Node never answers again leaves
// `releasing` for `done` on D5's unreachability window alone, and Cloud never reports that as a
// released Workspace.
//
// Every case starts from `driveToReleasing`, which is D5's own give-up settlement — so the state D8
// acts on is produced by the production delivery path, not seeded. The give-up condition itself is
// produced the same way: aging the Node's `last_seen_at` is the durable form of "the Node's state is
// unknown", and the window is a configured duration compared on the database clock, so no case sleeps.

// releasingRun is one scene driven to exactly D8's starting point — the run `releasing` with D5's
// give-up settlement — together with the phase trace that reached it.
type releasingRun struct {
	liveThreadScene
	// execution is the settled delivery execution, for cases that need to address it.
	execution string
	// phases is the run's own observed history up to `releasing`, oldest first, so the lifecycle
	// assertions read what the run actually did rather than a list of expected values.
	phases []string
}

// driveToReleasing builds the D8 starting point through production code: a live session that ends
// with the given reason, a delivery that D5 gives up on (the configured failure window is armed at
// a nanosecond, so the very failure that would otherwise retry releases the run), and the delete
// intent that release declares. Every phase is observed as it happens.
func driveToReleasing(t *testing.T, f *fixture, reason controlpb.AgentSessionEndReason) releasingRun {
	t.Helper()
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	phases := []string{f.runPhase(scene.runID)}
	scene.start(t, f)
	f.runningThread(t, scene)
	phases = append(phases, f.runPhase(scene.runID))
	f.sessionEndOK(t, scene, "", 2, reason)
	phases = append(phases, f.runPhase(scene.runID))
	execution := f.deliverRevision(t, scene)
	f.store.DeliveryGiveUpAfter = time.Nanosecond
	if _, e := f.deliveryTerminal(scene, execution, "", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
		"revision_failed/upload_failed"); e != nil {
		t.Fatalf("the D5 give-up result must be taken over: %v", e)
	}
	phases = append(phases, f.runPhase(scene.runID))
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("the scene must be releasing, got %q", got)
	}
	return releasingRun{liveThreadScene: scene, execution: execution, phases: phases}
}

// runFailureReason reads the run's own record of why Cloud stopped: the column D3 already uses for
// the Workspace it could not make available, and the one D8 reuses.
func (f *fixture) runFailureReason(runID string) string {
	f.t.Helper()
	var reason string
	must(f.t, f.store.Pool.QueryRow(`SELECT failure_reason FROM issue_runs WHERE id=$1`, runID).Scan(&reason))
	return reason
}

// ageRunWorkspaceNodeBy pushes the run Workspace's Node heartbeat age into the past, which is the
// durable form of "the Node's state has been unknown for this long". The database clock decides the
// comparison, so a case proves a window either side of the boundary without waiting for it.
func (f *fixture) ageRunWorkspaceNodeBy(runID string, age time.Duration) error {
	f.t.Helper()
	_, e := f.store.Pool.Exec(`
		UPDATE node_instances SET last_seen_at=now()-make_interval(secs => $2)
		WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1) AND ended_at IS NULL`,
		runID, age.Seconds())
	return e
}

// ageDeleteAttempts ages the run Workspace's delete intent, which is how a case separates "the
// delete has been failing for a long time" from "the Node is unreachable". D8 must be moved by the
// second and never by the first.
func (f *fixture) ageDeleteAttempts(runID string, age time.Duration) error {
	f.t.Helper()
	_, e := f.store.Pool.Exec(`
		UPDATE operations SET created_at=now()-make_interval(secs => $2), updated_at=now()-make_interval(secs => $2)
		WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1) AND kind='delete_workspace'`,
		runID, age.Seconds())
	return e
}

// workspaceTruth reads the Workspace columns that decide whether Cloud told the truth about it: the
// binding to the run, the soft-delete marker, admission, and the row's own version.
func (f *fixture) workspaceTruth(wid string) core.Object {
	f.t.Helper()
	var desired, observed, runID string
	var open, deleted bool
	var epoch, version int64
	must(f.t, f.store.Pool.QueryRow(`
		SELECT desired_state, observed_state, admission_open, admission_epoch, version,
		       deleted_at IS NOT NULL, COALESCE(issue_run_id::text,'')
		FROM workspaces WHERE id=$1`, wid).
		Scan(&desired, &observed, &open, &epoch, &version, &deleted, &runID))
	return core.Object{
		"desiredState": desired, "observedState": observed, "admissionOpen": open,
		"admissionEpoch": epoch, "version": version, "deleted": deleted, "issueRunId": runID,
	}
}

// assertGiveUpSettlement requires the whole of D8's terminal state, in the places it is observable:
// the run reached `done` with its business outcome untouched and its reason recorded, and the
// unfinished delete is terminally failed with no successful deletion anywhere.
func assertGiveUpSettlement(t *testing.T, f *fixture, runID, status string) {
	t.Helper()
	if got := f.runPhase(runID); got != "done" {
		t.Fatalf("D8 must settle the run to done, got phase %q", got)
	}
	if got, _ := f.runStatusVersion(runID); got != status {
		t.Fatalf("D8 must not rewrite the business outcome: status %q, want %q", got, status)
	}
	if got := f.runFailureReason(runID); got != "workspace_unavailable" {
		t.Fatalf("D8 must record failure_reason=workspace_unavailable, got %q", got)
	}
	ops := deleteOperations(f.runOperations(runID))
	if len(ops) == 0 {
		t.Fatal("the scene must hold a delete intent")
	}
	for _, op := range ops {
		if op.S("state") == "succeeded" {
			t.Fatalf("D8 must never leave a successful deletion result, got %v", op)
		}
	}
	last := ops[len(ops)-1]
	if last.S("state") != "failed" || last.S("errorCode") != "node_unavailable" {
		t.Fatalf("the unfinished delete must be terminally failed as node_unavailable, got %v", last)
	}
	// And out of every worker's reach: no ordinary claim can pick it up, so the residual Workspace is
	// never retried by the machinery D8 just stopped.
	var unfinished int
	must(t, f.store.Pool.QueryRow(`
		SELECT count(*) FROM operations
		WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1)
		  AND kind='delete_workspace' AND state IN ('queued','running','retry_wait','blocked')`, runID).Scan(&unfinished))
	if unfinished != 0 {
		t.Fatalf("a given-up delete must leave no unfinished operation, got %d", unfinished)
	}
}

// driveDeleteTolerant drives the run Workspace's operations exactly as driveDelete does, but returns
// the Controller's own error instead of failing the test. Under a D8 race the operation can be
// terminalized between two of the Controller's steps, and its next advance is then a legal refusal
// rather than a fault.
func (f *fixture) driveDeleteTolerant(t *testing.T, runID string) error {
	t.Helper()
	f.bindRunWorkspaceSandbox(t, runID)
	f.pointLeaseAtSimulator(t)
	return f.controller.Drain(context.Background())
}

// pointLeaseAtSimulator hands the global lease to the fixture's simulator Controller with the epoch it
// holds, which is what a real Controller takeover performs. The Thread scenes' seed points the lease
// at `ctrl-a`, so the operation path needs it moved before the simulator may claim anything.
func (f *fixture) pointLeaseAtSimulator(t *testing.T) {
	t.Helper()
	_, e := f.store.Pool.Exec(
		`UPDATE controller_leases SET holder_id=$2, epoch=$1, expires_at=clock_timestamp()+interval '1 hour' WHERE name='global'`,
		f.controller.Epoch, f.client.Subject,
	)
	must(t, e)
}

// D8 changes nothing about the ordinary release. A delete that succeeds still settles
// the run `done` with no failure reason at all, and the give-up pass is inert on a finished run even
// when its window is armed: the unreachability limit is not a second, parallel completion path.
func TestG032_1_SuccessfulDeleteIsUnchangedByTheGiveUpPass(t *testing.T) {
	f := setup(t)
	// Armed, so the pass below really scans and really evaluates the condition; the workspace is
	// gone by then, which is the whole point.
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	wid := f.runWorkspaceID(run.runID)

	f.driveDelete(t, run.runID)

	if got := f.runPhase(run.runID); got != "done" {
		t.Fatalf("a succeeded delete must settle the run to done, got %q", got)
	}
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("a successful release must not record a failure reason, got %q", got)
	}
	status, _ := f.runStatusVersion(run.runID)
	version := f.runVersion(run.runID)
	ws := f.workspaceTruth(wid)
	if !ws.B("deleted") || ws.S("issueRunId") != run.runID {
		t.Fatalf("the Workspace must be soft-deleted and keep its binding, got %v", ws)
	}
	// A late pass — the race the ADR asks about from the other side — writes nothing at all.
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("the give-up pass must be inert on a finished run: version %d → %d", version, got)
	}
	if got, _ := f.runStatusVersion(run.runID); got != status {
		t.Fatalf("the pass must not rewrite the status: %q → %q", status, got)
	}
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("the pass must not manufacture a failure reason, got %q", got)
	}
	if got := f.workspaceTruth(wid); got.S("version") != ws.S("version") {
		t.Fatalf("the pass must not touch the Workspace, got %v (was %v)", got, ws)
	}
}

// The window is a limit, not an immediate verdict. A Node whose heartbeat is younger
// than the configured window leaves the run `releasing` with everything untouched, so a Controller
// that is merely between reports is never abandoned.
func TestG032_2_UnreachableBelowTheWindowStaysReleasing(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)

	must(t, f.ageRunWorkspaceNodeBy(run.runID, time.Minute))
	version := f.runVersion(run.runID)
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("the scene must start with no failure reason, got %q", got)
	}
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

	if got := f.runPhase(run.runID); got != "releasing" {
		t.Fatalf("a Node unreachable for less than the window must not end the release, got %q", got)
	}
	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("a pass that gives up on nothing must write nothing: version %d → %d", version, got)
	}
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("no give-up means no failure reason, got %q", got)
	}
	ops := deleteOperations(f.runOperations(run.runID))
	if len(ops) != 1 || ops[0].S("state") != "queued" {
		t.Fatalf("the delete intent must stay queued, got %v", ops)
	}
}

// The core case. A Node unknown for longer than the window ends the
// release: the run reaches `done` with its business status untouched and failure_reason set, while
// the Workspace it could not delete is still there, still bound, and never claimed as released.
func TestG032_3_UnreachablePastTheWindowSettlesDoneWithoutClaimingDeletion(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	wid := f.runWorkspaceID(run.runID)
	status, _ := f.runStatusVersion(run.runID)
	version := f.runVersion(run.runID)
	before := f.workspaceTruth(wid)
	activities := f.activityCount(run.issueID)

	// Two hours past a one-hour window: the condition holds by a margin, on database time.
	must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

	assertGiveUpSettlement(t, f, run.runID, status)
	if got := f.runVersion(run.runID); got != version+1 {
		t.Fatalf("D8 is exactly one write: version %d → %d", version, got)
	}
	// The Workspace is a residual resource, and it must be readable as one: still present, still
	// bound to the run that could not delete it, and never soft-deleted or rewritten.
	after := f.workspaceTruth(wid)
	if after.B("deleted") {
		t.Fatalf("D8 must not soft-delete the Workspace, got %v", after)
	}
	if after.S("issueRunId") != run.runID {
		t.Fatalf("D8 must not clear the run binding, got %v", after)
	}
	if after.S("version") != before.S("version") {
		t.Fatalf("D8 must not write the Workspace at all: %v → %v", before, after)
	}
	if got := f.runWorkspaceID(run.runID); got != wid {
		t.Fatalf("the run must keep its Workspace, got %q want %q", got, wid)
	}
	// The delivery conclusion D4 derived is not rewritten by a release-side decision.
	if got := f.runResult(run.runID).S("deliveryState"); got != "failed" {
		t.Fatalf("D8 must leave the delivery outcome alone, got %q", got)
	}
	// D8 decides nothing about the business outcome, so it publishes no Timeline activity: the
	// event that carries the outcome is the `releasing` transition, which already happened.
	if got := f.activityCount(run.issueID); got != activities {
		t.Fatalf("D8 must not publish an activity: %d → %d", activities, got)
	}
}

// D8 is an unreachability limit, not a delete-retry limit. A Node that explicitly
// refuses to quiesce keeps the run `releasing` no matter how long the delete has been failing, and
// the release keeps being pursued: Cloud re-declares, and the run still finishes normally.
func TestG032_4_RefusedQuiesceNeverTriggersGiveUpEvenPastTheWindow(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Minute
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	wid := f.runWorkspaceID(run.runID)
	f.bindRunWorkspaceSandbox(t, run.runID)

	ops := deleteOperations(f.runOperations(run.runID))
	if len(ops) != 1 {
		t.Fatalf("the scene must hold one delete intent, got %d", len(ops))
	}
	// The Node answers, and its answer is "no": a well-formed refusal that fails the operation and
	// restores the Workspace. The Node is reachable — it just spoke.
	body, statusCode, e := f.refuseQuiesce(ops[0].S("id"), wid)
	must(t, e)
	if statusCode != 200 || body.B("accepted") || body.S("errorCode") != "resource_in_use" {
		t.Fatalf("the refusal must be the contract's verdict, got HTTP %d %v", statusCode, body)
	}
	if got := f.runPhase(run.runID); got != "releasing" {
		t.Fatalf("a refused quiesce must leave the run releasing, got %q", got)
	}

	// The delete intent has now been outstanding far longer than the window, while the Node's
	// heartbeat stays fresh. That is the whole difference between D8 and a retry timeout.
	must(t, f.ageDeleteAttempts(run.runID, time.Hour))
	version := f.runVersion(run.runID)
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

	if got := f.runPhase(run.runID); got != "releasing" {
		t.Fatalf("a reachable Node's refusal is not D8's condition, got phase %q", got)
	}
	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("the pass must write nothing while the Node is reachable: %d → %d", version, got)
	}
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("D8's reason must not be recorded for a refusal, got %q", got)
	}
	if ws := f.workspaceTruth(wid); ws.B("deleted") {
		t.Fatalf("the Workspace must be untouched, got %v", ws)
	}

	// The release is still alive: operation D4's redeclaration gives Cloud a fresh attempt, and the
	// run still reaches `done` the ordinary way.
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
	ops = deleteOperations(f.runOperations(run.runID))
	if len(ops) != 2 || ops[1].S("state") != "queued" {
		t.Fatalf("the refused intent must be re-declared, got %v", ops)
	}
	f.driveDelete(t, run.runID)
	if got := f.runPhase(run.runID); got != "done" {
		t.Fatalf("the re-declared delete must settle the run, got %q", got)
	}
	if got := f.runFailureReason(run.runID); got != "" {
		t.Fatalf("the ordinary release must carry no failure reason, got %q", got)
	}
}

// When the delete succeeds, D8 has nothing left to decide. The success commits
// the run `done` with no failure reason, and a pass that runs afterwards — including one racing it
// from the same start — is a deterministic no-op.
func TestG032_5_SuccessfulDeleteWinsTheRaceAndThePassIsANoOp(t *testing.T) {
	t.Run("the pass after a committed success", func(t *testing.T) {
		f := setup(t)
		f.store.DeliveryUnreachableAfter = time.Hour
		run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)

		f.driveDelete(t, run.runID)
		if got := f.runPhase(run.runID); got != "done" {
			t.Fatalf("the delete must settle the run, got %q", got)
		}
		version := f.runVersion(run.runID)

		// The Node is now as unreachable as it will ever be, and the window is armed: only the run's
		// own phase keeps the pass away, which is exactly what the ADR requires.
		must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
		must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

		if got := f.runVersion(run.runID); got != version {
			t.Fatalf("a success must leave D8 nothing to decide: version %d → %d", version, got)
		}
		if got := f.runFailureReason(run.runID); got != "" {
			t.Fatalf("a successful release must never gain D8's reason, got %q", got)
		}
		ops := deleteOperations(f.runOperations(run.runID))
		if len(ops) != 1 || ops[0].S("state") != "succeeded" {
			t.Fatalf("the successful delete must stand, got %v", ops)
		}
	})

	t.Run("the pass and the delete settle from one start", func(t *testing.T) {
		f := setup(t)
		// A window short enough that a Node which has already reported is still "unknown" to it, while
		// it stays well inside the quiesce step's own freshness requirement. Both outcomes are
		// therefore reachable, and the global transaction serialization decides which one happens.
		f.store.DeliveryUnreachableAfter = time.Nanosecond
		run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
		must(t, f.ageRunWorkspaceNodeBy(run.runID, time.Minute))

		var wg sync.WaitGroup
		start := make(chan struct{})
		var passErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			passErr = f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background())
		}()
		var driveErr error
		go func() {
			defer wg.Done()
			<-start
			driveErr = f.driveDeleteTolerant(t, run.runID)
		}()
		close(start)
		wg.Wait()

		must(t, passErr)
		// A mid-drive terminalization is a legal refusal, not a fault: the Controller's next advance
		// is answered with the operation's own state. Any other error would be a real defect.
		if driveErr != nil {
			var fault *core.Fault
			if !errors.As(driveErr, &fault) || fault.Code != "stale_operation" {
				t.Fatalf("the losing side of the race must fail as a stale operation, got %v", driveErr)
			}
		}

		// Whatever the lock order produced, the run is finished and the two outcomes are the only
		// ones permitted — never a `done` run that both claims a deletion and reports it failed.
		if got := f.runPhase(run.runID); got != "done" {
			t.Fatalf("either side must finish the run, got phase %q", got)
		}
		status, _ := f.runStatusVersion(run.runID)
		reason := f.runFailureReason(run.runID)
		ops := deleteOperations(f.runOperations(run.runID))
		succeeded := 0
		for _, op := range ops {
			if op.S("state") == "succeeded" {
				succeeded++
			}
		}
		switch reason {
		case "":
			// The delete won: exactly one successful result, and the Workspace is gone.
			if succeeded != 1 {
				t.Fatalf("a run with no failure reason must hold exactly one successful delete, got %v", ops)
			}
			if ws := f.workspaceTruth(f.runWorkspaceID(run.runID)); !ws.B("deleted") {
				t.Fatalf("a successful delete must leave the Workspace deleted, got %v", ws)
			}
		case "workspace_unavailable":
			// D8 won: no successful result anywhere, and the Workspace is still there.
			assertGiveUpSettlement(t, f, run.runID, status)
			if ws := f.workspaceTruth(f.runWorkspaceID(run.runID)); ws.B("deleted") {
				t.Fatalf("a given-up release must leave the Workspace in place, got %v", ws)
			}
		default:
			t.Fatalf("the race may only end with a released or a given-up run, got reason %q", reason)
		}
	})
}

// When D8 wins, the delete's own result is stale and must not be able to
// reverse the run, rewrite the operation into a success, or report the residual Workspace as
// deleted. The Controller's next settlement for that operation is refused by the operation's state.
func TestG032_6_GiveUpWinsTheRaceAndAStaleDeleteSettlementIsRefused(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	wid := f.runWorkspaceID(run.runID)
	f.bindRunWorkspaceSandbox(t, run.runID)

	// Renew the seeded lease and claim the delete, which is the state a Controller is in while it
	// drives the operation: `running`, owned by this epoch.
	_, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "lease_renew", Body: core.Object{"epoch": 1}, Service: controllerClaims(),
	})
	must(t, e)
	claimed, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "claim", Body: core.Object{"epoch": 1}, Service: controllerClaims(),
	})
	must(t, e)
	op := claimed.O("operation")
	if op.S("kind") != "delete_workspace" || op.S("state") != "running" {
		t.Fatalf("the claim must hand the Controller the run's delete, got %v", op)
	}
	opID, opVersion := op.S("id"), op.N("version")

	// D8 wins while the Controller holds the operation.
	must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	status, _ := f.runStatusVersion(run.runID)
	assertGiveUpSettlement(t, f, run.runID, status)
	version := f.runVersion(run.runID)
	before := f.workspaceTruth(wid)

	// The Controller's stale settlement for the same operation: refused, because the operation is no
	// longer running. Nothing about the refusal may move the run back or invent a deletion.
	_, e = f.store.Control(context.Background(), &core.ControlRequest{
		Action: "advance", OperationID: opID,
		Body: core.Object{"epoch": 1, "version": opVersion}, Service: controllerClaims(),
	})
	var fault *core.Fault
	if !errors.As(e, &fault) || fault.Code != "stale_operation" {
		t.Fatalf("a stale delete settlement must be refused as stale_operation, got %v", e)
	}

	if got := f.runPhase(run.runID); got != "done" {
		t.Fatalf("a stale settlement must not reverse the terminal phase, got %q", got)
	}
	if got, _ := f.runStatusVersion(run.runID); got != status {
		t.Fatalf("a stale settlement must not rewrite the status, got %q", got)
	}
	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("a refused settlement must write nothing: version %d → %d", version, got)
	}
	if got := f.runFailureReason(run.runID); got != "workspace_unavailable" {
		t.Fatalf("the give-up's reason must stand, got %q", got)
	}
	after := f.workspaceTruth(wid)
	if after.B("deleted") || after.S("version") != before.S("version") {
		t.Fatalf("the residual Workspace must not be reported deleted: %v → %v", before, after)
	}
	assertGiveUpSettlement(t, f, run.runID, status)
}

// A run D8 settled is out of every recovery path. The release is never re-declared,
// the deletion is never retried, and the second give-up pass cannot write a second time.
func TestG032_7_ADoneRunIsNeverRedeclaredOrRewritten(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	status, _ := f.runStatusVersion(run.runID)
	assertGiveUpSettlement(t, f, run.runID, status)

	version := f.runVersion(run.runID)
	ops := len(deleteOperations(f.runOperations(run.runID)))
	// Three ticks of every recovery pass the run could conceivably be in, plus the delete driver
	// itself. A `done` run is out of all of their scans.
	for i := 0; i < 3; i++ {
		must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
		must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
		must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	}
	// And the ordinary worker itself, handed the control plane, is left with nothing to execute.
	f.pointLeaseAtSimulator(t)
	f.drain()

	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("a given-up run must be inert: version %d → %d", version, got)
	}
	if got := len(deleteOperations(f.runOperations(run.runID))); got != ops {
		t.Fatalf("no pass may declare a second delete, got %d (was %d)", got, ops)
	}
	if got := f.runFailureReason(run.runID); got != "workspace_unavailable" {
		t.Fatalf("repeated ticks must not restate the reason, got %q", got)
	}
	if got, _ := f.runStatusVersion(run.runID); got != status {
		t.Fatalf("repeated ticks must not rewrite the status, got %q", got)
	}
}

// `done` is not a business outcome. All three session outcomes D4 derives survive
// D8 untouched: the give-up decides only that Cloud stopped waiting, never how the Agent's run ended.
func TestG032_8_BusinessStatusMatrixSurvivesTheGiveUp(t *testing.T) {
	cases := []struct {
		name   string
		reason controlpb.AgentSessionEndReason
		status string
	}{
		{"completed", controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "completed"},
		{"cancelled", controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED, "cancelled"},
		{"failed", controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_AGENT_FAILED, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.store.DeliveryUnreachableAfter = time.Hour
			run := driveToReleasing(t, f, c.reason)
			if got, _ := f.runStatusVersion(run.runID); got != c.status {
				t.Fatalf("the scene must be %s, got %q", c.status, got)
			}

			must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
			must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

			assertGiveUpSettlement(t, f, run.runID, c.status)
			// The two entries into `done` are not the same statement, and the difference is
			// observable: an ordinary release carries no reason, D8's carries its own.
			if got := f.runFailureReason(run.runID); got == "" {
				t.Fatal("D8's `done` must be distinguishable from a successful release")
			}
		})
	}
}

// No backward lifecycle. The phases a D8 run passed through advance exactly one stage
// at a time, and every transition that would move it backwards, re-open a closed stage, or convert
// one business outcome into another is refused or is a no-op.
func TestG032_9_NoBackwardLifecycleAfterGiveUp(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	must(t, f.ageRunWorkspaceNodeBy(run.runID, 2*time.Hour))
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
	status, _ := f.runStatusVersion(run.runID)
	assertGiveUpSettlement(t, f, run.runID, status)

	trace := append(append([]string{}, run.phases...), f.runPhase(run.runID))
	assertForwardOnly(t, trace)
	if got := trace[len(trace)-1]; got != "done" {
		t.Fatalf("the observed trace must end at done, got %v", trace)
	}

	// `done` is terminal: none of the three writes that could re-open a closed stage may move the
	// run, and none may turn one business outcome into another.
	version := f.runVersion(run.runID)
	for _, attempt := range []struct {
		name string
		run  func() error
	}{
		{"the delivery give-up pass", func() error {
			return f.store.GiveUpStaleDeliveriesOnce(context.Background())
		}},
		{"the delete reconciliation pass", func() error {
			return f.store.RedeclareRunWorkspaceDeletesOnce(context.Background())
		}},
		{"the release give-up pass", func() error {
			return f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background())
		}},
	} {
		must(t, attempt.run())
		if got := f.runPhase(run.runID); got != "done" {
			t.Fatalf("%s reopened a closed stage: done → %q", attempt.name, got)
		}
	}
	// The hook that normally settles `releasing → done` is reached only from a delete operation the
	// Controller has driven to `succeeded`. D8 left none claimable, so a replayed deletion cannot reach
	// the run at all — which is the forward-only half of the same fact.
	claimable := f.scalar(`
		SELECT count(*) FROM operations
		WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1)
		  AND kind='delete_workspace' AND (state='queued' OR (state='retry_wait' AND retry_at<=clock_timestamp()) OR state='running')`,
		run.runID)
	if claimable != 0 {
		t.Fatalf("a given-up release must leave nothing for a Controller to claim, got %d", claimable)
	}
	if got := f.runVersion(run.runID); got != version {
		t.Fatalf("no backward attempt may write: version %d → %d", version, got)
	}
	if got, _ := f.runStatusVersion(run.runID); got != status {
		t.Fatalf("no backward attempt may convert the outcome: %q → %q", status, got)
	}
}

// activityCount is the number of Timeline activities the issue holds, which is how a case proves a
// transition published nothing.
func (f *fixture) activityCount(issueID string) int {
	f.t.Helper()
	return f.scalar(`SELECT count(*) FROM issue_activities WHERE issue_id=$1`, issueID)
}
