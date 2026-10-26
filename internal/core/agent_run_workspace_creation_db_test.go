package core

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Scenes for the D5 bounded abandonment of a blocked run Workspace creation. Everything before the
// `blocked` write uses shipped code paths: the AgentRunDispatcher claim (createRunWorkspace plus the
// provisioning transition), the control plane's own claim action and its defer action. The defer, and
// Cloud's own claim parking a clone step it cannot hand out, are the writers of `blocked` and
// therefore the transactions the deadline freezes in.

// provisioningSeed is one queued run whose claim created the run Workspace and moved the run to
// provisioning, plus the ids the abandonment asserts on.
type provisioningSeed struct {
	dispSeed
	workspace, operation string
}

// seedProvisioningCreation claims the seeded run through the shipped dispatcher: one transaction
// creates the run Workspace, its create_workspace operation and the phase/status/workspace binding.
func seedProvisioningCreation(t *testing.T, store *Store) provisioningSeed {
	t.Helper()
	seed := seedDispatchScene(t, store.Pool, false)
	if err := store.agentRunDispatcher().Dispatch(context.Background(), seed.run); err != nil {
		t.Fatalf("claim the run into provisioning: %v", err)
	}
	row := store.Pool.QueryRow(`SELECT w.id, o.id FROM issue_runs r
	 JOIN workspaces w ON w.id=r.workspace_id
	 JOIN operations o ON o.workspace_id=w.id AND o.kind='create_workspace'
	 WHERE r.id=$1`, seed.run)
	var p provisioningSeed
	p.dispSeed = seed
	if err := row.Scan(&p.workspace, &p.operation); err != nil {
		t.Fatalf("read the provisioned creation: %v", err)
	}
	status, phase := runClaim(t, store, seed.tenant, seed.run)
	if status != "dispatched" || phase.String != "provisioning" {
		t.Fatalf("dispatcher claim left the run at %s/%s", status, phase.String)
	}
	return p
}

// creationPass runs one bounded abandonment pass, the way the server loop does.
func creationPass(t *testing.T, store *Store) {
	t.Helper()
	if err := store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()); err != nil {
		t.Fatalf("blocked-creation pass: %v", err)
	}
}

// parkCreationBlocked drives the real control path onto the precondition D5 exists for: the
// Controller claims the create_workspace operation and defers it to `blocked` with a deferral code.
func parkCreationBlocked(t *testing.T, store *Store, claims *Claims, operation, code string) {
	t.Helper()
	out, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: claims})
	if err != nil {
		t.Fatalf("claim the creation operation: %v", err)
	}
	claimed := out.O("operation")
	if claimed.S("id") != operation {
		t.Fatalf("claim picked %s, not this scene's creation %s", claimed.S("id"), operation)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "defer", OperationID: operation,
		Body:    Object{"epoch": 1, "version": claimed.N("version"), "state": "blocked", "errorCode": code, "retrySeconds": 60},
		Service: claims,
	}); err != nil {
		t.Fatalf("defer the creation to blocked: %v", err)
	}
}

