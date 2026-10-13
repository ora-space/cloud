package core

import (
	"context"
	"testing"
)

// D8 of the credentials decision makes the execution's credential audit pair a long-term obligation
// rather than optional telemetry: the reference and the version the admission validated have to be
// written with the execution row, in the same transaction, so that "which credential version did this
// clone use" stays answerable after the reference has been rotated. `execution_credential_binding` pairs
// the two columns, so the only states a row can be in are both and neither.
//
// The obligation is assertable off a real dispatch only where a credentialed clone can be admitted at
// all. With neither channel installed the guard refuses before any row exists — which is why the columns
// were declared in `0023` and never written — so the scenes below are the mirror channel, where the
// admission is served by the internal Git Server and no executor is installed.

// dispatchedMirrorScene stages the mirror channel's admitted path — a credentialed Project, a published
// mirror, the gate's transition into `clone` — and registers one clone execution through the real control
// action. That registration is the row the audit obligation is about.
func dispatchedMirrorScene(t *testing.T, store *Store, execution string) credentialedScene {
	t.Helper()
	scene := publishedMirror(t, store, seedCredentialedCloneScene(t, store, mirrorSceneRepository, admittedScope))
	// The transition first, because that is the order the two run in: the gate admits the move into
	// `clone` and pins the baseline there, and only then can a Controller dispatch the clone.
	if err := mirrorGate(t, store, scene); err != nil {
		t.Fatalf("a ready mirror must admit the transition into clone, got %v", err)
	}
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("register the clone execution: %v", err)
	}
	return scene
}

// rotateCredential drives the real rotation API, which is what advances the reference's own version. It
// is the same call the credential-ingress path serves, so the version the audit pair records is one a
// product action produced rather than one a fixture wrote.
func rotateCredential(t *testing.T, store *Store, scene credentialedScene) {
	t.Helper()
	store.Secrets = &lifecycleSecrets{}
	out, err := store.WriteGitCredential(context.Background(),
		credentialMutationRequest("PUT", scene.tenant, scene.credential, callerIdentity(t, store, scene.user), "", 1), fakeGitToken)
	if err != nil {
		t.Fatalf("rotate the credential: %v", err)
	}
	if out.Resource.N("version") != 2 {
		t.Fatalf("a rotation must advance the reference's version to 2, got %d", out.Resource.N("version"))
	}
}

// recordedExecutionCredential reads an execution's audit pair out of durable state, the way an auditor
// would. It fails on half a pair rather than reporting it as an absence: the columns are paired by
// `execution_credential_binding`, so a row carrying one and not the other is a broken invariant, and
// reading it as "no credential" would hide exactly that.
func recordedExecutionCredential(t *testing.T, store *Store, execution string) (string, int64) {
	t.Helper()
	var id *string
	var version *int64
	if err := store.Pool.QueryRow(`SELECT credential_ref_id::text,credential_ref_version FROM clone_executions WHERE execution_id=$1`, execution).Scan(&id, &version); err != nil {
		t.Fatalf("read clone execution %s: %v", execution, err)
	}
	if (id == nil) != (version == nil) {
		t.Fatalf("an execution records half a credential binding: id=%v version=%v", id, version)
	}
	if id == nil {
		return "", 0
	}
	return *id, *version
}

// TestACredentialedCloneRecordsTheCredentialVersionThatAdmittedIt is the audit obligation itself, taken
// off durable state rather than off the dispatch response: the row a real `clone_dispatch` writes has to
// carry the reference and the version the admission validated, and nothing later may move it.
func TestACredentialedCloneRecordsTheCredentialVersionThatAdmittedIt(t *testing.T) {
	t.Run("the registration carries the reference and the version admission validated", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := dispatchedMirrorScene(t, store, "exec-audit")
		if id, version := recordedExecutionCredential(t, store, "exec-audit"); id != scene.credential || version != 1 {
			t.Fatalf("the execution must record the credential that admitted it, got id=%q version=%d", id, version)
		}
	})

	// The recorded number is the one this admission read, not one the row implies: the same scene with the
	// reference already advanced records the advanced version.
	t.Run("an advanced reference is recorded at the version admission read", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := publishedMirror(t, store, seedCredentialedCloneScene(t, store, mirrorSceneRepository, admittedScope))
		rotateCredential(t, store, scene)
		// A rotation leaves the published mirror prepared under the older version, and the gate refuses that
		// — correctly, because content fetched before the reference moved is not what the new version
		// authorizes. The preparation is advanced here rather than re-published because this case is about
		// the number the registration records; the gate's own version comparison is pinned by
		// `TestTheMirrorGateRefusesACloneItsPreparationDoesNotCover`.
		execTest(t, store, `UPDATE mirror_preparations SET credential_ref_version=2 WHERE operation_id=$1`, scene.operation)
		if err := mirrorGate(t, store, scene); err != nil {
			t.Fatalf("a mirror prepared under the advanced version must admit the transition, got %v", err)
		}
		if _, err := scene.dispatch(store, "exec-rotated"); err != nil {
			t.Fatalf("register the clone execution: %v", err)
		}
		if id, version := recordedExecutionCredential(t, store, "exec-rotated"); id != scene.credential || version != 2 {
			t.Fatalf("the registration must record the version admission read, got id=%q version=%d", id, version)
		}
	})

	// The other half of D4: a rotation after the fact does not rewrite what an execution recorded. The
	// lifecycle tests pin the same rule on a row a fixture wrote; this is the row a real dispatch wrote.
	t.Run("a later rotation leaves the recorded binding alone", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := dispatchedMirrorScene(t, store, "exec-later")
		rotateCredential(t, store, scene)
		if id, version := recordedExecutionCredential(t, store, "exec-later"); id != scene.credential || version != 1 {
			t.Fatalf("a rotation rewrote the execution's binding: id=%q version=%d", id, version)
		}
	})

	// The recorded version has to be the one the content was fetched under, not merely a number the row
	// could have been written with. The gate that admitted the transition into `clone` compared the
	// preparation's version against this admission's, and the two agreeing is what lets the audit pair
	// answer "which credential version's content is in this Workspace" rather than only "which one was
	// current when the clone was registered". Read off both rows rather than inferred from the gate
	// having passed.
	t.Run("the recorded version is the one the mirror was prepared under", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := dispatchedMirrorScene(t, store, "exec-prepared")
		prepared := scalarOf(t, store, `SELECT credential_ref_version FROM mirror_preparations WHERE operation_id=$1`, scene.operation)
		if _, version := recordedExecutionCredential(t, store, "exec-prepared"); version != prepared {
			t.Fatalf("the execution records version %d while the mirror was prepared under %d", version, prepared)
		}
	})

	// The negative control, and the reason the pair is written from one decision: an anonymous clone has no
	// credential to record, and the schema wants the absence to be two nulls rather than one column set.
	t.Run("an anonymous clone records no binding at all", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := anonymousWorkspaceCloneScene(t, store)
		if _, err := scene.dispatch(store, "exec-anonymous"); err != nil {
			t.Fatalf("register the clone execution: %v", err)
		}
		if id, version := recordedExecutionCredential(t, store, "exec-anonymous"); id != "" || version != 0 {
			t.Fatalf("an anonymous clone must record no credential, got id=%q version=%d", id, version)
		}
	})
}
