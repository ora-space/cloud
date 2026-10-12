package core

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// gitReadScene is one live workspace with a running sandbox and a pinned commit, plus everything the
// binding query joins across. Every test below changes exactly one thing about it and asserts what
// the reader answers, so a refusal can only be attributed to that change.
type gitReadScene struct {
	store      *Store
	tenant     string
	project    string
	workspace  string
	container  string
	commit     string
	repository string
}

const gitReadCommit = "1234567890abcdef1234567890abcdef12345678"

func seedGitReadScene(t *testing.T) gitReadScene {
	t.Helper()
	store := dispatcherDB(t)
	scene := gitReadScene{
		store: store, tenant: uuid.NewString(), project: uuid.NewString(), workspace: uuid.NewString(),
		// The key carries the canonical upstream identity, and the stored URL is whatever the
		// caller submitted; the seed stores one form and the tests compare against the other.
		container: uuid.NewString(), commit: gitReadCommit, repository: "https://example.invalid/demo",
	}
	user, space := uuid.NewString(), uuid.NewString()
	// One transaction: the tenant's "active tenants need an active administrator" guard is a
	// deferred constraint, so the tenant and its membership have to arrive together, exactly as the
	// control plane creates them.
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'git read','active')`, user)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'git read','active')`, scene.tenant)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, scene.tenant, user)
	exec("space", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'git read',$3,$4)`, space, scene.tenant, "git-read-"+space[:8], user)
	exec("project", `INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'demo',$5,'main','active')`, scene.project, scene.tenant, user, space, scene.repository+".git")
	// A live project needs exactly one main workspace, and the scene's workspace is a separate
	// isolated one so that deleting the scene's workspace does not trip that guard.
	exec("main workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), scene.tenant, user, scene.project)
	exec("workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','running','ready',3,'HEAD')`, scene.workspace, scene.tenant, user, scene.project)
	exec("pinned commit", `UPDATE workspaces SET base_commit_id=$2 WHERE id=$1`, scene.workspace, scene.commit)
	// An isolated workspace is only valid while it carries exactly one live task.
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'git read')`, uuid.NewString(), scene.workspace)
	exec("sandbox", `INSERT INTO sandbox_instances(id,workspace_id,generation,substrate_sandbox_id,observed_state) VALUES($1,$2,3,$3,'running')`, scene.container, scene.workspace, scene.container)
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return scene
}

// origin is the identity the origin resolver would have produced for this scene's container.
func (s gitReadScene) origin() gitread.Origin {
	return gitread.Origin{WorkspaceID: s.workspace, ContainerID: s.container, ContainerName: "ora-sandbox"}
}

// key is the mirror key the scene is entitled to.
func (s gitReadScene) key() mirror.Key {
	return mirror.Key{ProjectID: s.project, Repository: s.repository, Commit: s.commit}
}

func (s gitReadScene) read(t *testing.T) gitread.Snapshot {
	t.Helper()
	snapshot, err := s.store.ReadGitMirror(context.Background(), s.origin(), s.key().Digest())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return snapshot
}

// TestReadGitMirrorAnswersTheBindingAndOnlyTheBinding is the positive control, and it also pins the
// division of labor: the reader answers the workspace, container, project, tenant and key, and
// deliberately says nothing about whether the mirror exists or is ready.
func TestReadGitMirrorAnswersTheBindingAndOnlyTheBinding(t *testing.T) {
	scene := seedGitReadScene(t)
	snapshot := scene.read(t)
	if !snapshot.Found {
		t.Fatal("a fully bound workspace produced no mirror key")
	}
	if snapshot.Key != scene.key() {
		t.Fatalf("key = %+v, want %+v", snapshot.Key, scene.key())
	}
	if !snapshot.ContainerLive {
		t.Fatal("the running sandbox instance was not reported as live")
	}
	if snapshot.WorkspaceID != scene.workspace || snapshot.ContainerID != scene.container {
		t.Fatalf("binding = %s/%s, want %s/%s", snapshot.WorkspaceID, snapshot.ContainerID, scene.workspace, scene.container)
	}
	if snapshot.WorkspaceProjectID != scene.project {
		t.Fatalf("project = %s, want %s", snapshot.WorkspaceProjectID, scene.project)
	}
	if snapshot.WorkspaceTenantID != scene.tenant || snapshot.ProjectTenantID != scene.tenant {
		t.Fatalf("tenants = %s/%s, want %s", snapshot.WorkspaceTenantID, snapshot.ProjectTenantID, scene.tenant)
	}
	if snapshot.RuntimeGeneration != 3 {
		t.Fatalf("generation = %d, want 3", snapshot.RuntimeGeneration)
	}
	// The mirror's own state belongs to the mirror store, and this reader must not invent it.
	if snapshot.MirrorReady || snapshot.MirrorRevoked || snapshot.MirrorGeneration != 0 {
		t.Fatalf("the authoritative reader answered mirror state it does not own: %+v", snapshot)
	}
	// The binding alone is not an authorization. Evaluated on its own it is refused, because
	// whether the mirror exists and is ready belongs to the mirror store; the composed reader in
	// internal/gitserver fills those two fields in, and only then can the policy allow. Asserting
	// the refusal here is what keeps a later change from quietly making this reader answer state it
	// does not own.
	verdict := (gitread.Request{Origin: scene.origin(), MirrorDigest: scene.key().Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot)
	if verdict.Allow || verdict.Reason != gitread.ReasonMirrorNotReady {
		t.Fatalf("the binding alone decided %+v; it must not be an allow", verdict)
	}
}

// TestReadGitMirrorAnswersItsOwnKeyWhateverDigestWasAsked pins the derivation: the key never comes
// from the request, so asking for someone else's mirror yields this workspace's own key, and the
// policy then refuses the mismatch rather than the reader having to know about digests at all.
func TestReadGitMirrorAnswersItsOwnKeyWhateverDigestWasAsked(t *testing.T) {
	scene := seedGitReadScene(t)
	elsewhere := mirror.Key{ProjectID: uuid.NewString(), Repository: scene.repository, Commit: scene.commit}
	snapshot, err := scene.store.ReadGitMirror(context.Background(), scene.origin(), elsewhere.Digest())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snapshot.Key != scene.key() {
		t.Fatalf("key = %+v, want the caller's own %+v", snapshot.Key, scene.key())
	}
	verdict := (gitread.Request{Origin: scene.origin(), MirrorDigest: elsewhere.Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot)
	if verdict.Allow || verdict.Reason != gitread.ReasonDigestMismatch {
		t.Fatalf("a digest for another project's mirror produced %+v", verdict)
	}
}

func TestReadGitMirrorRefusesEveryWayAWorkspaceStopsBeingReadable(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, scene gitReadScene)
		// noKey is for the cases where the workspace no longer resolves to a mirror key at all; in
		// every other case the key is still derived and the refusal has to come from liveness.
		noKey bool
	}{
		{
			name: "the sandbox was terminated",
			change: func(t *testing.T, scene gitReadScene) {
				execTest(t, scene.store, `UPDATE sandbox_instances SET observed_state='terminated', terminated_at=now() WHERE id=$1`, scene.container)
			},
		},
		{
			name: "the sandbox is still allocating",
			change: func(t *testing.T, scene gitReadScene) {
				execTest(t, scene.store, `UPDATE sandbox_instances SET observed_state='allocating' WHERE id=$1`, scene.container)
			},
		},
		{
			name: "the workspace runs under a newer generation",
			change: func(t *testing.T, scene gitReadScene) {
				// The container the resolver named still exists and still holds its address, but it
				// is no longer the instance the workspace runs: a restart moved the generation, and
				// the old instance is not the live one.
				execTest(t, scene.store, `UPDATE sandbox_instances SET observed_state='terminated', terminated_at=now() WHERE id=$1`, scene.container)
				execTest(t, scene.store, `UPDATE workspaces SET runtime_generation=4 WHERE id=$1`, scene.workspace)
				next := uuid.NewString()
				execTest(t, scene.store, `INSERT INTO sandbox_instances(id,workspace_id,generation,substrate_sandbox_id,observed_state) VALUES($1,$2,4,$3,'running')`, next, scene.workspace, next)
			},
		},
		{
			name: "the workspace was deleted",
			change: func(t *testing.T, scene gitReadScene) {
				// The workspace is gone. Its container may still be running and still resolvable;
				// that must not keep the read open.
				// The workspace goes first: the isolated-task guard is only satisfied while the
				// workspace is still live.
				execTest(t, scene.store, `UPDATE workspaces SET deleted_at=now() WHERE id=$1`, scene.workspace)
				execTest(t, scene.store, `UPDATE tasks SET deleted_at=now() WHERE workspace_id=$1`, scene.workspace)
			},
		},
		{
			name: "the project is no longer active",
			change: func(t *testing.T, scene gitReadScene) {
				execTest(t, scene.store, `UPDATE projects SET lifecycle='deleting' WHERE id=$1`, scene.project)
			},
		},
		{
			name: "the project was deleted",
			change: func(t *testing.T, scene gitReadScene) {
				execTest(t, scene.store, `UPDATE projects SET deleted_at=now() WHERE id=$1`, scene.project)
			},
		},
		{
			name: "no commit has been pinned yet",
			change: func(t *testing.T, scene gitReadScene) {
				// The clone has not completed, so there is no mirror this workspace can be entitled
				// to. Serving whatever it had last is exactly what must not happen.
				execTest(t, scene.store, `UPDATE workspaces SET base_commit_id=NULL WHERE id=$1`, scene.workspace)
			},
			noKey: true,
		},
		{
			name: "the pinned commit is replaced by a shorter one",
			change: func(t *testing.T, scene gitReadScene) {
				// The column's own CHECK constraint refuses anything that is not a full hash, so the
				// only way to reach the reader with an unusable commit is to write one the constraint
				// accepts and have the reader's own validation be the thing that refuses it — which
				// is what the empty-commit case above covers. This case asserts the constraint
				// instead: a short hash never reaches the table at all.
				if _, err := scene.store.Pool.Exec(`UPDATE workspaces SET base_commit_id='deadbeef' WHERE id=$1`, scene.workspace); err == nil {
					t.Fatal("the database accepted a commit that is not a full hash")
				}
			},
		},
		{
			name: "the project's repository is not a usable https reference",
			change: func(t *testing.T, scene gitReadScene) {
				execTest(t, scene.store, `UPDATE projects SET repository_url='git@github.com:ora-space/demo.git' WHERE id=$1`, scene.project)
			},
			noKey: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scene := seedGitReadScene(t)
			expected := !tc.noKey
			tc.change(t, scene)
			snapshot, err := scene.store.ReadGitMirror(context.Background(), scene.origin(), scene.key().Digest())
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if snapshot.Found != expected {
				t.Fatalf("Found = %v, want %v", snapshot.Found, expected)
			}
			// Whatever the reader answered, the policy must refuse.
			verdict := (gitread.Request{Origin: scene.origin(), MirrorDigest: scene.key().Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot)
			if verdict.Allow {
				t.Fatalf("a workspace that is no longer readable was authorized")
			}
		})
	}
}

func TestReadGitMirrorRefusesAContainerThatIsNotTheLiveOne(t *testing.T) {
	scene := seedGitReadScene(t)
	// A different container of the same workspace — an older generation's instance that the Engine
	// still reports, or a rebuilt container whose address Docker has already reassigned.
	other := gitread.Origin{WorkspaceID: scene.workspace, ContainerID: uuid.NewString()}
	snapshot, err := scene.store.ReadGitMirror(context.Background(), other, scene.key().Digest())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snapshot.ContainerLive {
		t.Fatal("a container that is not the live sandbox instance was reported as live")
	}
	if verdict := (gitread.Request{Origin: other, MirrorDigest: scene.key().Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot); verdict.Allow {
		t.Fatal("a container that is not the live one was authorized")
	}
}

func TestReadGitMirrorRefusesAWorkspaceThatDoesNotExist(t *testing.T) {
	scene := seedGitReadScene(t)
	for name, workspace := range map[string]string{
		"an unknown id":     uuid.NewString(),
		"a malformed id":    "not-a-uuid",
		"an empty id":       "",
		"another workspace": uuid.NewString() + "x",
	} {
		t.Run(name, func(t *testing.T) {
			origin := gitread.Origin{WorkspaceID: workspace, ContainerID: scene.container}
			snapshot, err := scene.store.ReadGitMirror(context.Background(), origin, scene.key().Digest())
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if snapshot.Found || snapshot.ContainerLive {
				t.Fatalf("an unknown workspace produced %+v", snapshot)
			}
			if verdict := (gitread.Request{Origin: origin, MirrorDigest: scene.key().Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot); verdict.Allow {
				t.Fatal("an unknown workspace was authorized")
			}
		})
	}
}

func TestReadGitMirrorRefusesAnotherWorkspacesMirror(t *testing.T) {
	// Two projects, each with its own workspace and container. The second workspace's container asks
	// for the first's mirror: the derived key is its own, so the digest does not match, and even if
	// the digests had been equal the project binding would refuse it.
	first := seedGitReadScene(t)
	second := seedGitReadScene(t)
	snapshot, err := second.store.ReadGitMirror(context.Background(), second.origin(), first.key().Digest())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snapshot.Key.ProjectID != second.project {
		t.Fatalf("cross-project read produced key %+v", snapshot.Key)
	}
	verdict := (gitread.Request{Origin: second.origin(), MirrorDigest: first.key().Digest(), Operation: gitread.OperationUploadPack}).Evaluate(snapshot)
	if verdict.Allow {
		t.Fatal("a workspace was authorized to read another project's mirror")
	}
}

func TestReadGitMirrorUsesTheCanonicalUpstreamIdentity(t *testing.T) {
	// The stored URL is whatever the caller submitted; the key must be derived from its canonical
	// form, so that the same repository submitted two ways is one mirror and not two.
	scene := seedGitReadScene(t)
	execTest(t, scene.store, `UPDATE projects SET repository_url='https://example.invalid/demo.git/' WHERE id=$1`, scene.project)
	snapshot := scene.read(t)
	if !snapshot.Found {
		t.Fatal("a trailing slash made the repository unusable")
	}
	if snapshot.Key.Repository != scene.repository {
		t.Fatalf("key repository = %q, want the canonical %q", snapshot.Key.Repository, scene.repository)
	}
	if snapshot.Key != scene.key() {
		t.Fatal("the same repository submitted two ways produced two different keys")
	}
}

func execTest(t *testing.T, store *Store, q string, args ...any) {
	t.Helper()
	if _, err := store.Pool.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