// expireCreationDeadline moves the frozen deadline into the past, the way waiting out the window
// would; it never touches the anchor so the source stays auditable.
func expireCreationDeadline(t *testing.T, store *Store, runID string) {
	t.Helper()
	if _, err := store.Pool.Exec("UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
}

// creationDeadline reads the frozen anchor, deadline and source of one run.
func creationDeadline(t *testing.T, store *Store, runID string) (blockedAt, deadlineAt, source sql.NullString) {
	t.Helper()
	err := store.Pool.QueryRow(`SELECT workspace_blocked_at, workspace_deadline_at, workspace_blocked_source FROM issue_runs WHERE id=$1`, runID).
		Scan(&blockedAt, &deadlineAt, &source)
	if err != nil {
		t.Fatalf("read creation deadline: %v", err)
	}
	return blockedAt, deadlineAt, source
}

// The first persistent `blocked` freezes the deadline in the same transaction; retry_wait does not.
func TestBlockedCreationFreezesDeadlineInDeferTransaction(t *testing.T) {
	store := commandStore(t)
	blocked := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	claims := controllerClaims()

	// A retry_wait deferral is a known temporary failure, not an unknown result: it must not open
	// the window (ADR D5: 只有首次 blocked 触发).
	parked, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: claims})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "defer", OperationID: blocked.operation,
		Body:    Object{"epoch": 1, "version": parked.O("operation").N("version"), "state": "retry_wait", "errorCode": "node_unavailable", "retrySeconds": 30},
		Service: claims,
	}); err != nil {
		t.Fatalf("defer to retry_wait: %v", err)
	}
	creationPass(t, store)
	if _, deadlineAt, _ := creationDeadline(t, store, blocked.run); deadlineAt.Valid {
		t.Fatal("retry_wait opened the creation window")
	}

	// Re-claim (retry_wait becomes due) and defer to blocked: the window opens, anchored at the write.
	if _, err := store.Pool.Exec(`UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1`, blocked.operation); err != nil {
		t.Fatal(err)
	}
	parked, err = store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: claims})
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "defer", OperationID: blocked.operation,
		Body:    Object{"epoch": 1, "version": parked.O("operation").N("version"), "state": "blocked", "errorCode": "node_unavailable", "retrySeconds": 60},
		Service: claims,
	}); err != nil {
		t.Fatalf("defer to blocked: %v", err)
	}
	blockedAt, deadlineAt, source := creationDeadline(t, store, blocked.run)
	if !blockedAt.Valid || !deadlineAt.Valid || source.String != "blocked_write" {
		t.Fatalf("blocked write did not freeze the deadline: %v/%v/%v", blockedAt, deadlineAt, source)
	}
	store.AgentWorkspaceBlockedTimeout = time.Hour
	creationPass(t, store)
	if _, after, _ := creationDeadline(t, store, blocked.run); after.String != deadlineAt.String {
		t.Fatalf("scan renewed the frozen deadline: %s -> %s", deadlineAt.String, after.String)
	}
}

// Cloud parking a run Workspace creation's clone step as blocked when it claims it (no Git Server
// configured, so no snapshot input can be frozen) is a persistent blocked write too: the deadline
// freezes in the claim's transaction as blocked_write, not later as first_observed.
func TestBlockedCloneParkFreezesDeadlineInClaimTransaction(t *testing.T) {
	store := commandStore(t)
	parked := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	if _, err := store.Pool.Exec(`UPDATE operations SET step='clone',state='queued' WHERE id=$1`, parked.operation); err != nil {
		t.Fatal(err)
	}
	out, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: controllerClaims()})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got := out.O("operation").S("id"); got == parked.operation {
		t.Fatal("a clone step without a Git Server was handed to the Controller")
	}
	var state, code string
	if err := store.Pool.QueryRow(`SELECT state,error_code FROM operations WHERE id=$1`, parked.operation).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "blocked" || code != gitServerUnconfigured {
		t.Fatalf("claim left the clone step %s/%s, want blocked/%s", state, code, gitServerUnconfigured)
	}
	blockedAt, deadlineAt, source := creationDeadline(t, store, parked.run)
	if !blockedAt.Valid || !deadlineAt.Valid || source.String != "blocked_write" {
		t.Fatalf("parking as blocked did not freeze the deadline: %v/%v/%v", blockedAt, deadlineAt, source)
	}
}

