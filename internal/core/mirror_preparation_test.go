package core

import (
	"context"
	"strings"
	"testing"
)

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
		s.credential, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("insert a ready mirror preparation: %v", err)
	}
}

// mirrorGate runs the gate the way `advance` runs it — inside the control transaction, for the
// transition the Operation is about to make — and reports its verdict.
func mirrorGate(t *testing.T, store *Store, s credentialedScene) error {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		requireMirrorPrepared(tx, Object{"id": s.operation, "projectId": s.project, "workspaceId": s.workspace}, s.workspace)
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
				// content a clone could be pinned to.
				execTest(t, store, `UPDATE mirror_preparations SET state='dispatched',commit_id=NULL,mirror_generation=NULL WHERE operation_id=$1`, s.operation)
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
			// Precedence, not the mirror: the deployment installed no executor, so no credentialed clone
			// can ever be admitted and the gate must not be the thing that says so.
			name: "a deployment without an executor outranks the mirror",
			apply: func(t *testing.T, store *Store, _ credentialedScene) {
				store.SetCredentialedCloner(nil)
			},
			want: repositoryCredentialExecutorUnavailable,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			store.SetCredentialedCloner(&recordingCloner{})
			scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
			readyMirror(t, store, scene)
			c.apply(t, store, scene)

			err := mirrorGate(t, store, scene)
			if c.want == "" {
				if err != nil {
					t.Fatalf("a ready preparation that still agrees must let the transition happen, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the gate must refuse with %q", c.want)
			}
			if got := faultCode(t, err); got != c.want {
				t.Fatalf("the gate answered %q, want %q", got, c.want)
			}
		})
	}
}

// TestTheMirrorGateIsInertForAnonymousProjects pins the other half of the boundary: a Project that
// names no credential keeps the existing public-clone path, and the gate must not become a new
// precondition on it. A gate that refused here would break every anonymous clone while looking like it
// was only about mirrors.
func TestTheMirrorGateIsInertForAnonymousProjects(t *testing.T) {
	store := dispatcherDB(t)
	store.SetCredentialedCloner(&recordingCloner{})
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/public.git", "")
	execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project)

	if err := mirrorGate(t, store, scene); err != nil {
		t.Fatalf("an anonymous Project must not be gated on a mirror, got %v", err)
	}
}
