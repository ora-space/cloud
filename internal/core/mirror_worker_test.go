package core

import (
	"context"
	"slices"
	"testing"
	"time"
)

// recordingPreparer stands in for the executor. It records what it was handed so a case can assert on
// the binding the worker claimed, and answers with whatever the case decided.
type recordingPreparer struct {
	calls   []MirrorPreparation
	outcome MirrorPreparationOutcome
	err     error
}

func (r *recordingPreparer) Prepare(_ context.Context, p MirrorPreparation) (MirrorPreparationOutcome, error) {
	r.calls = append(r.calls, p)
	return r.outcome, r.err
}

// preparationRow is the part of a preparation the assertions compare. It is typed rather than a
// generic map so a renamed column fails at the query instead of silently reading as empty.
type preparationRow struct {
	state            string
	errorCode        string
	commit           string
	generation       int64
	attempts         int64
	mirrorGeneration int64
	leased           bool
}

// mirrorPreparationOf reads the preparation row, or nil when the Operation has none.
func mirrorPreparationOf(t *testing.T, store *Store, operation string) *preparationRow {
	t.Helper()
	rows, err := store.Pool.Query(
		`SELECT state,COALESCE(error_code,''),COALESCE(commit_id,''),generation,attempts,
			COALESCE(mirror_generation,0),lease_expires_at
		 FROM mirror_preparations WHERE operation_id=$1`, operation,
	)
	if err != nil {
		t.Fatalf("read the preparation for %s: %v", operation, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var row preparationRow
	var lease *time.Time
	if err := rows.Scan(&row.state, &row.errorCode, &row.commit, &row.generation, &row.attempts, &row.mirrorGeneration, &lease); err != nil {
		t.Fatalf("scan the preparation for %s: %v", operation, err)
	}
	row.leased = lease != nil
	return &row
}

// nodeScene stages a credentialed Workspace whose Operation is at `node` — the step the worker
// prepares for, and the step the `node` -> `clone` gate reads.
//
// The seed puts the Operation at `clone` because that is where the credentialed-clone tests need it;
// moving it back is what makes these tests exercise the window the worker actually works in.
func nodeScene(t *testing.T, store *Store) credentialedScene {
	t.Helper()
	s := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	execTest(t, store, `UPDATE operations SET step='node' WHERE id=$1`, s.operation)
	return s
}

// makePreparationDue stops an in-flight attempt from blocking the next pass, the way the passage of
// time would: the lease lapses and the backoff expires. Tests drive this explicitly rather than
// sleeping, so no case depends on wall-clock timing.
func makePreparationDue(t *testing.T, store *Store, operation string) {
	t.Helper()
	execTest(t, store, `UPDATE mirror_preparations SET lease_expires_at=now()-interval '1 minute',
		retry_at=now()-interval '1 minute' WHERE operation_id=$1`, operation)
}

// commitHex is a well-formed commit id. The schema requires 40 or 64 hex characters once a
// preparation is ready, so a fixture using a short string would fail on the constraint rather than on
// the behavior under test.
func commitHex() string { return "0123456789abcdef0123456789abcdef01234567" }

// scalarText reads one text column, for the assertions that compare a state or a reason code.
func scalarText(t *testing.T, store *Store, q string, args ...any) string {
	t.Helper()
	var out string
	if err := store.Pool.QueryRow(q, args...).Scan(&out); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return out
}

// TestTheMirrorWorkerClaimsOnlyOperationsThatNeedPreparing pins the policy of "who needs preparing".
// Each case is a state the claim must work on or decline to work on, so a passing case names the
// condition that held rather than the accident that nothing was claimed.
func TestTheMirrorWorkerClaimsOnlyOperationsThatNeedPreparing(t *testing.T) {
	cases := []struct {
		name       string
		apply      func(t *testing.T, store *Store, s credentialedScene)
		wantCalled int
	}{
		{
			name:       "an Operation waiting at node",
			apply:      func(*testing.T, *Store, credentialedScene) {},
			wantCalled: 1,
		},
		{
			// The gate is on the transition into `clone`, so an Operation already past `node` has either
			// been admitted with a mirror or is not this worker's business.
			name: "an Operation that is not at node",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE operations SET step='clone' WHERE id=$1`, s.operation)
			},
		},
		{
			name: "an anonymous Project",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, s.project)
			},
		},
		{
			// The one kind of Operation that stands at `node` without ever reaching `clone`: a restart's
			// Workspace already holds its content, so nothing on that path reads the mirror. Preparing one
			// would spend an upstream fetch on content nobody clones, and a credential that has since been
			// revoked would end a restart that does not need it.
			name: "a restart waiting at node",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE operations SET kind='restart' WHERE id=$1`, s.operation)
			},
		},
		{
			// A published mirror that still describes what the Workspace asks for. Re-claiming it would
			// fetch the same ref again, so this is the case the drift branch must not swallow.
			name: "a preparation that is already ready",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				readyMirror(t, store, s)
			},
		},
		{
			// The drift branch, in the shape a user produces: rotating the token advances the reference's
			// version, so the mirror was built from a fetch authorized by a credential that no longer
			// applies. The gate refuses the transition for that reason, and only the worker can fix it.
			//
			// Rotation is the drift a user can produce today. The branch compares the whole list the gate
			// re-checks — upstream, ref, runtime generation and credential identity as well as version —
			// because a state the gate refuses and the worker does not re-prepare is the same wedge, and
			// the two lists drifting apart is how this case came about.
			name: "a ready preparation the credential has moved past",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				readyMirror(t, store, s)
				execTest(t, store, `UPDATE credential_refs SET version=version+1 WHERE id=$1`, s.credential)
			},
			wantCalled: 1,
		},
		{
			// And the guard on that branch. A frozen reference is a refusal the gate reports in its own
			// words, and the owner can unfreeze it without the mirror changing: re-preparing would spend a
			// fetch to rebuild a mirror that is still the right one, and a batch of such rows would keep
			// the batch busy while nothing could be prepared.
			name: "a ready preparation whose reference is frozen",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				readyMirror(t, store, s)
				execTest(t, store, `UPDATE credential_refs SET availability='frozen' WHERE id=$1`, s.credential)
			},
		},
		{
			// Another worker holds it. The lease is the whole reason two passes do not dispatch the same
			// authorization.
			name: "a preparation an attempt is still working on",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `INSERT INTO mirror_preparations(operation_id,tenant_id,project_id,workspace_id,
					repository_url,pinned_ref,runtime_generation,control_epoch,state,generation,lease_expires_at)
					VALUES($1,$2,$3,$4,$5,$6,1,1,'dispatched',1,now()+interval '5 minutes')`,
					s.operation, s.tenant, s.project, s.workspace, s.repository, "refs/heads/"+s.ref)
			},
		},
		{
			name: "a preparation waiting out its backoff",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `INSERT INTO mirror_preparations(operation_id,tenant_id,project_id,workspace_id,
					repository_url,pinned_ref,runtime_generation,control_epoch,state,generation,lease_expires_at,retry_at)
					VALUES($1,$2,$3,$4,$5,$6,1,1,'dispatched',1,now(),now()+interval '5 minutes')`,
					s.operation, s.tenant, s.project, s.workspace, s.repository, "refs/heads/"+s.ref)
			},
		},
		{
			// The attempt bound is spent. Retrying past it would be the unbounded silent retry the bound
			// exists to prevent; the row is terminal until something changes its inputs.
			name: "a preparation that has used every attempt",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `INSERT INTO mirror_preparations(operation_id,tenant_id,project_id,workspace_id,
					repository_url,pinned_ref,runtime_generation,control_epoch,state,generation,attempts)
					VALUES($1,$2,$3,$4,$5,$6,1,1,'failed',3,3)`,
					s.operation, s.tenant, s.project, s.workspace, s.repository, "refs/heads/"+s.ref)
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			store.SetCredentialedCloner(&recordingCloner{})
			preparer := &recordingPreparer{outcome: MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 1}}
			store.SetMirrorPreparer(preparer)
			scene := nodeScene(t, store)
			c.apply(t, store, scene)

			if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
				t.Fatalf("a pass must not fail on a state it declined to claim: %v", err)
			}
			if len(preparer.calls) != c.wantCalled {
				t.Fatalf("the pass dispatched %d attempts, want %d", len(preparer.calls), c.wantCalled)
			}
		})
	}
}

