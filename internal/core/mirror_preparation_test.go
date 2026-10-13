package core

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// mirrorCommit is the commit every staged mirror in these tests pins. It is the value the gate writes as
// the Workspace's baseline and the value `advanceClone` then requires a clone to land on, so a case that
// changes it says which of the two comparisons it is testing.
const mirrorCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// mirrorBaseURL is the internal Git Server's sandbox-facing address these scenes name. Nothing dials it:
// it is the base the mirror's address is built from, and the assertions are about the address rather than
// about a reachable host.
const mirrorBaseURL = "https://git-server.example.invalid"

// installMirrorEndpoint names the internal Git Server on the Store, the way `cmd/server` does at startup.
func installMirrorEndpoint(t *testing.T, store *Store) *MirrorEndpoint {
	t.Helper()
	endpoint, err := NewMirrorEndpoint(mirrorBaseURL)
	if err != nil {
		t.Fatalf("name the internal Git Server: %v", err)
	}
	store.SetMirrorEndpoint(endpoint)
	return endpoint
}

// readyMirror inserts the preparation a credentialed Workspace must have before it may enter `clone`.
//
// Every value here is one the gate compares, so a case that changes exactly one of them says which
// comparison it is testing. `runtime_generation` is read from the Workspace rather than assumed, so
// the "still agrees" case stays honest if the schema's default ever moves.
func readyMirror(t *testing.T, store *Store, s credentialedScene) {
	t.Helper()
	generation := scalarOf(t, store, `SELECT runtime_generation FROM workspaces WHERE id=$1`, s.workspace)
	if _, err := store.Pool.Exec(`INSERT INTO mirror_preparations(operation_id,tenant_id,project_id,workspace_id,
		repository_url,pinned_ref,runtime_generation,control_epoch,state,credential_ref_id,credential_ref_version,commit_id,mirror_generation)
		VALUES($1,$2,$3,$4,$5,$6,$7,1,'ready',$8,1,$9,1)`,
		s.operation, s.tenant, s.project, s.workspace, s.repository, "refs/heads/"+s.ref, generation,
		s.credential, mirrorCommit); err != nil {
		t.Fatalf("insert a ready mirror preparation: %v", err)
	}
}

// publishedMirror stages both halves of the mirror channel — the deployment names the internal Git
// Server, and the Operation has a ready preparation — and answers with the scene whose dispatch names the
// mirror, which is what a Controller derives from that state once its Project is credentialed.
//
// The address is derived here from the same three facts the Git Server would, rather than read back out
// of the code under test. A test that took the address from `cloneInputFor` would agree with whatever it
// produced, including an address the server does not serve.
func publishedMirror(t *testing.T, store *Store, s credentialedScene) credentialedScene {
	t.Helper()
	endpoint := installMirrorEndpoint(t, store)
	readyMirror(t, store, s)
	repository, err := cloneauth.CanonicalRepository(s.repository)
	if err != nil {
		t.Fatalf("canonicalize the scene's repository: %v", err)
	}
	address, err := endpoint.CloneAddress(mirror.Key{ProjectID: s.project, Repository: repository, Commit: mirrorCommit})
	if err != nil {
		t.Fatalf("build the mirror's address: %v", err)
	}
	s.cloneRepository, s.cloneBranch = address, mirrorpublish.MirrorBranch
	return s
}

// mirrorGate runs the gate the way `advance` runs it — inside the control transaction, for the
// transition the Operation is about to make — and reports its verdict.
func mirrorGate(t *testing.T, store *Store, s credentialedScene) error {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.store.requireMirrorPrepared(tx, Object{"id": s.operation, "projectId": s.project, "workspaceId": s.workspace}, s.workspace)
		return Object{}
	})
	return err
}