// Defer, manual retry, re-claiming, restarts and configuration changes never renew a frozen window;
// a temporary return to running or retry_wait does not erase it either.
func TestBlockedCreationDeadlineIsNotRenewedByRetryRestartOrConfiguration(t *testing.T) {
	store := commandStore(t)
	store.AgentWorkspaceBlockedTimeout = 5 * time.Minute
	p := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	claims := controllerClaims()
	parkCreationBlocked(t, store, claims, p.operation, "node_unavailable")
	_, deadlineAt, _ := creationDeadline(t, store, p.run)

	// A manual retry re-queues the creation; the deadline stays (the public retry action cannot
	// even reach a run Workspace, so the re-queue is staged the way it would be).
	if _, err := store.Pool.Exec(`UPDATE operations SET state='queued',retry_at=NULL,error_code=NULL,version=version+1 WHERE id=$1`, p.operation); err != nil {
		t.Fatal(err)
	}
	// A second Controller cycle defers to blocked again after re-claiming.
	parked, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: claims})
	if err != nil {
		t.Fatalf("re-claim after retry: %v", err)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "defer", OperationID: p.operation,
		Body:    Object{"epoch": 1, "version": parked.O("operation").N("version"), "state": "blocked", "errorCode": "substrate_timeout", "retrySeconds": 60},
		Service: claims,
	}); err != nil {
		t.Fatalf("re-defer to blocked: %v", err)
	}
	// A restarted replica with a different configuration runs the pass.
	restarted := *store
	restarted.AgentWorkspaceBlockedTimeout = time.Hour
	creationPass(t, &restarted)
	_, after, source := creationDeadline(t, store, p.run)
	if after.String != deadlineAt.String || source.String != "blocked_write" {
		t.Fatalf("frozen deadline moved: %s -> %s (%s)", deadlineAt.String, after.String, source.String)
	}

	// A temporary return to running does not erase the window: the deadline still governs.
	if _, err := store.Pool.Exec(`UPDATE operations SET state='running',version=version+1 WHERE id=$1`, p.operation); err != nil {
		t.Fatal(err)
	}
	creationPass(t, &restarted)
	if _, still, _ := creationDeadline(t, store, p.run); !still.Valid || still.String != deadlineAt.String {
		t.Fatal("a running creation erased the frozen window")
	}
}

// A legacy blocked creation (pre-0038 row, no deadline) is anchored once by the first policy scan as
// first_observed with one default window, and never re-anchored.
func TestLegacyBlockedCreationAnchorsOnceAsFirstObserved(t *testing.T) {
	store := commandStore(t)
	store.AgentWorkspaceBlockedTimeout = 5 * time.Minute
	p := seedProvisioningCreation(t, store)
	// The pre-upgrade shape: the operation is parked blocked with no frozen deadline, staged
	// directly because the defer path now freezes in its own transaction.
	if _, err := store.Pool.Exec(`UPDATE operations SET state='blocked',error_code='node_unavailable' WHERE id=$1`, p.operation); err != nil {
		t.Fatal(err)
	}
	creationPass(t, store)
	blockedAt, deadlineAt, source := creationDeadline(t, store, p.run)
	if !blockedAt.Valid || !deadlineAt.Valid || source.String != "first_observed" {
		t.Fatalf("first scan did not anchor the legacy creation: %v/%v/%v", blockedAt, deadlineAt, source)
	}
	phase, status, _, _, _, _ := runFields(t, store, p.run)
	if phase.String != "provisioning" || status != "dispatched" {
		t.Fatalf("anchoring settled the run: %s/%s", phase.String, status)
	}
	// Later passes with different configuration never re-anchor or renew.
	store.AgentWorkspaceBlockedTimeout = time.Hour
	creationPass(t, store)
	if _, after, afterSource := creationDeadline(t, store, p.run); after.String != deadlineAt.String || afterSource.String != "first_observed" {
		t.Fatal("a later pass re-anchored the legacy window")
	}
}