// TestTheCandidateQueryOffersOnlyPreparationsItCouldCarryOut pins the half of the candidate policy the
// claim cannot show.
//
// The claim refuses a reference the Project may no longer use, so a frozen or invalid preparation
// dispatches nothing whether or not the query hands it over. What the query decides is which rows the
// pass spends its bounded, oldest-first batch on, and a row every pass returns while no pass can claim
// it would hold a slot in every batch — the drift branch's own comparison list would then be working
// against the Operations waiting behind it, until the owner acts on the reference.
//
// The two halves are the same row with one field moved, so the case cannot pass by the drift branch
// declining both: the available one must be offered, and the frozen one must not.
func TestTheCandidateQueryOffersOnlyPreparationsItCouldCarryOut(t *testing.T) {
	store := dispatcherDB(t)
	scene := nodeScene(t, store)
	readyMirror(t, store, scene)

	// Drifted, the way a user produces it: the reference has moved past the mirror the gate re-checks.
	execTest(t, store, `UPDATE workspaces SET runtime_generation=runtime_generation+1 WHERE id=$1`, scene.workspace)

	execTest(t, store, `UPDATE credential_refs SET availability='frozen' WHERE id=$1`, scene.credential)
	if ids := candidateIDs(t, store); len(ids) != 0 {
		t.Fatalf("a preparation that cannot be prepared was offered to the batch: %v", ids)
	}

	execTest(t, store, `UPDATE credential_refs SET availability='available' WHERE id=$1`, scene.credential)
	if ids := candidateIDs(t, store); len(ids) != 1 || ids[0] != scene.operation {
		t.Fatalf("an available preparation the gate refuses was not offered: %v", ids)
	}
}

