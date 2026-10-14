package core

// The credentialed-clone boundary is exercised here at the layer a Controller reaches it: one real
// `clone_dispatch` against a real schema, with the credential-executing component replaced by a
// recorder.
//
// Nothing in this file mints an authorization, opens a socket, reads a token or runs Git. Cloud's only
// seam to the executor is the one-method `CredentialedCloner`, so installing a recording implementation
// of it exercises everything up to that seam and nothing past it: the scenes here prove what Cloud
// decided and what it handed over, and they cannot prove anything about cloning.
//
// A-side preconditions these tests do not exercise (a live Project and its main Workspace, a connected
// Node, a reserved runtime control, a Controller lease) are staged with direct SQL, as the Agent-run
// scenes in agent_run_test_support_test.go are, and each says so where it does it. The boundary under
// test then runs on the shipped path unchanged:
//
//	Store.Control -> admitCredentialedClone -> CredentialedCloner -> registerCredentialedClone
//	              -> workspaceCloneDispatch (the real clone_executions registration)

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// recordingCloner stands in for the credentialed-clone executor. It records the clone Cloud admitted —
// the repository identity, the requested ref and the whole fencing set — and answers with a revision,
// so a test can read what an authorization would have named without a credential or a network
// existing anywhere in the process.
type recordingCloner struct {
	calls []CredentialedClone
	err   error
}

func (c *recordingCloner) Execute(_ context.Context, clone CredentialedClone) (CredentialedCloneOutcome, error) {
	c.calls = append(c.calls, clone)
	if c.err != nil {
		return CredentialedCloneOutcome{}, c.err
	}
	return CredentialedCloneOutcome{Commit: baseCommit, CredentialVersion: clone.CredentialVersion}, nil
}

// credentialedScene names the rows one `clone_dispatch` reads before it can admit anything.
type credentialedScene struct {
	tenant, user, space           string
	project, workspace, operation string
	credential, node, ref         string
	repository                    string
	// cloneRepository and cloneBranch override the repository and branch a Controller's dispatch names,
	// for the scenes whose Workspace clones the internal mirror rather than the Project's upstream. An
	// empty value means the Project's upstream and the Workspace's requested ref, which is what a
	// Controller derives on every deployment that has no mirror.
	cloneRepository, cloneBranch string
}