// Expiry settles Run and creation operation together, releases the Project slot for another
// Workspace, and keeps the residual Workspace protected against reuse and deletion.
func TestBlockedCreationExpirySettlesReleasesSlotAndProtectsResidual(t *testing.T) {
	store := commandStore(t)
	store.AgentWorkspaceBlockedTimeout = 30 * time.Minute
	p := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	parkCreationBlocked(t, store, controllerClaims(), p.operation, "node_unavailable")
	// An in-flight sandbox plan whose result stayed unknown: the ledger must carry it.
	effect := newID()
	if _, err := store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch)
	 VALUES($1,$2,(SELECT project_id FROM workspaces WHERE id=$3),$3,'sandbox_ensure','planned','{}',1)`, effect, p.operation, p.workspace); err != nil {
		t.Fatal(err)
	}
	// A queued user turn the abandonment must close without rewriting.
	seedQueuedUserTurn(t, store, p.run, "never sent")
	expireCreationDeadline(t, store, p.run)

	// A sibling run in the same tenant and Project is still queued: its creation must be able to
	// proceed once the blocked one releases the single one_project_operation slot. It gets its own
	// Issue so the pending-run dedup (one queued run per issue/executor) does not reject it.
	siblingIssue, siblingRun := newID(), newID()
	if _, err := store.Pool.Exec(`INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number)
	 SELECT $1,tenant_id,creator_user_id,$2,'sibling',2 FROM issues WHERE id=$3`, siblingIssue, p.project, p.issue); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status)
	 SELECT $1,tenant_id,$2,'agent',executor_id,'queued' FROM issue_runs WHERE id=$3`, siblingRun, siblingIssue, p.run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details)
	 SELECT $1,tenant_id,$2,1,'user',creator_user_id,'run.enqueued',jsonb_build_object('runId',$3::text) FROM issues WHERE id=$2`,
		newID(), siblingIssue, siblingRun); err != nil {
		t.Fatal(err)
	}

	creationPass(t, store)
	creationPass(t, store) // a repeated pass confirms the same settlement instead of rebuilding it

	phase, status, reason, _, _, _ := runFields(t, store, p.run)
	if phase.String != "done" || status != "failed" || reason != "workspace_unavailable" {
		t.Fatalf("expiry did not abandon the run: %s/%s/%s", phase.String, status, reason)
	}
	var delivery, abandonReason, abandonSource string
	if err := store.Pool.QueryRow(`SELECT result->>'deliveryState', result->'workspace'->>'reason', result->'workspace'->>'source'
	 FROM issue_runs WHERE id=$1`, p.run).Scan(&delivery, &abandonReason, &abandonSource); err != nil {
		t.Fatal(err)
	}
	if delivery != "skipped" || abandonReason != "creation_blocked_timeout" || abandonSource != "cloud" {
		t.Fatalf("settlement result lost its abandonment provenance: %s/%s/%s", delivery, abandonReason, abandonSource)
	}
	var opState, opCode string
	var runIDFromOp sql.NullString
	if err := store.Pool.QueryRow(`SELECT state, error_code, result->>'runId' FROM operations WHERE id=$1`, p.operation).Scan(&opState, &opCode, &runIDFromOp); err != nil {
		t.Fatal(err)
	}
	if opState != "failed" || opCode != "provisioning_result_unknown" || runIDFromOp.String != p.run {
		t.Fatalf("creation operation not terminally abandoned: %s/%s/%s", opState, opCode, runIDFromOp.String)
	}
	// One residual row, carrying the unknown effect plan and the reason D5 fixes.
	var ledgerCount int
	var responsibility string
	var ledgerReason string
	if err := store.Pool.QueryRow(`SELECT count(*), responsibility::text, reason FROM agent_run_residuals WHERE run_id=$1 GROUP BY responsibility, reason`, p.run).Scan(&ledgerCount, &responsibility, &ledgerReason); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 || ledgerReason != "workspace_unavailable" || !strings.Contains(responsibility, effect) {
		t.Fatalf("residual ledger incomplete: count=%d reason=%s effect=%v", ledgerCount, ledgerReason, strings.Contains(responsibility, effect))
	}
	// The Workspace stays protected: admission closed with a fenced epoch, no delete queued.
	var admissionOpen bool
	var admissionEpoch int64
	if err := store.Pool.QueryRow(`SELECT admission_open, admission_epoch FROM workspaces WHERE id=$1`, p.workspace).Scan(&admissionOpen, &admissionEpoch); err != nil {
		t.Fatal(err)
	}
	if admissionOpen || admissionEpoch == 0 {
		t.Fatalf("residual workspace lost its admission fence: open=%v epoch=%d", admissionOpen, admissionEpoch)
	}
	var deletes int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='delete_workspace'`, p.workspace).Scan(&deletes); err != nil {
		t.Fatal(err)
	}
	if deletes != 0 {
		t.Fatal("abandonment queued an ordinary delete for the residual Workspace")
	}
	// The queued user turn was discarded, and the Thread stayed undeclared (nothing fabricated).
	var queuedTurns int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user' AND status='queued'`, p.run).Scan(&queuedTurns); err != nil {
		t.Fatal(err)
	}
	if queuedTurns != 0 {
		t.Fatalf("queued user turns survived the abandonment: %d", queuedTurns)
	}
	// The residual Workspace cannot be deleted or reused: the run-scoped delete seam refuses it
	// until a separately approved recovery workflow clears the ledger row.
	if _, err := store.DeleteRunWorkspace(context.Background(), p.run); err == nil {
		t.Fatal("residual workspace deletion was accepted")
	}
	// The slot release is real: the queued sibling run's creation is no longer busy.
	if err := store.agentRunDispatcher().Dispatch(context.Background(), siblingRun); err != nil {
		t.Fatalf("sibling dispatch failed: %v", err)
	}
	var secondPhase sql.NullString
	var secondWorkspace sql.NullString
	if err := store.Pool.QueryRow(`SELECT phase, workspace_id FROM issue_runs WHERE id=$1`, siblingRun).Scan(&secondPhase, &secondWorkspace); err != nil {
		t.Fatal(err)
	}
	if secondPhase.String != "provisioning" || !secondWorkspace.Valid {
		t.Fatalf("the freed Project slot did not serve the sibling run: %s/%v", secondPhase.String, secondWorkspace.Valid)
	}
}