// candidateIDs is the batch the pass would work on, in the order it would work on it.
func candidateIDs(t *testing.T, store *Store) []string {
	t.Helper()
	ids, err := store.mirrorPreparationCandidates(context.Background(), 8)
	if err != nil {
		t.Fatalf("read the candidate batch: %v", err)
	}
	return ids
}

// TestALostAttemptDoesNotSpendTheAttemptBudget is the wedge in the other direction, and the case the
// attempt budget was confused with the generation fence over.
//
// Three attempts are claimed and none of them answers — the shape of a process that dies between the
// claim and the result, which leaves the row `dispatched` at the bound with its lease lapsed. Nothing
// concluded, so nothing may be counted against the budget: the next pass has to claim the row again.
// A worker that read the budget off the generation number would refuse it here and the Operation would
// sit at `node` behind the gate with nothing working on the mirror — the silent wedge the bound exists
// to prevent, reached through the bound itself.
func TestALostAttemptDoesNotSpendTheAttemptBudget(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	preparer := &recordingPreparer{outcome: MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 1}}
	store.SetMirrorPreparer(preparer)
	scene := nodeScene(t, store)
	ctx := context.Background()

	for i := 0; i < mirrorPreparationAttempts; i++ {
		if i > 0 {
			makePreparationDue(t, store, scene.operation)
		}
		claimed, err := store.claimMirrorPreparation(ctx, scene.operation)
		if err != nil || claimed == nil {
			t.Fatalf("attempt %d must be claimed, got %v (err %v)", i+1, claimed, err)
		}
	}
	// The state the loss leaves: the fence at the bound, the budget untouched, and the row still claimed.
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.generation != mirrorPreparationAttempts || row.attempts != 0 {
		t.Fatalf("three lost attempts left generation=%d attempts=%d, want %d and 0",
			row.generation, row.attempts, mirrorPreparationAttempts)
	}

	makePreparationDue(t, store, scene.operation)
	if err := store.PrepareMirrorsOnce(ctx); err != nil {
		t.Fatalf("a pass must work on an Operation whose attempts were lost: %v", err)
	}
	if len(preparer.calls) != 1 {
		t.Fatalf("the pass dispatched %d attempts, want 1 after three lost ones", len(preparer.calls))
	}
	if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != "running" {
		t.Fatalf("the Operation must still be running at node, state=%q", got)
	}
	row = mirrorPreparationOf(t, store, scene.operation)
	if row.generation != mirrorPreparationAttempts+1 || row.state != "ready" {
		t.Fatalf("the reclaim is generation=%d state=%q, want %d and the mirror the reclaim published",
			row.generation, row.state, mirrorPreparationAttempts+1)
	}
	if row.attempts != 0 {
		t.Fatalf("the reclaim published a mirror, so it must clear the budget, attempts=%d", row.attempts)
	}
}