// TestTheMirrorGateRefusesACloneItsPreparationDoesNotCover is the negative matrix for the `node` ->
// `clone` gate. Each case starts from a preparation that does agree and breaks exactly one thing, so a
// passing case names the comparison that held rather than the coincidence that nothing was checked.
//
// Two cases are about precedence rather than about the mirror: a frozen reference and a deployment
// with no executor must keep answering with their own codes. `mirror_not_ready` is retryable while
// those two settle an Operation, so answering them with the mirror's code would turn an Operation that
// settles today into one that retries forever — and would name the mirror as the obstacle when the
// cause is the reference or the deployment.
func TestTheMirrorGateRefusesACloneItsPreparationDoesNotCover(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T, store *Store, s credentialedScene)
		want  string
	}{
		{
			name:  "a ready preparation that still agrees",
			apply: func(*testing.T, *Store, credentialedScene) {},
		},
		{
			name: "no preparation at all",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `DELETE FROM mirror_preparations WHERE operation_id=$1`, s.operation)
			},
			want: mirrorNotReady,
		},
		{
			name: "a preparation that has not finished",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				// A dispatched preparation has no published artifact yet, so it names no immutable
				// content a clone could be pinned to. It holds a live lease, because an attempt in
				// flight is exactly what `dispatched` means — 0036 refuses the state without one.
				execTest(t, store, `UPDATE mirror_preparations SET state='dispatched',lease_expires_at=now()+interval '5 minutes',commit_id=NULL,mirror_generation=NULL WHERE operation_id=$1`, s.operation)
			},
			want: mirrorNotReady,
		},
		{
			name: "a preparation pinned to another ref",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE mirror_preparations SET pinned_ref='refs/heads/somewhere-else' WHERE operation_id=$1`, s.operation)
			},
			want: mirrorNotReady,
		},
		{
			// Rebinding: the preparation was built under one credential and the Project now names
			// another, so the mirror's content came from a fetch this Project is no longer entitled to.
			name: "a preparation built under a credential the Project was rebound away from",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				other := newID()
				execTest(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,availability,repository_scope,capabilities)
					VALUES($1,$2,$3,'git','credential-ref/2','rebound','personal','available',$4,'["clone"]'::jsonb)`,
					other, s.tenant, s.user, "https://git.example.invalid/group/private")
				execTest(t, store, `UPDATE mirror_preparations SET credential_ref_id=$2 WHERE operation_id=$1`, s.operation, other)
			},
			want: mirrorNotReady,
		},
		{
			name: "a preparation from an older runtime generation",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE mirror_preparations SET runtime_generation=runtime_generation+1 WHERE operation_id=$1`, s.operation)
			},
			want: mirrorNotReady,
		},
		{
			name: "a preparation built before the credential was rotated",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE credential_refs SET version=version+1 WHERE id=$1`, s.credential)
			},
			want: mirrorNotReady,
		},
		{
			name: "a preparation built from a repository the Project no longer names",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE mirror_preparations SET repository_url='https://git.example.invalid/group/other' WHERE operation_id=$1`, s.operation)
			},
			want: mirrorNotReady,
		},
		{
			// Precedence, not the mirror: the reference is frozen, and that answer settles the Operation.
			name: "a frozen reference outranks the mirror",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE credential_refs SET availability='frozen' WHERE id=$1`, s.credential)
			},
			want: "repository_credential_frozen",
		},
		{
			// Precedence, not the mirror: the deployment installed no channel at all, so no credentialed
			// clone can ever be admitted and the gate must not be the thing that says so. Both halves are
			// cleared, because either one alone is a channel that can serve the work.
			name: "a deployment with neither channel outranks the mirror",
			apply: func(t *testing.T, store *Store, _ credentialedScene) {
				store.SetCredentialedCloner(nil)
				store.SetMirrorEndpoint(nil)
			},
			want: repositoryCredentialExecutorUnavailable,
		},
		{
			// The mirror channel is what a deployment names when its Workspace content arrives from the
			// internal Git Server, and a mirror with no server to read it from is not an input: there is no
			// address Cloud may write, so the transition must not happen. This is the fail-closed direction
			// rather than a fallback to the upstream, which is the whole point of the channel decision.
			name: "a deployment that named no Git Server has no input at all",
			apply: func(t *testing.T, store *Store, _ credentialedScene) {
				store.SetMirrorEndpoint(nil)
			},
			want: mirrorNotReady,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			store.SetCredentialedCloner(&recordingCloner{})
			installMirrorEndpoint(t, store)
			scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
			readyMirror(t, store, scene)
			c.apply(t, store, scene)

			err := mirrorGate(t, store, scene)
			if c.want == "" {
				if err != nil {
					t.Fatalf("a ready preparation that still agrees must let the transition happen, got %v", err)
				}
				// The transition is also where the Workspace's baseline is pinned, and the pin is what makes
				// the mirror a sandbox is authorized to read the same mirror Cloud recorded: the Git Server
				// derives its key from this column. A gate that admitted the transition without writing it
				// would leave every read of the mirror refused.
				if got := textOf(t, store, "SELECT base_commit_id FROM workspaces WHERE id=$1", scene.workspace); got != mirrorCommit {
					t.Fatalf("the gate must pin the mirror's commit as the baseline, got %q", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("the gate must refuse with %q", c.want)
			}
			if got := faultCode(t, err); got != c.want {
				t.Fatalf("the gate answered %q, want %q", got, c.want)
			}
			// A refusal is a refusal of the whole transition, baseline included: the Workspace must not be
			// left naming a commit no gate admitted.
			if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id IS NULL", scene.workspace); n != 1 {
				t.Fatal("a refused transition moved the Workspace's baseline")
			}
		})
	}
}