// A ledger write failure rolls the whole abandonment back: the Project slot is not released without
// residual ownership, and the run stays provisioning for a later pass.
func TestBlockedCreationLedgerFailureRollsBackTerminalAndSlotRelease(t *testing.T) {
	store := commandStore(t)
	p := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	parkCreationBlocked(t, store, controllerClaims(), p.operation, "node_unavailable")
	expireCreationDeadline(t, store, p.run)
	if _, err := store.Pool.Exec(`CREATE FUNCTION reject_creation_residual() RETURNS trigger LANGUAGE plpgsql AS
	 $$ BEGIN RAISE EXCEPTION 'injected residual failure'; END $$;
	 CREATE TRIGGER reject_creation_residual BEFORE INSERT ON agent_run_residuals FOR EACH ROW EXECUTE FUNCTION reject_creation_residual()`); err != nil {
		t.Fatal(err)
	}
	creationPass(t, store)
	phase, status, _, _, _, _ := runFields(t, store, p.run)
	var opState string
	if err := store.Pool.QueryRow(`SELECT state FROM operations WHERE id=$1`, p.operation).Scan(&opState); err != nil {
		t.Fatal(err)
	}
	if phase.String != "provisioning" || status != "dispatched" || opState != "blocked" {
		t.Fatalf("abandonment committed without residual ownership: %s/%s/%s", phase.String, status, opState)
	}
	if _, err := store.Pool.Exec("DROP TRIGGER reject_creation_residual ON agent_run_residuals; DROP FUNCTION reject_creation_residual()"); err != nil {
		t.Fatal(err)
	}
	creationPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, p.run)
	if phase.String != "done" || status != "failed" || reason != "workspace_unavailable" {
		t.Fatalf("recovery did not settle after the ledger became writable: %s/%s/%s", phase.String, status, reason)
	}
}