// TestAPublishedMirrorClearsTheAttemptBudget walks the whole requirement-change story, because the
// budget is only per requirement if a success clears it.
//
// One attempt fails, the next one publishes a mirror, and then the owner rotates the token. The
// rotation is a new thing to prepare — the mirror was fetched with a credential version that no longer
// applies — so it must be prepared again, and it must be prepared with a whole budget rather than with
// whatever the previous requirement left over. A count that survived the success would make the third
// rotation of a working repository the one that ends a user's Operation.
func TestAPublishedMirrorClearsTheAttemptBudget(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	preparer := &recordingPreparer{err: &Fault{Code: "mirror_upstream_unreachable", Status: 409}}
	store.SetMirrorPreparer(preparer)
	scene := nodeScene(t, store)
	ctx := context.Background()

	if err := store.PrepareMirrorsOnce(ctx); err != nil {
		t.Fatalf("a retryable failure must not fail the pass: %v", err)
	}
	preparer.err = nil
	preparer.outcome = MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 1}
	makePreparationDue(t, store, scene.operation)
	if err := store.PrepareMirrorsOnce(ctx); err != nil {
		t.Fatalf("the retry must prepare the mirror: %v", err)
	}
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.state != "ready" || row.attempts != 0 {
		t.Fatalf("a published mirror left state=%q attempts=%d, want ready and a cleared budget", row.state, row.attempts)
	}

	// The rotation: the reference advances, so the ready mirror is no longer the mirror the gate will
	// accept, and the next pass has to prepare it again.
	execTest(t, store, `UPDATE credential_refs SET version=version+1 WHERE id=$1`, scene.credential)
	if err := store.PrepareMirrorsOnce(ctx); err != nil {
		t.Fatalf("a rotation must be prepared again: %v", err)
	}
	if len(preparer.calls) != 3 {
		t.Fatalf("the pass dispatched %d attempts, want a third one for the rotated credential", len(preparer.calls))
	}
	if got := preparer.calls[2].CredentialVersion; got != 2 {
		t.Fatalf("the re-preparation was admitted for credential version %d, want the rotated version 2", got)
	}
	if got := scalarOf(t, store, `SELECT credential_ref_version FROM mirror_preparations WHERE operation_id=$1`, scene.operation); got != 2 {
		t.Fatalf("the row records credential version %d, want 2 — the gate compares exactly this", got)
	}
}

// TestARestartIsNotSettledByACredentialItDoesNotUse is the second half of the kind filter.
//
// A restart stands at `node` like a create does, but its Workspace holds its content already, so the
// restart path reads no mirror and no credential. A reference the owner has since revoked is a fact
// about remote Git work the restart never asks for, and letting the worker reach it would settle a
// local restart as a failure — with the maintenance reservation the restart holds left unreleased.
func TestARestartIsNotSettledByACredentialItDoesNotUse(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	preparer := &recordingPreparer{outcome: MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 1}}
	store.SetMirrorPreparer(preparer)
	scene := nodeScene(t, store)
	execTest(t, store, `UPDATE operations SET kind='restart' WHERE id=$1`, scene.operation)
	execTest(t, store, `UPDATE credential_refs SET availability='invalid' WHERE id=$1`, scene.credential)

	if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
		t.Fatalf("a pass must not fail on an Operation it does not prepare for: %v", err)
	}
	if len(preparer.calls) != 0 {
		t.Fatal("a restart must not be given a mirror preparation: it clones nothing")
	}
	if row := mirrorPreparationOf(t, store, scene.operation); row != nil {
		t.Fatalf("a restart must be left with no preparation, got state=%q", row.state)
	}
	if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != "running" {
		t.Fatalf("the restart must be left running, state=%q", got)
	}
	if code := scalarText(t, store, `SELECT COALESCE(error_code,'') FROM operations WHERE id=$1`, scene.operation); code != "" {
		t.Fatalf("the restart was settled with error_code=%q for a credential it never uses", code)
	}
}

