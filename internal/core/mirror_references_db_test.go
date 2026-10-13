package core

// The mirror reference set is what the reclaim loop sweeps against, and it is the one query in this
// repository that has to agree with two consumers at once: the read authorization (`gitReadState`) and
// the clone input (`cloneInputFor`). A key it omits is a mirror the sweep may destroy, so the cases
// below are the ones where the two consumers and the query have to line up — and, in two of them, where
// they deliberately do not.
//
// The rows come from the real seed (`seedCredentialedCloneScene`, `readyMirror`), which writes the same
// columns the control plane writes; what is under test is the projection, not how the rows got there.
// The one thing the seed cannot do is produce a real credentialed clone's baseline, because a
// credentialed clone is refused on this deployment until the executor exists — so the case that needs a
// pinned baseline pins it the way the gate does.

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// mirrorSceneRepository is the upstream every case in this file is built on. It carries the decorations
// a caller actually pastes — a trailing `.git` — so the assertions exercise the canonicalization the
// key is built from rather than a repository spelling that happens to already be canonical.
const mirrorSceneRepository = "https://git.example.invalid/group/private.git"

// mirrorOtherCommit is a second commit of the same repository, used by the cases that need two keys of
// one Project to tell them apart.
const mirrorOtherCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// mirrorReferences asks Cloud's rows the question the reclaim loop asks.
func mirrorReferences(t *testing.T, store *Store) mirrorpublish.Binding {
	t.Helper()
	binding, err := (&mirrorpublish.References{Pool: store.Pool}).Binding(context.Background())
	if err != nil {
		t.Fatalf("read Cloud's mirror references: %v", err)
	}
	return binding
}

// sceneKey is the mirror key one scene's Project and commit resolve to, derived the same way both
// consumers derive it: the Project, its canonical upstream and the preparation's commit.
func sceneKey(t *testing.T, s credentialedScene, commit string) mirror.Key {
	t.Helper()
	canonical, err := cloneauth.CanonicalRepository(s.repository)
	if err != nil {
		t.Fatalf("canonicalize the scene's repository: %v", err)
	}
	return mirror.Key{ProjectID: s.project, Repository: canonical, Commit: commit}
}

// countKey reports how many entries of the live set name this key, which is both the membership test
// and the dedupe assertion: two consumers naming one mirror must produce one entry, not two.
func countKey(binding mirrorpublish.Binding, key mirror.Key) int {
	count := 0
	for i := range binding.Live {
		if binding.Live[i].Key == key {
			count++
		}
	}
	return count
}