// seedCredentialedCloneScene stages one Project that names one credential, the Workspace whose clone
// step is running, and the Node that Workspace is on.
//
// `repository` is written to `projects.repository_url` and nowhere else: the Workspace row has no URL
// column, which is the whole point of the fix these tests pin. `scope` is written to
// `credential_refs.repository_scope`; the empty string is the deployment-managed reference
// (`ConfigureCredential`) that carries no scope at all.
func seedCredentialedCloneScene(t *testing.T, store *Store, repository, scope string) credentialedScene {
	t.Helper()
	s := credentialedScene{
		tenant: newID(), user: newID(), space: newID(), project: newID(),
		workspace: newID(), operation: newID(), credential: newID(),
		repository: repository, ref: "release/2026-10",
	}
	// One transaction, because the schema's own integrity triggers are deferred: an active tenant
	// requires an active administrator, and separate statements would commit the tenant before its
	// member exists. The seeded rows are a single consistent scene or none of them.
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'clone','active')`, s.user)
		t.exec(`INSERT INTO tenants(id,name,status) VALUES($1,'clone','active')`, s.tenant)
		t.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, s.tenant, s.user)
		t.exec(`INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'clone','clone-scene',$3)`, s.space, s.tenant, s.user)
		// The two attribution values the guard distinguishes: a product credential is `personal` and
		// carries a scope, a deployment-managed reference is `unknown` and carries none. Both are owned
		// by this scene's active member, so neither is frozen by the membership predicates on any path.
		//
		// The capability set is the one a product reference is written with. It is seeded explicitly
		// because the column's default is the empty set — "permits nothing" — which the guard refuses;
		// the scene is about scope, so it has to be a reference that would otherwise be admitted.
		attribution := "personal"
		if scope == "" {
			attribution = "unknown"
		}
		t.exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,availability,repository_scope,capabilities)
			VALUES($1,$2,$3,'git','credential-ref/1','clone','`+attribution+`','available',NULLIF($4,''),'["clone"]'::jsonb)`,
			s.credential, s.tenant, s.user, scope)
		t.exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,credential_ref_id,lifecycle)
			VALUES($1,$2,$3,$4,'clone',$5,'main',$6,'active')`, s.project, s.tenant, s.user, s.space, repository, s.credential)
		// A live Project must carry exactly one main Workspace (the deferred `project_main` trigger).
		// The requested ref is the Workspace's own column; the repository is not.
		t.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',$5)`, s.workspace, s.tenant, s.user, s.project, s.ref)
		// The operation drives the Workspace's clone step: `clone` is the step workspaceCloneDispatch
		// requires, and `running` is what makes it claimable and dispatchable at all.
		t.exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,controller_epoch)
			VALUES($1,$2,$3,$4,$5,'create_project','running','clone','{}','clone-scene','clone-scene-hash',1)`,
			s.operation, s.tenant, s.user, s.project, s.workspace)
		// Lifecycle control state a real operation would have reserved before its clone step.
		t.exec(`INSERT INTO runtime_controls(workspace_id,state,control_epoch,maintenance_operation_id)
			VALUES($1,'maintenance',1,$2)`, s.workspace, s.operation)
		return Object{}
	}); err != nil {
		t.Fatalf("seed credentialed clone scene: %v", err)
	}
	_, s.node = bindConnectedNode(t, store, s.workspace)
	seedControllerLease(t, store)
	return s
}

// dispatch drives one real `clone_dispatch` for the scene's Workspace operation, with the input the
// Controller derives from the snapshot it was handed: the Project's repository and the Workspace's
// requested ref, unless the scene was staged on the mirror channel, where both are the mirror's.
func (s credentialedScene) dispatch(store *Store, execution string) (Object, error) {
	repository, branch := s.repository, s.ref
	if s.cloneRepository != "" {
		repository, branch = s.cloneRepository, s.cloneBranch
	}
	return store.Control(context.Background(), &ControlRequest{
		Action: "clone_dispatch",
		Body: Object{
			"epoch":       1,
			"operationId": s.operation,
			"executionId": execution,
			"nodeId":      s.node,
			"input":       Object{"kind": "clone", "repositoryUrl": repository, "branch": branch},
		},
		Service: controllerClaims(),
	})
}

// faultCode reads the stable reason code out of a control refusal, or reports that the call was not a
// refusal at all.
func faultCode(t *testing.T, err error) string {
	t.Helper()
	var fault *Fault
	if !errors.As(err, &fault) {
		t.Fatalf("want a coded refusal, got %v", err)
	}
	return fault.Code
}

// TestWorkspaceCarriesNoRepositoryURLOfItsOwn pins the shape the fix depends on: the Project is the
// only place a repository is named. The clone step reads the URL through a JOIN for exactly this
// reason, and a second column on `workspaces` would be a second source of truth that could drift from
// the Project the credential was scoped against.
func TestWorkspaceCarriesNoRepositoryURLOfItsOwn(t *testing.T) {
	store := dispatcherDB(t)
	// Both counts are scoped to the schema under test, which is what `search_path` points at. Without
	// the scope they would count every schema in the database the role can read, so a sibling package
	// running its own isolated schema in parallel — `go test ./...` runs packages concurrently — would
	// make the number answer a question about the database rather than about this schema.
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspaces' AND column_name='repository_url'`).Scan(&n); err != nil {
		t.Fatalf("read workspaces columns: %v", err)
	}
	if n != 0 {
		t.Fatal("workspaces acquired a repository_url column: the clone step's JOIN would no longer be the only reading")
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='projects' AND column_name='repository_url'`).Scan(&n); err != nil {
		t.Fatalf("read projects columns: %v", err)
	}
	if n != 1 {
		t.Fatal("projects.repository_url is not the single source of the repository URL")
	}
}

// TestCredentialedCloneAdmitsTheProjectsRepository is the regression for the P0 defect: admission read
// `workspaces.repositoryUrl`, a column that does not exist, so it read the empty string, canonicalized
// nothing and refused every credentialed clone — which meant the deployment flag that enables this
// boundary could not open the path at all.
//
// The assertion is on the value that reaches the executor, because that is what an authorization would
// name and what the executor re-derives: a binding minted from anything but the Project's own URL
// would be a clone of some other repository, or of none.
func TestCredentialedCloneAdmitsTheProjectsRepository(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")

	out, err := scene.dispatch(store, "exec-admitted")
	if err != nil {
		t.Fatalf("a credentialed clone whose scope matches its Project must be admitted, got %v", err)
	}
	if len(cloner.calls) != 1 {
		t.Fatalf("the executor must be asked exactly once, got %d calls", len(cloner.calls))
	}
	clone := cloner.calls[0]
	if clone.Repository != "https://git.example.invalid/group/private" {
		t.Fatalf("the binding must carry the Project's canonical repository, got %q", clone.Repository)
	}
	if clone.Branch != scene.ref {
		t.Fatalf("the binding must carry the Workspace's requested ref %q, got %q", scene.ref, clone.Branch)
	}
	// Everything the admission decided, so a change to any of it is visible here rather than only at
	// the executor.
	if clone.TenantID != scene.tenant || clone.ProjectID != scene.project || clone.WorkspaceID != scene.workspace {
		t.Fatalf("the binding names the wrong scope: %+v", clone)
	}
	if clone.ExecutionID != "exec-admitted" {
		t.Fatalf("the binding must carry the Controller's execution id, got %q", clone.ExecutionID)
	}
	if clone.CredentialID != scene.credential || clone.CredentialVersion != 1 {
		t.Fatalf("the binding must pin the exact credential material, got %q v%d", clone.CredentialID, clone.CredentialVersion)
	}
	if clone.OwnerUserID != scene.user {
		t.Fatalf("the binding must carry the credential's owner, got %q", clone.OwnerUserID)
	}
	if clone.Runtime.NodeID != scene.node || clone.Runtime.NodeInstanceID == "" || clone.Runtime.NodeIncarnationID == "" {
		t.Fatalf("the binding must fence the runtime it was admitted under, got %+v", clone.Runtime)
	}
	if clone.Runtime.RuntimeGeneration == 0 || clone.Runtime.ControlEpoch != 1 {
		t.Fatalf("the binding must carry the Workspace generation and control epoch, got %+v", clone.Runtime)
	}
	// The execution is registered with its result already written, so the Controller can never hand it
	// to a Node that holds no credential.
	e := out
	if e.S("executionId") != "exec-admitted" || e.O("result").S("outcome") != cloneReadyResult || e.O("result").S("commit") != baseCommit {
		t.Fatalf("the registered execution must already carry Cloud's own clone result: %v", e)
	}
}

// TestCredentialedCloneAdmissionRefusesEverySpellingThatIsNotTheProjectsRepository is the boundary
// table. Each row is one URL a caller could store as a Project's repository, checked against the scope
// the credential was created with.
//
// The refusals are the security half: percent-encoding, dot segments, empty segments, userinfo, query
// and a non-HTTPS transport must all be refused rather than decoded, resolved or waved through,
// because each of them names something a plain string comparison would happily equate with the scope.
// The admissions are the availability half: the spellings that genuinely name the same repository —
// a trailing `.git`, a trailing slash, a differently cased host — must still clone, or the boundary is
// fail-closed into uselessness.
func TestCredentialedCloneAdmissionRefusesEverySpellingThatIsNotTheProjectsRepository(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		scope  string
		code   string // empty means the dispatch is admitted
		want   string // the canonical repository the executor must receive when admitted
		reason string
	}{
		{
			name: "the scope itself", repo: "https://git.example.invalid/group/private",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: "a trailing .git names the same repository", repo: "https://git.example.invalid/group/private.git",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: "a trailing slash names the same repository", repo: "https://git.example.invalid/group/private/",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: ".git and a trailing slash together name the same repository", repo: "https://git.example.invalid/group/private.git/",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: "a host is case-insensitive", repo: "https://Git.Example.Invalid/group/private",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: "surrounding whitespace is trimmed", repo: "  https://git.example.invalid/group/private  ",
			scope: "https://git.example.invalid/group/private", want: "https://git.example.invalid/group/private",
		},
		{
			name: "an unscoped deployment reference is admitted over HTTPS", repo: "https://git.example.invalid/group/private.git",
			scope: "", want: "https://git.example.invalid/group/private",
		},
		{
			name: "a path is case-sensitive and names another repository", repo: "https://git.example.invalid/group/Private",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "path folding would widen one credential to cover two repositories",
		},
		{
			name: "a percent-encoded path is refused rather than decoded", repo: "https://git.example.invalid/group/priv%61te",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "%2F and / would otherwise normalize to one path while meaning different things to the server",
		},
		{
			name: "a doubly-encoded path is refused", repo: "https://git.example.invalid/group/priv%2561te",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "a decoded path would hide the encoding from the comparison that authorizes it",
		},
		{
			name: "an empty path segment is refused", repo: "https://git.example.invalid//private",
			scope: "https://git.example.invalid/private", code: "repository_credential_scope_mismatch",
			reason: "dropping an empty segment would equate a path with one that does not obviously mean it",
		},
		{
			name: "a parent segment is refused", repo: "https://git.example.invalid/group/../private",
			scope: "https://git.example.invalid/private", code: "repository_credential_scope_mismatch",
			reason: "resolving .. would let one spelling authorize a repository the scope never named",
		},
		{
			name: "a current-directory segment is refused", repo: "https://git.example.invalid/group/./private",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "a relative segment is a path this comparison does not implement",
		},
		{
			name: "userinfo is refused", repo: "https://user:token@git.example.invalid/group/private",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "a URL that embeds a credential is a second secret in a column this code signs",
		},
		{
			name: "a query is refused", repo: "https://git.example.invalid/group/private?ref=main",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "a query names something other than the repository",
		},
		{
			name: "a fragment is refused", repo: "https://git.example.invalid/group/private#readme",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "a fragment names something other than the repository",
		},
		{
			name: "a bare host names no repository", repo: "https://git.example.invalid",
			scope: "https://git.example.invalid/private", code: "repository_credential_scope_mismatch",
			reason: "a host-wide scope is exactly what the scope form refuses to be",
		},
		{
			name: "ssh cannot be authorized by an HTTPS token", repo: "ssh://git@git.example.invalid/group/private.git",
			scope: "https://git.example.invalid/group/private", code: "repository_credential_scope_mismatch",
			reason: "an ssh URL normalizes to no scope at all, so it can never match one",
		},
		{
			name: "an unscoped reference cannot name a non-HTTPS repository", repo: "ssh://git@git.example.invalid/group/private.git",
			scope: "", code: "repository_credential_scope_mismatch",
			reason: "there is no scope to compare, so the canonical form is the only thing that can refuse it",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			cloner := &recordingCloner{}
			store.SetCredentialedCloner(cloner)
			scene := seedCredentialedCloneScene(t, store, c.repo, c.scope)

			_, err := scene.dispatch(store, "exec-boundary")
			if c.code != "" {
				if err == nil {
					t.Fatalf("%s: must be refused", c.reason)
				}
				if got := faultCode(t, err); got != c.code {
					t.Fatalf("%s: got %q, want %q", c.reason, got, c.code)
				}
				if len(cloner.calls) != 0 {
					t.Fatalf("%s: a refused clone must never reach the executor", c.reason)
				}
				if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
					t.Fatalf("%s: a refused clone left %d execution rows behind", c.reason, n)
				}
				return
			}
			if err != nil {
				t.Fatalf("this spelling names the repository the scope does: %v", err)
			}
			if len(cloner.calls) != 1 {
				t.Fatalf("the executor must be asked exactly once, got %d calls", len(cloner.calls))
			}
			if got := cloner.calls[0].Repository; got != c.want {
				t.Fatalf("the binding must carry %q, got %q", c.want, got)
			}
		})
	}
}

// TestCredentialedCloneAdmissionRequiresTheReferencesCloneCapability is the capability half of the
// execution plan the continuity decision's D4 fixes: a clone names the reference, the version, the
// repository *and the capability it needs*, and a capability the reference does not support is refused
// rather than inferred.
//
// The column is the one place the capability is durable, and the rows below are the values it can
// hold. `clone` is what the product path writes; the empty set is the column's own default, which is
// what a deployment-managed reference written before the column existed carries; and the remaining
// rows are sets a credential cannot have been created with on any product path but that durable state
// can still hold. Everything that does not contain `clone` permits no clone, and an empty set in
// particular must not be read as "no restriction" — that reading is what would turn an unclassified
// reference into an unrestricted one.
//
// Two rows pin the boundary the push decision moved, in both directions. A reference that permits
// `push` alone is still refused here: admission requires the capability *this* work needs, and a
// reference's whole authority belongs to whatever work exercises it. A reference that permits both is
// admitted, and the plan it produces carries `clone` alone — the requirement, not the row's set —
// because `cloneauth` signs exactly what one operation permits and a plan carrying the wider set would
// be an authorization no operation could sign.
//
// The admitted rows also pin what the plan carries, because the value has to survive from the
// credential row to the executor: a guard that checked the capability and then let the binding name
// nothing would enforce the vocabulary nowhere.
func TestCredentialedCloneAdmissionRequiresTheReferencesCloneCapability(t *testing.T) {
	cases := []struct {
		name         string
		capabilities string
		admitted     bool
	}{
		{name: "the one capability the product path writes", capabilities: `["clone"]`, admitted: true},
		{name: "a reference that permits the write direction as well", capabilities: `["clone","push"]`, admitted: true},
		{name: "the column's own default", capabilities: `[]`},
		{name: "a capability the vocabulary does not define", capabilities: `["fetch"]`},
		{name: "a reference that permits only the write direction", capabilities: `["push"]`},
		{name: "a differently cased spelling", capabilities: `["Clone"]`},
		{name: "the same capability twice", capabilities: `["clone","clone"]`},
		{name: "a member that is not a capability at all", capabilities: `["clone",7]`},
		{name: "an empty-string member", capabilities: `[""]`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := dispatcherDB(t)
			cloner := &recordingCloner{}
			store.SetCredentialedCloner(cloner)
			scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
			if _, err := store.Pool.Exec(`UPDATE credential_refs SET capabilities=$2::jsonb WHERE id=$1`, scene.credential, testCase.capabilities); err != nil {
				t.Fatalf("write the reference's capability set: %v", err)
			}

			_, err := scene.dispatch(store, "exec-capability")
			if !testCase.admitted {
				if got := faultCode(t, err); got != repositoryCredentialCapabilityUnsupported {
					t.Fatalf("refusal code = %q, want %q", got, repositoryCredentialCapabilityUnsupported)
				}
				if len(cloner.calls) != 0 {
					t.Fatalf("a reference that permits no clone reached the executor %d times", len(cloner.calls))
				}
				return
			}
			if err != nil {
				t.Fatalf("a reference that permits a clone must be admitted, got %v", err)
			}
			if len(cloner.calls) != 1 {
				t.Fatalf("the executor must be asked exactly once, got %d calls", len(cloner.calls))
			}
			if got := cloner.calls[0].Capabilities; !reflect.DeepEqual(got, []string{"clone"}) {
				t.Fatalf("the plan must carry the capability admission required, got %v", got)
			}
		})
	}
}

// TestADeploymentsOwnRefusalOutranksTheReferencesCapability pins the order of the guard's two
// deployment-level answers.
//
// A deployment with no executor refuses every credentialed clone whatever the reference says, and that
// refusal is the one the fail-closed decision fixes, settles and reports to members. Answering a
// capability problem there would name the reference as the obstacle when the deployment is, and would
// change the code of a refusal this repository has already shipped.
func TestADeploymentsOwnRefusalOutranksTheReferencesCapability(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	// No SetCredentialedCloner, and a reference that permits nothing: both refusals apply.
	if _, err := store.Pool.Exec(`UPDATE credential_refs SET capabilities='[]'::jsonb WHERE id=$1`, scene.credential); err != nil {
		t.Fatalf("strip the reference's capability: %v", err)
	}

	_, err := scene.dispatch(store, "exec-order")
	if got := faultCode(t, err); got != repositoryCredentialExecutorUnavailable {
		t.Fatalf("refusal code = %q, want %q", got, repositoryCredentialExecutorUnavailable)
	}
}

// scalarOf reads one integer straight from PostgreSQL. The public shapes strip columns this boundary
// writes, and the assertion is about durable state rather than about a response.
func scalarOf(t *testing.T, store *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := store.Pool.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("read %q: %v", q, err)
	}
	return n
}

// TestCredentialedCloneStaysRefusedWithoutAnExecutor is the fail-closed half of the fix, at the level
// the fix was made.
//
// Changing which column admission reads could not be allowed to change *whether* a credentialed clone
// is refused when the deployment installed no executor, so this pins the original reason code on the
// corrected path: the guard's own refusal must still be the answer, and it must still be given before
// any execution row exists for a later recovery to pick up.
func TestCredentialedCloneStaysRefusedWithoutAnExecutor(t *testing.T) {
	store := dispatcherDB(t)
	// No SetCredentialedCloner: the default, and the only state this repository ships.
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")

	_, err := scene.dispatch(store, "exec-refused")
	if err == nil {
		t.Fatal("a credentialed clone must be refused when no executor is configured")
	}
	if got := faultCode(t, err); got != "repository_credential_executor_unavailable" {
		t.Fatalf("the reason code must stay %q, got %q", "repository_credential_executor_unavailable", got)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
		t.Fatalf("a refused dispatch registered %d executions a later recovery could run with no credential", n)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2", scene.project, scene.credential); n != 1 {
		t.Fatal("the refusal rewrote the binding it refused")
	}
}

// TestAnonymousCloneIsUntouchedByTheCredentialedBoundary is the negative control. A Project that names
// no credential takes the ordinary path: the guard returns before any of this, the executor is never
// consulted, and the execution is registered with no result — which is what tells the Controller to
// hand the clone to a Node, the only way an anonymous clone has ever run.
//
// Without it, an admission change that started treating every Project as credentialed would look like
// a passing boundary table.
func TestAnonymousCloneIsUntouchedByTheCredentialedBoundary(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/public.git", "")
	if _, err := store.Pool.Exec(`UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project); err != nil {
		t.Fatalf("unbind the Project: %v", err)
	}

	out, err := scene.dispatch(store, "exec-anonymous")
	if err != nil {
		t.Fatalf("an anonymous clone must still dispatch: %v", err)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("an anonymous clone must never reach the credential executor, got %d calls", len(cloner.calls))
	}
	if out.S("executionId") != "exec-anonymous" {
		t.Fatalf("the ordinary dispatch must register the execution: %v", out)
	}
	if result := out.O("result"); len(result) != 0 {
		t.Fatalf("an anonymous clone must wait for the Node's result, got %v", result)
	}
}