// TestTheMirrorGateIsInertForAnonymousProjects pins the other half of the boundary: a Project that
// names no credential keeps the existing public-clone path, and the gate must not become a new
// precondition on it. A gate that refused here would break every anonymous clone while looking like it
// was only about mirrors.
//
// The baseline is asserted too, because the pin is the one thing this function writes: an anonymous
// Workspace's baseline is whatever its own clone produces, so the gate must leave the column alone
// rather than pinning it to a mirror's commit that does not exist for this Project.
func TestTheMirrorGateIsInertForAnonymousProjects(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	installMirrorEndpoint(t, store)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/public.git", "")
	execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project)

	if err := mirrorGate(t, store, scene); err != nil {
		t.Fatalf("an anonymous Project must not be gated on a mirror, got %v", err)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id IS NULL", scene.workspace); n != 1 {
		t.Fatal("the gate pinned a baseline for an anonymous Workspace, which owns its own clone result")
	}
}

// TestAnUnboundProjectsSnapshotIsNeverRedirectedAtItsOldMirror is the projection-level half of the
// public-clone regression, and the state it stages is a supported one rather than a contrived one:
// unbinding a Project's credential turns it back into an anonymous Project that dispatches and runs
// (`TestSettlementRefusesToWriteOnceTheBindingIsGone` states that as the reason its fence exists).
//
// The window is what makes it worth pinning. A Project that was credentialed has a `ready`
// preparation for this Operation, and nothing deletes that row when the binding goes: the reclaim
// sweep withdraws the mirror *content* because the reference set no longer names it, but the
// preparation row is the Operation's own history and stays `ready`. So the snapshot is derived in a
// state where a credentialed-looking preparation and an anonymous Project are both present, and the
// order in which the derivation consults them decides what the Controller is handed. Consulting the
// preparation first — the natural way to write the projection if the anonymous branch is forgotten —
// would hand the Controller the address of a mirror that the sweep has already withdrawn, and the
// Workspace would dispatch a clone of content the Git Server no longer serves.
//
// The assertion is on the snapshot because that is what the Controller reads `repository_url` and
// `requested_ref` from (`cloud/operations/steps.rs`, `dispatch_clone`), so an address that never
// appears here can never become a clone.
func TestAnUnboundProjectsSnapshotIsNeverRedirectedAtItsOldMirror(t *testing.T) {
	store := dispatcherDB(t)
	// The whole credentialed channel, staged: the deployment names the internal Git Server, the Project
	// is bound, and a ready preparation exists for this Operation.
	scene := publishedMirror(t, store, seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private"))
	if scene.cloneRepository == "" {
		t.Fatal("the scene must have a published mirror, or this case asserts nothing")
	}
	// Then the one change: the Project no longer names a credential. The mirror address above belongs
	// to a binding that no longer exists.
	execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project)

	// The snapshot is read through the function the Controller's own path uses (`claim`, `operation`),
	// rather than through `projectMirrorInput`'s two writes, so the assertion is about what is handed
	// out and not about an intermediate.
	snap, err := store.transact(context.Background(), func(tx *transaction) Object {
		o := tx.one("SELECT * FROM operations WHERE id=$1", scene.operation)
		if o == nil {
			return Object{}
		}
		return snapshot(tx, o)
	})
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	project, ok := snap["project"].(Object)
	if !ok || project == nil {
		t.Fatal("the snapshot carries no Project")
	}
	if got := project.S("repositoryUrl"); got != scene.repository {
		t.Fatalf("an unbound Project's snapshot names %q, want its own upstream %q", got, scene.repository)
	}
	workspaces, ok := snap["workspaces"].([]Object)
	if !ok || len(workspaces) != 1 {
		t.Fatalf("the snapshot carries %d workspaces, want the one this Operation clones", len(workspaces))
	}
	// The Workspace's ref is the other half of the same decision: a mirrored clone is pinned to the
	// mirror's single branch rather than to the ref the Workspace asked for, so a ref that still names
	// `release/2026-10` is the second half of "this clone was not redirected".
	if got := workspaces[0].S("requestedRef"); got != scene.ref {
		t.Fatalf("the Workspace's requested ref became %q, want %q", got, scene.ref)
	}
	// And the stronger statement, which is the one the risk is actually about: no part of the snapshot
	// names the artifact built for the credential the Project no longer has. The address cannot appear
	// in a field this test did not think to check.
	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("encode the snapshot: %v", err)
	}
	if bytes.Contains(encoded, []byte(scene.cloneRepository)) {
		t.Fatalf("the snapshot names the old mirror's address %q for a Project that no longer has a credential", scene.cloneRepository)
	}
}