// TestTheClaimedBindingIsTheOneTheGateWillReCheck pins that the attempt carries the values the
// authorization is signed against, taken from live state rather than from anything a caller passed. A
// preparation claimed for the wrong ref or the wrong credential version would publish content the gate
// then refuses, turning a recoverable preparation into a permanently failing one.
func TestTheClaimedBindingIsTheOneTheGateWillReCheck(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	preparer := &recordingPreparer{outcome: MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 1}}
	store.SetMirrorPreparer(preparer)
	scene := nodeScene(t, store)

	if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
		t.Fatalf("one pass must prepare the waiting Operation: %v", err)
	}
	if len(preparer.calls) != 1 {
		t.Fatalf("the pass dispatched %d attempts, want 1", len(preparer.calls))
	}
	got := preparer.calls[0]
	if got.AuthorizationID == "" {
		t.Fatal("the attempt must carry the authorization id it was admitted under")
	}
	want := MirrorPreparation{
		OperationID:       scene.operation,
		AuthorizationID:   got.AuthorizationID, // Minted per attempt; asserted non-empty above.
		Generation:        1,
		ControlEpoch:      scalarOf(t, store, `SELECT controller_epoch FROM operations WHERE id=$1`, scene.operation),
		TenantID:          scene.tenant,
		OwnerUserID:       scene.user,
		ProjectID:         scene.project,
		WorkspaceID:       scene.workspace,
		Repository:        scene.repository,
		Ref:               "refs/heads/" + scene.ref,
		CredentialID:      scene.credential,
		CredentialVersion: 1,
		// The capability set admission found on the reference, carried so the authorization binds what
		// the guard approved rather than a set the dispatcher would decide at signing time.
		Capabilities:      []string{"clone"},
		RuntimeGeneration: scalarOf(t, store, `SELECT runtime_generation FROM workspaces WHERE id=$1`, scene.workspace),
	}
	// Compared field by field rather than as a whole value: the claim carries a slice, so the struct is
	// no longer comparable, and `reflect.DeepEqual` would report only that something differs.
	if got.OperationID != want.OperationID || got.AuthorizationID != want.AuthorizationID ||
		got.Generation != want.Generation || got.ControlEpoch != want.ControlEpoch ||
		got.TenantID != want.TenantID || got.OwnerUserID != want.OwnerUserID ||
		got.ProjectID != want.ProjectID || got.WorkspaceID != want.WorkspaceID ||
		got.Repository != want.Repository || got.Ref != want.Ref ||
		got.CredentialID != want.CredentialID || got.CredentialVersion != want.CredentialVersion ||
		got.RuntimeGeneration != want.RuntimeGeneration ||
		!slices.Equal(got.Capabilities, want.Capabilities) {
		t.Fatalf("the claimed binding is\n%+v\nwant\n%+v", got, want)
	}

	// The claim must also leave the row in the state the gate will read, with the attempt's lease held.
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.state != "ready" || row.commit != commitHex() || row.mirrorGeneration != 1 {
		t.Fatalf("the settled preparation is %+v, want a ready one carrying the published commit", row)
	}
}

// TestASupersededAttemptCannotOverwriteThePreparationThatReplacedIt is the late-result fence.
//
// An attempt whose lease lapsed and which was replaced must not be able to write its answer over the
// attempt that replaced it. This is the failure a mirror would suffer if a slow first fetch landed
// after a second one had already published: the row would name the older commit while the newer
// attempt believed it had won.
func TestASupersededAttemptCannotOverwriteThePreparationThatReplacedIt(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	store.SetMirrorPreparer(&recordingPreparer{})
	scene := nodeScene(t, store)
	ctx := context.Background()

	first, err := store.claimMirrorPreparation(ctx, scene.operation)
	if err != nil || first == nil {
		t.Fatalf("the first claim must succeed, got %v (err %v)", first, err)
	}
	if first.Generation != 1 {
		t.Fatalf("the first attempt is generation %d, want 1", first.Generation)
	}
	makePreparationDue(t, store, scene.operation)
	second, err := store.claimMirrorPreparation(ctx, scene.operation)
	if err != nil || second == nil {
		t.Fatalf("the second claim must succeed, got %v (err %v)", second, err)
	}
	if second.Generation != 2 {
		t.Fatalf("the second attempt is generation %d, want 2 — the fence must move forward", second.Generation)
	}

	// The first attempt finally answers, after being replaced.
	applied, err := store.settleMirrorPreparation(ctx, *first, MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 7}, nil)
	if err != nil {
		t.Fatalf("settling a superseded attempt must not fail the pass: %v", err)
	}
	if applied {
		t.Fatal("a superseded attempt must not be applied to the preparation that replaced it")
	}
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.state != "dispatched" || row.generation != 2 || row.commit != "" {
		t.Fatalf("the superseded answer reached the row: %+v", row)
	}

	// The current attempt answers, and is the one that lands.
	applied, err = store.settleMirrorPreparation(ctx, *second, MirrorPreparationOutcome{Commit: commitHex(), MirrorGeneration: 8}, nil)
	if err != nil || !applied {
		t.Fatalf("the current attempt must be applied, got applied=%v err=%v", applied, err)
	}
	row = mirrorPreparationOf(t, store, scene.operation)
	if row.state != "ready" || row.mirrorGeneration != 8 {
		t.Fatalf("the current attempt did not land: %+v", row)
	}
}