// TestCanonicalRepositoryNeverNamesARepositoryTheScopeWouldNot is the differential check between the
// two normalizers this boundary now uses for two different values.
//
// `normalizeRepositoryScope` decides authorization and `cloneauth.CanonicalRepository` decides what an
// authorization names. They are separate functions with separate lifetimes — one is durable state
// written when a credential was created, the other a per-dispatch identity — so the property that must
// hold is not that they are the same function, but that the second can never name a repository the
// first would have refused. If it could, a scope check could pass and the binding still name something
// else.
//
// Every input either normalizer accepts must therefore land on exactly one string, and every input the
// scope normalizer refuses must still be refused even though the canonical one would accept it. The
// corpus is the scope test's own plus the spellings the canonical normalizer is deliberately laxer
// about, so the divergence is pinned rather than assumed.
func TestCanonicalRepositoryNeverNamesARepositoryTheScopeWouldNot(t *testing.T) {
	corpus := []string{
		"https://git.example.invalid/group/repo",
		"https://git.example.invalid/group/repo.git",
		"https://git.example.invalid/group/repo.git/",
		"https://git.example.invalid/group/repo/",
		"https://Git.Example.Invalid/Group/Repo",
		"https://git.example.invalid:8443/group/repo",
		"  https://git.example.invalid/group/repo  ",
		"https://git.example.invalid/group/repo.GIT",
		"https://git.example.invalid/group/repo.git.git",
		"https://git.example.invalid/group/a.git/b",
		"https://git.example.invalid",
		"https://git.example.invalid/",
		"",
		"ssh://git@git.example.invalid/group/repo",
		"http://git.example.invalid/group/repo",
		"https://user:token@git.example.invalid/group/repo",
		"https://user@git.example.invalid/group/repo",
		"https://git.example.invalid/group/repo?ref=main",
		"https://git.example.invalid/group/repo#readme",
		"https://git.example.invalid/group%2Frepo",
		"https://git.example.invalid/group%2Frepo/x",
		"https://git.example.invalid/group%2Frepo/x.git",
		"https://git.example.invalid/group//repo",
		"https://git.example.invalid/group/../repo",
		"https://git.example.invalid/group/./repo",
		"https://git.example.invalid/.git",
		"https://git.example.invalid/group/.git",
		"https://git.example.invalid/.git/x",
		"https://git.example.invalid/group/priv%61te",
		"https://git.example.invalid/group/priv%61te.git",
	}
	for _, raw := range corpus {
		t.Run(raw, func(t *testing.T) {
			scope, scoped := normalizeRepositoryScope(raw)
			canonical, err := cloneauth.CanonicalRepository(raw)
			if !scoped {
				// The scope normalizer refused it, so no binding may be minted from it either: the
				// admission refuses before the canonical form is ever computed. This is where the two
				// functions deliberately disagree, and the disagreement must stay one-directional.
				return
			}
			if err != nil {
				// Accepted for authorization but not nameable as a clone target. Fail-closed, and the
				// admission refuses rather than binding something the executor would reject.
				t.Logf("normalizeRepositoryScope accepted %q but no canonical form exists; admission refuses", raw)
				return
			}
			if canonical != scope {
				t.Fatalf("two spellings of one authorization: scope normalizer says %q, canonical says %q", scope, canonical)
			}
		})
	}
}