// TestMirrorReferencesAreTheKeysTheTwoConsumersDerive is the predicate matrix. Each case starts from a
// scene where the reference is live and breaks exactly one thing, so a case that passes names the
// condition that held rather than the coincidence that nothing was checked.
func TestMirrorReferencesAreTheKeysTheTwoConsumersDerive(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T, store *Store, s credentialedScene)
		want  int
		// absent names a second commit of the same repository whose key must not be live at all. It is
		// what lets a case add a row that must be ignored without hiding the row that must not be.
		absent string
	}{
		{
			// The clone half on its own, and the reason it exists: the mirror is published, the
			// Operation that will consume it is still at `node`, and the Workspace has no baseline yet,
			// so the read half names nothing at all. A sweep here would reclaim the mirror the gate is
			// about to require.
			name: "a ready preparation for an Operation that has not settled",
			want: 1,
		},
		{
			// Both halves at once, which is the state a Workspace is in for the rest of its life. The
			// count is the assertion: a union that reported the same mirror twice would make the writer
			// look at one key twice and report it twice.
			name: "a Workspace that has pinned the mirror's commit, before the Operation settles",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE workspaces SET base_commit_id=$2 WHERE id=$1`, s.workspace, mirrorCommit)
			},
			want: 1,
		},
		{
			// The read half on its own: the Operation is over and its preparation row is historical, but
			// the Workspace still pins the commit and a sandbox may still read it.
			name: "a Workspace that has pinned the mirror's commit, after the Operation settled",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE workspaces SET base_commit_id=$2 WHERE id=$1`, s.workspace, mirrorCommit)
				execTest(t, store, `UPDATE operations SET state='succeeded' WHERE id=$1`, s.operation)
			},
			want: 1,
		},
		{
			// Neither half. The Operation ended without pinning a baseline, so nothing will ever consume
			// this mirror and the sweep may reclaim it: this is the leak the settled-Operation limit on
			// the clone half exists to prevent.
			name: "a settled Operation that pinned no baseline",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE operations SET state='failed' WHERE id=$1`, s.operation)
			},
			want: 0,
		},
		{
			// The channel predicate: an anonymous Project clones its upstream and is never served from a
			// mirror, so a key derived for it is content no clone input and no reader is entitled to.
			name: "an anonymous Project",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, s.project)
			},
			want: 0,
		},
		{
			// The same Project with its baseline pinned, which is what puts the *read* half's copy of the
			// channel predicate under test: without a baseline that half names nothing at all, so the case
			// above would pass whether or not it had the predicate.
			name: "an anonymous Project whose Workspace pins a commit anyway",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, s.project)
				execTest(t, store, `UPDATE workspaces SET base_commit_id=$2 WHERE id=$1`, s.workspace, mirrorCommit)
			},
			want: 0,
		},
		{
			// The window the read path was fixed for: the first Workspace of a credentialed Project
			// clones its mirror while the Project is still `provisioning`, because `openWorkspace` sets
			// `active` at the end of that same Operation. A projection that admitted only `active` would
			// reclaim the mirror of the clone that is happening.
			name: "a Project that is still provisioning",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET lifecycle='provisioning' WHERE id=$1`, s.project)
			},
			want: 1,
		},
		{
			name: "a Project being deleted",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET lifecycle='deleting' WHERE id=$1`, s.project)
			},
			want: 0,
		},
		{
			name: "a deleted Project",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE projects SET deleted_at=now() WHERE id=$1`, s.project)
			},
			want: 0,
		},
		{
			// A Workspace that has been deleted stops keeping its mirror alive, and the baseline it still
			// carries is what makes that worth asserting: the column is not cleared when a Workspace goes,
			// so without the predicate the key would be live forever and its mirror would never be
			// reclaimed.
			//
			// The deleted Workspace is a second, isolated one rather than the main one, because a live
			// Project may not be left without its main Workspace (0001's `project_main`): under a live
			// Project an isolated Workspace is the only deletable kind, so this is also the only shape the
			// case can take in production. The main Workspace keeps its own baseline, which is the row
			// that must stay live, so the two keys differ and nothing is hidden by the other.
			name: "a deleted Workspace",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE workspaces SET base_commit_id=$2 WHERE id=$1`, s.workspace, mirrorCommit)
				execTest(t, store, `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref,base_commit_id,deleted_at)
					VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','deleted','deleted',$5,$6,now())`,
					newID(), s.tenant, s.user, s.project, s.ref, mirrorOtherCommit)
			},
			want:   1,
			absent: mirrorOtherCommit,
		},
		{
			// A Project rebound to another upstream. The preparation names content fetched for a
			// repository this Project no longer clones from, so the clone input refuses it and the mirror
			// must not be kept on its account.
			name: "a preparation pinned to a repository the Project no longer names",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE mirror_preparations SET repository_url='https://git.example.invalid/group/other' WHERE operation_id=$1`, s.operation)
			},
			want: 0,
		},
		{
			// A preparation whose fetch is still in flight has published no commit, which the schema
			// states as much as it can: `ready` requires both. There is nothing to key a mirror by.
			name: "a preparation that has not finished",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				execTest(t, store, `UPDATE mirror_preparations SET state='dispatched',lease_expires_at=now()+interval '5 minutes',commit_id=NULL,mirror_generation=NULL WHERE operation_id=$1`, s.operation)
			},
			want: 0,
		},
		{
			// The deliberate latitude, asserted so it stays deliberate. `cloneInputFor` also compares the
			// preparation's credential against the Project's and would refuse this row; the projection
			// keeps the mirror anyway, because the two agree on the key — the Project, the repository and
			// the commit — and a key the projection omits is one the sweep may destroy. Keeping content
			// an in-flight Operation is between attempts at is the safe direction; a re-preparation
			// rewrites this same row, and the replacement is what the gate will then require.
			name: "a preparation built under a credential the Project was rebound away from",
			apply: func(t *testing.T, store *Store, s credentialedScene) {
				other := newID()
				execTest(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,availability,repository_scope,capabilities)
					VALUES($1,$2,$3,'git','credential-ref/2','rebound','personal','available',$4,'["clone"]'::jsonb)`,
					other, s.tenant, s.user, "https://git.example.invalid/group/private")
				execTest(t, store, `UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, s.project, other)
			},
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			scene := seedCredentialedCloneScene(t, store, mirrorSceneRepository, "https://git.example.invalid/group/private")
			readyMirror(t, store, scene)
			if c.apply != nil {
				c.apply(t, store, scene)
			}
			key := sceneKey(t, scene, mirrorCommit)

			binding := mirrorReferences(t, store)
			// Every scene here names one Project, one repository and one candidate commit, so Cloud's
			// answer is either exactly that key or nothing. Asserting the whole set rather than membership
			// is what makes a wrong key visible: an entry naming another repository, another project or a
			// commit nobody pinned is a mirror no clone input and no reader could ever ask for, and it
			// would otherwise satisfy a membership test by simply being absent.
			if len(binding.Live) != c.want {
				t.Fatalf("the live set holds %d keys, want %d: %v", len(binding.Live), c.want, binding.Live)
			}
			for i := range binding.Live {
				if binding.Live[i].Key != key {
					t.Fatalf("the live set names %s, want %s", binding.Live[i].Key.String(), key.String())
				}
				// The reference carries the facts the fetch needs, and the branch is one of them: the writer
				// fetches this ref and refuses to publish unless its tip is the pinned commit. It is carried
				// in the short spelling, because that is what the fetch takes.
				if binding.Live[i].Upstream != scene.ref {
					t.Fatalf("the reference names upstream %q, want the Workspace's requested ref %q", binding.Live[i].Upstream, scene.ref)
				}
				if binding.Live[i].TenantID != scene.tenant {
					t.Fatalf("the reference names tenant %q, want %q", binding.Live[i].TenantID, scene.tenant)
				}
			}
			if c.absent != "" {
				if got := countKey(binding, sceneKey(t, scene, c.absent)); got != 0 {
					t.Fatalf("the live set names the row that must be ignored %d times: %v", got, binding.Live)
				}
			}
		})
	}
}

// TestMirrorReferenceProjectsAreTheWithdrawalInput pins the other half of Cloud's answer. Withdrawal is
// about the Project rather than about a Workspace, so the live Projects have to be listed separately —
// and only the ones a mismatch can be observed against, which is what `active` means here: a Project
// that is going away is not a Project whose mirrors must be withdrawn, it is one whose content is being
// reclaimed.
func TestMirrorReferenceProjectsAreTheWithdrawalInput(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, mirrorSceneRepository, "https://git.example.invalid/group/private")

	binding := mirrorReferences(t, store)
	project, found := projectOf(binding, scene.project)
	if !found {
		t.Fatalf("the live Projects do not name %s: %+v", scene.project, binding.Projects)
	}
	// The identity is stored canonical, which is what the comparison against a stored key's repository
	// needs: the key was built from the same canonicalization, so a Project and a key that name one
	// repository must compare equal rather than differing by the decoration the caller pasted.
	if project.Repository != "https://git.example.invalid/group/private" {
		t.Fatalf("the Project's identity = %q, want the canonical form", project.Repository)
	}
	if project.TenantID != scene.tenant {
		t.Fatalf("the Project names tenant %q, want %q", project.TenantID, scene.tenant)
	}

	// A Project that cannot be canonicalized is reported as not comparable rather than as pointing
	// somewhere else, which is what keeps an unusable URL from withdrawing a mirror. An empty identity
	// is the whole of that statement.
	execTest(t, store, `UPDATE projects SET repository_url='ssh://git@example.invalid/repo.git' WHERE id=$1`, scene.project)
	if changed, found := projectOf(mirrorReferences(t, store), scene.project); !found || changed.Repository != "" {
		t.Fatalf("a repository this deployment cannot address was reported as %q", changed.Repository)
	}

	execTest(t, store, `UPDATE projects SET lifecycle='deleting' WHERE id=$1`, scene.project)
	if _, found := projectOf(mirrorReferences(t, store), scene.project); found {
		t.Fatal("a Project that is being deleted is still offered as a withdrawal input")
	}
}

// projectOf finds one Project in Cloud's answer.
func projectOf(binding mirrorpublish.Binding, id string) (mirrorpublish.Project, bool) {
	for i := range binding.Projects {
		if binding.Projects[i].ID == id {
			return binding.Projects[i], true
		}
	}
	return mirrorpublish.Project{}, false
}