// TestARetryablePreparationFailureReleasesTheAttemptWithoutEndingTheOperation pins the retry half of
// the bound: a mirror that failed for a reason that may pass is retried, and the Operation keeps
// running so the next pass can still prepare it.
func TestARetryablePreparationFailureReleasesTheAttemptWithoutEndingTheOperation(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	store.SetMirrorPreparer(&recordingPreparer{err: &Fault{Code: "mirror_upstream_unreachable", Status: 409}})
	scene := nodeScene(t, store)

	if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
		t.Fatalf("a retryable preparation failure must not fail the pass: %v", err)
	}
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.state != "dispatched" {
		t.Fatalf("a retryable failure must leave the preparation in flight, state=%q", row.state)
	}
	if row.errorCode != "mirror_upstream_unreachable" {
		t.Fatalf("the row must record why the attempt failed, errorCode=%q", row.errorCode)
	}
	if row.generation != 1 {
		t.Fatalf("releasing an attempt must not spend the next one, generation=%d", row.generation)
	}
	// The released attempt *is* spent: the budget counts the attempts that concluded, and this one
	// concluded with a failure. A row that released the lease without spending anything would retry an
	// upstream that fails on every attempt forever.
	if row.attempts != 1 {
		t.Fatalf("a concluded attempt must be counted, attempts=%d", row.attempts)
	}
	if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != "running" {
		t.Fatalf("the Operation must keep running so a later pass can prepare it, state=%q", got)
	}
}

// TestThePreparationEndsItsOperationWhenItCannotBePrepared is the other half of the bound, and the
// answer to "不能因镜像未 ready 就让 Project Operation 永久无声重试": once the attempts are spent, the
// Operation settles with a reason instead of the Controller retrying the gate forever.
func TestThePreparationEndsItsOperationWhenItCannotBePrepared(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	preparer := &recordingPreparer{err: &Fault{Code: "mirror_upstream_unreachable", Status: 409}}
	store.SetMirrorPreparer(preparer)
	scene := nodeScene(t, store)
	ctx := context.Background()

	// Three passes, and the lease is nudged lapsed only before the retries. Nudging it after the last
	// pass would touch a row that has already settled, and a terminal preparation may hold no lease —
	// which is itself a check worth not papering over.
	for i := 0; i < mirrorPreparationAttempts; i++ {
		if i > 0 {
			makePreparationDue(t, store, scene.operation)
		}
		if err := store.PrepareMirrorsOnce(ctx); err != nil {
			t.Fatalf("pass %d must not fail on a retryable refusal: %v", i+1, err)
		}
	}
	if len(preparer.calls) != mirrorPreparationAttempts {
		t.Fatalf("the worker dispatched %d attempts, want the bound %d", len(preparer.calls), mirrorPreparationAttempts)
	}
	row := mirrorPreparationOf(t, store, scene.operation)
	if row.state != "failed" || row.errorCode != "mirror_upstream_unreachable" {
		t.Fatalf("an exhausted preparation must be terminal and say why: %+v", row)
	}
	if row.attempts != mirrorPreparationAttempts {
		t.Fatalf("the preparation spent %d attempts of its budget %d", row.attempts, mirrorPreparationAttempts)
	}
	// A settled preparation carries no lease: one left behind would look claimable to the next pass.
	if row.leased {
		t.Fatal("a terminal preparation must hold no lease")
	}

	if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != "failed" {
		t.Fatalf("the Operation must settle rather than retry the gate forever, state=%q", got)
	}
	if code := scalarText(t, store, `SELECT error_code FROM operations WHERE id=$1`, scene.operation); code != MirrorPreparationFailed {
		t.Fatalf("the Operation must name the preparation as the cause, error_code=%q", code)
	}

	// And a further pass changes nothing: the bound is what stops the retry, not the passage of time.
	before := len(preparer.calls)
	if err := store.PrepareMirrorsOnce(ctx); err != nil {
		t.Fatalf("a pass after settlement must be a no-op: %v", err)
	}
	if len(preparer.calls) != before {
		t.Fatal("a settled preparation must not be claimed again")
	}
}

