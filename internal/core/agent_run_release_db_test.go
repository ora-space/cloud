package core

// White-box acceptance for the release half of an Agent IssueRun: the `releasing →
// done` transition behind the RunWorkspaceDeleted hook (IssueRun D3/D4, controller-integration D6).
// Everything here is real PostgreSQL in an isolated schema; the hook is driven
// inside a caller-owned transaction exactly as the delete_workspace terminal transaction drives it.
//
// Why these live in the core package rather than only in integration: the obligation under test is a
// transaction-scoped hook contract — which phase it accepts, what it refuses, and that a replay
// changes nothing — and the release half must be provable without standing up a Node, a Controller
// and a whole delete operation. The end-to-end chain (a real delete_workspace operation reaching
// `succeeded` and settling the run) is integration's job.

import (
	"context"
	"database/sql"
	"testing"
)

// releaseScene is one seeded Agent run bound to its own isolated run Workspace.
type releaseScene struct {
	seed  dispSeed
	runws string
}

// seedReleaseScene stages the seeded run into the phase the release transition starts from and binds
// a fresh isolated run Workspace to it. `releasing` runs carry no Thread state; the `status` is
// whatever D4 derived when the run entered the phase, which is exactly the value `done` must leave
// alone.
func seedReleaseScene(t *testing.T, store *Store, phase, status string) releaseScene {
	t.Helper()
	seed := seedDispatchScene(t, store.Pool, false)
	var owner string
	if err := store.Pool.QueryRow(`SELECT owner_user_id FROM projects WHERE id=$1`, seed.project).Scan(&owner); err != nil {
		t.Fatalf("load project owner: %v", err)
	}
	runws := newID()
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("release scene begin: %v", err)
	}
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("release scene %s: %v", name, err)
		}
	}
	// The run Workspace a release deletes: isolated, bound to this exact run by the unique
	// workspaces.issue_run_id the operation identity is defined on.
	// Match insertWorkspace's verified_request identity at insertion: migration 0018 requires
	// evidence whenever creator_user_id is non-NULL, and the creator tuple is immutable afterward.
	exec("run-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref,issue_run_id)
		VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','deleted','deleting',1,'HEAD',$5)`, runws, seed.tenant, owner, seed.project, seed.run)
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'release task')`, newID(), runws)
	exec("stage", `UPDATE issue_runs SET phase=$2, status=$3, workspace_id=$4, version=version+1, updated_at=now() WHERE id=$1`,
		seed.run, phase, status, runws)
	if err := tx.Commit(); err != nil {
		t.Fatalf("release scene commit: %v", err)
	}
	return releaseScene{seed: seed, runws: runws}
}

// settleRunDeleted drives the RunWorkspaceDeleted core inside one caller-owned transaction, rolling
// the transaction back through the databaseFailure control-flow boundary when the hook refuses —
// exactly what the delete_workspace terminal transaction does with a hook error.
func settleRunDeleted(t *testing.T, store *Store, runID string) error {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		if e := store.settleRunWorkspaceDeleted(tx, runID); e != nil {
			panic(databaseFailure{e})
		}
		return Object{}
	})
	return err
}

// runPhaseStatus reads a run's phase, status and version, which is the whole observable surface of
// this transition.
func runPhaseStatus(t *testing.T, store *Store, runID string) (phase, status string, version int64) {
	t.Helper()
	var p sql.NullString
	if err := store.Pool.QueryRow(`SELECT phase, status, version FROM issue_runs WHERE id=$1`, runID).Scan(&p, &status, &version); err != nil {
		t.Fatalf("read run phase/status: %v", err)
	}
	return p.String, status, version
}

// T5B-... — `releasing → done` is the terminal transition, and it deliberately changes no business
// outcome: D3's table gives `done` no status of its own ("终态不变"), so whatever D4 derived when the
// run entered `releasing` stands. A `done` run may therefore be `completed`, `cancelled` or `failed`
// depending on how the Agent's session ended — never on whether the delete succeeded.
func TestRunWorkspaceDeletedMovesReleasingToDoneAndKeepsStatus(t *testing.T) {
	for _, status := range []string{"completed", "cancelled", "failed"} {
		t.Run(status, func(t *testing.T) {
			store := dispatcherDB(t)
			scene := seedReleaseScene(t, store, "releasing", status)
			_, _, before := runPhaseStatus(t, store, scene.seed.run)

			if err := settleRunDeleted(t, store, scene.seed.run); err != nil {
				t.Fatalf("delete settle: %v", err)
			}
			phase, got, version := runPhaseStatus(t, store, scene.seed.run)
			if phase != "done" {
				t.Fatalf("the delete settle must reach done, got phase=%q", phase)
			}
			if got != status {
				t.Fatalf("`done` must not rewrite the status D4 derived: got %q want %q", got, status)
			}
			if version != before+1 {
				t.Fatalf("the transition must bump the version exactly once: got %d want %d", version, before+1)
			}
		})
	}
}

// A run already `done` is a deterministic no-op: the delete operation's terminal state is a fact that
// may be replayed (a Controller re-driving a committed terminal write, a recovery pass re-reading the
// run), and settling it twice must write nothing — no version bump, no second business fact.
func TestRunWorkspaceDeletedReplayIsANoOp(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedReleaseScene(t, store, "releasing", "completed")
	if err := settleRunDeleted(t, store, scene.seed.run); err != nil {
		t.Fatalf("first delete settle: %v", err)
	}
	phase, status, version := runPhaseStatus(t, store, scene.seed.run)

	if err := settleRunDeleted(t, store, scene.seed.run); err != nil {
		t.Fatalf("a replayed delete settle must be a no-op, not a fault: %v", err)
	}
	p2, s2, v2 := runPhaseStatus(t, store, scene.seed.run)
	if p2 != phase || s2 != status || v2 != version {
		t.Fatalf("a replayed settle must write nothing: %s/%s/%d became %s/%s/%d", phase, status, version, p2, s2, v2)
	}
}

// The transition is entered only from `releasing`. Every other phase is an invariant contradiction —
// the delete is declared only from `releasing`, so no other phase can have a succeeded delete to
// settle — and it must roll the caller's transaction back rather than be tolerated: a run still
// `delivering` whose delete terminal write was somehow reached would otherwise be declared finished
// while its Revision was never delivered.
func TestRunWorkspaceDeletedRefusesPhasesThatCannotHaveASucceededDelete(t *testing.T) {
	for _, phase := range []string{"provisioning", "starting", "running", "delivering"} {
		t.Run(phase, func(t *testing.T) {
			store := dispatcherDB(t)
			scene := seedReleaseScene(t, store, phase, "running")
			before, _, version := runPhaseStatus(t, store, scene.seed.run)

			if err := settleRunDeleted(t, store, scene.seed.run); err == nil {
				t.Fatalf("a delete settle from %s must be refused", phase)
			}
			got, _, v2 := runPhaseStatus(t, store, scene.seed.run)
			if got != before || v2 != version {
				t.Fatalf("a refused settle must roll back whole: %q/%d became %q/%d", before, version, got, v2)
			}
		})
	}
}

// An unknown run is an invariant violation, not a replay: the delete operation's workspace→run
// binding resolved to a run that is gone, which must abort the terminal write rather than succeed
// silently (the run's evidence is what the operation is for).
func TestRunWorkspaceDeletedRefusesAnUnknownRun(t *testing.T) {
	store := dispatcherDB(t)
	if err := settleRunDeleted(t, store, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("a delete settle for an unknown run must be refused")
	}
}

// deliveryRetryBackoff is IssueRun D5's own numbers: the exponential backoff starts at 30 s, doubles
// per attempt and caps at 10 minutes. It is asserted here as a table so a later change to the
// schedule has to be deliberate rather than an accident of arithmetic.
func TestDeliveryRetryBackoffFollowsD5sNumbers(t *testing.T) {
	cases := []struct {
		attempted int
		want      int // seconds
	}{
		{0, 30}, {1, 30}, {2, 60}, {3, 120}, {4, 240}, {5, 480}, {6, 600}, {7, 600}, {16, 600}, {1000, 600},
	}
	for _, c := range cases {
		if got := int(deliveryRetryBackoff(c.attempted).Seconds()); got != c.want {
			t.Errorf("backoff after %d failed attempts = %ds, want %ds", c.attempted, got, c.want)
		}
	}
}