// TestAMirrorDeploymentsWorkspaceCloneNamesTheMirror is the dispatch-level half of D1/D7: on a
// deployment that named the internal Git Server, a credentialed Workspace's clone is the Node's clone of
// the published mirror, Cloud executes no clone of its own, and the upstream address never appears in
// the record the Node is handed.
//
// Nothing here installs a credentialed-clone executor, which is the point: on this channel the
// credential is spent by the preparation, so the clone needs none. The three cases are the three states
// the dispatch can be in — mirror ready with the mirror named, mirror ready with the upstream named, and
// no mirror at all — and each asserts what the Controller would get.
func TestAMirrorDeploymentsWorkspaceCloneNamesTheMirror(t *testing.T) {
	t.Run("a ready mirror admits exactly the mirror's input and leaves the clone to the Node", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := publishedMirror(t, store, seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private"))
		// The transition first, because that is the order the two run in: the gate admits the move into
		// `clone` and pins the baseline there, and only then can a Controller dispatch the clone.
		if err := mirrorGate(t, store, scene); err != nil {
			t.Fatalf("a ready mirror must admit the transition into clone, got %v", err)
		}

		out, err := scene.dispatch(store, "exec-mirror")
		if err != nil {
			t.Fatalf("a Workspace whose mirror is ready must dispatch its clone, got %v", err)
		}
		// The Node's clone is the work this execution stands for, so Cloud leaves the result unwritten.
		// A result written here would tell the Controller the clone was already done, and the Workspace
		// would never be cloned at all.
		if result := out.O("result"); len(result) != 0 {
			t.Fatalf("the execution must wait for the Node's result, got %v", result)
		}
		// D7, read off durable state rather than off the response: the recorded input is what the
		// Controller's CloneSpec is generated from, so this is the record that must never name the
		// upstream.
		recorded := textOf(t, store, "SELECT input->>'repositoryUrl' FROM clone_executions WHERE execution_id=$1", "exec-mirror")
		if recorded != scene.cloneRepository {
			t.Fatalf("the recorded clone input must be the mirror's address, got %q", recorded)
		}
		if recorded == scene.repository || recorded == scene.repository+".git" {
			t.Fatal("the recorded clone input names the Project's upstream")
		}
		if branch := textOf(t, store, "SELECT input->>'branch' FROM clone_executions WHERE execution_id=$1", "exec-mirror"); branch != mirrorpublish.MirrorBranch {
			t.Fatalf("the recorded clone input must name the mirror's branch %q, got %q", mirrorpublish.MirrorBranch, branch)
		}
		if got := textOf(t, store, "SELECT base_commit_id FROM workspaces WHERE id=$1", scene.workspace); got != mirrorCommit {
			t.Fatalf("the dispatch must leave the pinned baseline in place, got %q", got)
		}
	})

	t.Run("the upstream is not an admissible input on this deployment", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := publishedMirror(t, store, seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private"))
		// A Controller that was handed the upstream — a stale snapshot, or one built before the mirror was
		// published — must not be able to register it.
		upstream := scene
		upstream.cloneRepository, upstream.cloneBranch = scene.repository, scene.ref

		_, err := upstream.dispatch(store, "exec-upstream")
		if got := faultCode(t, err); got != "dispatch_conflict" {
			t.Fatalf("dispatch of the upstream input: got %q, want dispatch_conflict", got)
		}
		if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE execution_id=$1", "exec-upstream"); n != 0 {
			t.Fatal("a refused dispatch registered an execution")
		}
	})

	t.Run("a deployment with no mirror yet refuses retryably rather than cloning the upstream", func(t *testing.T) {
		store := dispatcherDB(t)
		installMirrorEndpoint(t, store)
		scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")

		_, err := scene.dispatch(store, "exec-late")
		if got := faultCode(t, err); got != mirrorNotReady {
			t.Fatalf("dispatch before the mirror is ready: got %q, want %q", got, mirrorNotReady)
		}
		if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE execution_id=$1", "exec-late"); n != 0 {
			t.Fatal("a clone refused for a missing mirror registered an execution")
		}
		// Retryable rather than settled: `mirror_not_ready` is not one of the codes that ends an
		// Operation, because a preparation that is merely late becomes ready.
		if n := scalarOf(t, store, "SELECT count(*) FROM operations WHERE id=$1 AND state='running' AND step='clone'", scene.operation); n != 1 {
			t.Fatal("a mirror that is not ready yet settled the Operation instead of leaving it to retry")
		}
	})
}