// TestOnlyADurableRefusalEndsTheOperation classifies the two directions a refused preparation can go,
// because getting it backwards is silent either way: treating a durable refusal as retryable leaves an
// Operation retrying a frozen reference forever, and treating a retryable one as durable settles an
// Operation the user could have fixed by reconnecting.
//
// The classification is not invented here — it is `terminalCredentialRefusals`, the same set the
// credentialed-clone path settles on.
func TestOnlyADurableRefusalEndsTheOperation(t *testing.T) {
	cases := []struct {
		name         string
		apply        func(t *testing.T, store *Store, s credentialedScene)
		installClone bool
		wantState    string
		wantCode     string
	}{
		{
			// No executor is installed, so no credentialed clone can ever be admitted. That is durable for
			// this deployment, and the Operation settles instead of retrying a condition no retry changes.
			name:         "a deployment that cannot execute credentialed clones",
			apply:        func(*testing.T, *Store, credentialedScene) {},
			installClone: false,
			wantState:    "failed",
			wantCode:     repositoryCredentialExecutorUnavailable,
		},
		{
			// A frozen reference is the retryable direction: the owner can reconnect and unfreeze it, so
			// the Operation has to stay alive for the preparation to succeed later.
			name: "a reference that is frozen",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE credential_refs SET availability='frozen' WHERE id=$1`, s.credential)
			},
			installClone: true,
			wantState:    "running",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			if c.installClone {
				store.SetCredentialedCloner(&recordingCloner{})
			}
			preparer := &recordingPreparer{}
			store.SetMirrorPreparer(preparer)
			scene := nodeScene(t, store)
			c.apply(t, store, scene)

			if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
				t.Fatalf("a refusal must be classified rather than fail the pass: %v", err)
			}
			if len(preparer.calls) != 0 {
				t.Fatal("no attempt may be dispatched for a reference that was refused")
			}
			if row := mirrorPreparationOf(t, store, scene.operation); row != nil {
				t.Fatalf("a refused admission must leave no preparation behind, got state=%q", row.state)
			}
			if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != c.wantState {
				t.Fatalf("the Operation is %q, want %q", got, c.wantState)
			}
			if c.wantCode != "" {
				if code := scalarText(t, store, `SELECT COALESCE(error_code,'') FROM operations WHERE id=$1`, scene.operation); code != c.wantCode {
					t.Fatalf("the Operation carries error_code=%q, want %q", code, c.wantCode)
				}
			}
		})
	}
}

// TestNoMirrorIsClaimedWithoutAnInstalledPreparer pins the fail-closed default. Without an executor
// there is nothing that could publish a mirror, so claiming a preparation would mark an Operation as
// being worked on while nothing worked on it.
func TestNoMirrorIsClaimedWithoutAnInstalledPreparer(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	scene := nodeScene(t, store)

	if err := store.PrepareMirrorsOnce(context.Background()); err != nil {
		t.Fatalf("a pass without an executor must be a no-op: %v", err)
	}
	if row := mirrorPreparationOf(t, store, scene.operation); row != nil {
		t.Fatalf("no preparation may be claimed without an executor, got state=%q", row.state)
	}
	if got := scalarText(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); got != "running" {
		t.Fatalf("the Operation must be left untouched, state=%q", got)
	}
}
