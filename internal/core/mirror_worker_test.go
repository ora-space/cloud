package core

import (
	"context"
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
	mirrorGeneration int64
	leased           bool
}

// mirrorPreparationOf reads the preparation row, or nil when the Operation has none.
func mirrorPreparationOf(t *testing.T, store *Store, operation string) *preparationRow {
	t.Helper()
	rows, err := store.Pool.Query(
		`SELECT state,COALESCE(error_code,''),COALESCE(commit_id,''),generation,
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
	if err := rows.Scan(&row.state, &row.errorCode, &row.commit, &row.generation, &row.mirrorGeneration, &lease); err != nil {
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
// Each case is a state the claim must decline to work on, so a passing case names the condition that
// held rather than the accident that nothing was claimed.
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
			// A published mirror is the finished state. Re-claiming it would fetch the same ref again.
			name: "a preparation that is already ready",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				readyMirror(t, store, s)
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
					repository_url,pinned_ref,runtime_generation,control_epoch,state,generation)
					VALUES($1,$2,$3,$4,$5,$6,1,1,'failed',3)`,
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
		RuntimeGeneration: scalarOf(t, store, `SELECT runtime_generation FROM workspaces WHERE id=$1`, scene.workspace),
	}
	if got != want {
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