// A creation that really settles first keeps its original outcome: the deadline may have been due,
// but the run left provisioning in the creation's own terminal transaction and the abandonment is a
// no-op. An accepted provisioning cancel does not change the expiry outcome either: the deadline
// decides it as failed/done, not as a claimed physical cancellation.
func TestBlockedCreationRealSettlementAndCancelDoNotResurrectOrReorder(t *testing.T) {
	store := commandStore(t)
	seedControllerLease(t, store)
	// Real settlement first: the creation succeeds, the run reaches starting.
	settled := seedProvisioningCreation(t, store)
	parkCreationBlocked(t, store, controllerClaims(), settled.operation, "node_unavailable")
	if _, err := store.Pool.Exec(`UPDATE operations SET state='succeeded',step='done',version=version+1 WHERE id=$1`, settled.operation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		runWorkspaceSettled(tx, settled.run, "ready")
		return Object{}
	}); err != nil {
		t.Fatalf("settle the creation: %v", err)
	}
	expireCreationDeadline(t, store, settled.run)
	creationPass(t, store)
	phase, status, _, _, _, _ := runFields(t, store, settled.run)
	if phase.String != "starting" || status != "dispatched" {
		t.Fatalf("a settled creation was re-abandoned: %s/%s", phase.String, status)
	}

	// Cancel accepted during provisioning, deadline first: failed/done, cancel preserved untouched.
	cancelled := seedProvisioningCreation(t, store)
	parkCreationBlocked(t, store, controllerClaims(), cancelled.operation, "node_unavailable")
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=clock_timestamp() WHERE id=$1`, cancelled.run); err != nil {
		t.Fatal(err)
	}
	expireCreationDeadline(t, store, cancelled.run)
	creationPass(t, store)
	phase, status, reason, _, _, cancelAt := runFields(t, store, cancelled.run)
	if phase.String != "done" || status != "failed" || reason != "workspace_unavailable" || !cancelAt.Valid {
		t.Fatalf("cancel reinterpreted the abandonment: %s/%s/%s cancel=%v", phase.String, status, reason, cancelAt.Valid)
	}
	var opState string
	if err := store.Pool.QueryRow(`SELECT state FROM operations WHERE id=$1`, cancelled.operation).Scan(&opState); err != nil {
		t.Fatal(err)
	}
	if opState != "failed" {
		t.Fatalf("cancelled creation operation not abandoned: %s", opState)
	}
}

// The abandonment is keyed on the run row itself, so one tenant's due creation never settles
// another tenant's: the same pass that abandons the expired one leaves an unrelated blocked
// creation in a different tenant exactly where it is.
func TestBlockedCreationAbandonmentIsIsolatedPerTenant(t *testing.T) {
	store := commandStore(t)
	due := seedProvisioningCreation(t, store)
	seedControllerLease(t, store)
	parkCreationBlocked(t, store, controllerClaims(), due.operation, "node_unavailable")
	expireCreationDeadline(t, store, due.run)

	// A second tenant with its own blocked creation whose window has not expired.
	other := seedProvisioningCreation(t, store)
	parkCreationBlocked(t, store, controllerClaims(), other.operation, "node_unavailable")

	creationPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, due.run)
	if phase.String != "done" || status != "failed" || reason != "workspace_unavailable" {
		t.Fatalf("the due creation was not abandoned: %s/%s/%s", phase.String, status, reason)
	}
	otherPhase, otherStatus, otherReason, _, _, _ := runFields(t, store, other.run)
	if otherPhase.String != "provisioning" || otherStatus != "dispatched" || otherReason != "" {
		t.Fatalf("another tenant's creation was touched: %s/%s/%s", otherPhase.String, otherStatus, otherReason)
	}
}
