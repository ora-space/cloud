package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// seedSpaceAgent inserts a space_plugins pin plus a space_agents roster row for the given
// tenant/space, bypassing the marketplace install lifecycle so this file can focus solely on target
// resolution + project gate. pluginID is the canonical namespace/identifier the agent's plugin_id
// column uses.
//
// The pin carries selected_release as well as desired_version, because that column is what the
// control plane's plugin snapshot reads when the run's Workspace has to converge the Agent's plugin:
// pluginPayload builds the Node's install item — the id and the version the evidence is checked
// against — from selected_release, which is exactly what a synced marketplace leaves there. A row
// without it names no release, so the item would carry an empty version and Cloud would refuse the
// Node's own success report.
func (f *fixture) seedSpaceAgent(t *testing.T, sid, tid, agentID, pluginID, displayName, status, version string) {
	t.Helper()
	ns, id, ok := strings.Cut(pluginID, "/")
	if !ok {
		t.Fatalf("seedSpaceAgent: pluginID %q is not namespace/identifier", pluginID)
	}
	release := mustJSON(t, core.Object{"id": pluginID, "version": version})
	if _, err := f.store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,selected_release)
		VALUES($1,$2,$3,$4,'installed',$5,'installed',$6)`, sid, tid, ns, id, version, release); err != nil {
		t.Fatalf("seed space_plugins: %v", err)
	}
	if _, err := f.store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) VALUES($1,$2,$3,$4,$5,$6)`,
		agentID, sid, tid, pluginID, displayName, status); err != nil {
		t.Fatalf("seed space_agents: %v", err)
	}
}

// projectID returns the id of the project most recently created in the tenant's default space. The
// integration harness's project POST returns a workspace-focused 202 body, so the project UUID itself
// is read back from PostgreSQL (the only authoritative source) rather than guessed from a response.
func (f *fixture) projectID(t *testing.T, sid string) string {
	t.Helper()
	var id string
	must(t, f.store.Pool.QueryRow(`SELECT id::text FROM projects WHERE space_id=$1 AND tenant_id=$2 AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1`, sid, f.tid).Scan(&id))
	if id == "" {
		t.Fatalf("no project in space %s", sid)
	}
	return id
}

func (f *fixture) runCount(t *testing.T, iid string) int {
	t.Helper()
	var n int
	must(t, f.store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND deleted_at IS NULL`, iid, f.tid).Scan(&n))
	return n
}

func (f *fixture) commentCount(t *testing.T, iid string) int {
	t.Helper()
	var n int
	must(t, f.store.Pool.QueryRow(`SELECT count(*) FROM issue_comments WHERE issue_id=$1 AND tenant_id=$2 AND deleted_at IS NULL`, iid, f.tid).Scan(&n))
	return n
}

func (f *fixture) interactionCount(t *testing.T, iid string) int {
	t.Helper()
	var n int
	must(t, f.store.Pool.QueryRow(`SELECT count(*) FROM issue_interactions WHERE issue_id=$1 AND tenant_id=$2`, iid, f.tid).Scan(&n))
	return n
}

// issueForAgent creates an issue in the tenant's default space (optionally bound to a project_ref) and
// returns its id. `keyPrefix` disambiguates idempotency keys across calls in one test.
func (f *fixture) issueForAgent(t *testing.T, title, projectRef, keyPrefix string) string {
	t.Helper()
	body := core.Object{"title": title}
	if projectRef != "" {
		body["projectRef"] = projectRef
	}
	iss := f.call("POST", f.path("/issues"), body, keyPrefix+"-issue", 200).O("resource")
	return iss.S("id")
}

// agentComment posts a Task Mode @ agent comment targeting agentID expecting success (200). `key`
// disambiguates the idempotency key. Rejection paths use agentCommentT instead.
func (f *fixture) agentComment(t *testing.T, iid, agentID, key string) core.Object {
	t.Helper()
	body := core.Object{"body": "please do this", "targets": []any{core.Object{"type": "agent", "id": agentID, "task": "implement the thing"}}}
	o, status, e := f.client.Call(context.Background(), "POST", f.path("/issues/"+iid+"/comments"),
		"gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, key, body)
	must(t, e)
	if status != 200 {
		t.Fatalf("agent comment %s: want 200 got %d %v", key, status, o)
	}
	return o
}

// agentCommentT posts the same Task Mode @ agent comment but returns the raw status + body so a test
// can assert the exact rejection (404 target_not_found, 409 issue_project_required) that a real agent
// run path must produce.
func (f *fixture) agentCommentT(t *testing.T, iid, agentID, key string) (int, core.Object) {
	t.Helper()
	body := core.Object{"body": "please do this", "targets": []any{core.Object{"type": "agent", "id": agentID, "task": "implement the thing"}}}
	o, status, e := f.client.Call(context.Background(), "POST", f.path("/issues/"+iid+"/comments"),
		"gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, key, body)
	must(t, e)
	return status, o
}

// singleRun reads the one live run of an issue back through the API so tests assert the persisted,
// stripped resource (input snapshot included).
func (f *fixture) singleRun(t *testing.T, iid string) core.Object {
	t.Helper()
	runs := issueItems(f.call("GET", f.path("/issues/"+iid+"/runs"), nil, "", 200))
	if len(runs) != 1 {
		t.Fatalf("want exactly one run, got %d: %v", len(runs), runs)
	}
	return runs[0]
}

// TestAgentTargetResolvesAndSnapshots covers IssueRun D1/D2 for an active agent on a valid project, plus the D1 snapshot: a Task Mode
// @ of an ACTIVE space_agents agent on an issue with a valid project resolves, creates a run whose
// executor_id IS the space_agents.id, and pins the plugin identity/version into the run input.
func TestAgentTargetResolvesAndSnapshots(t *testing.T) {
	f := setup(t)
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "project-one")
	pid := f.projectID(t, sid)
	agentID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	f.seedSpaceAgent(t, sid, f.tid, agentID, "official/hello-world", "Hello World", "active", "1.0.0")

	iid := f.issueForAgent(t, "Needs agent", pid, "a")
	status, body := f.agentCommentT(t, iid, agentID, "a-1")
	if status != 200 {
		t.Fatalf("active agent comment status = %d: %v", status, body)
	}
	f.drain()

	run := f.singleRun(t, iid)
	if run.S("executorType") != "agent" || run.S("executorId") != agentID {
		t.Fatalf("run must reference the space_agents.id as executor: %v", run)
	}
	input := run.O("input")
	if input.S("agentPluginId") != "official/hello-world" || input.S("agentPluginVersion") != "1.0.0" {
		t.Fatalf("run input must snapshot the agent plugin identity/version: %v", input)
	}
	if input.S("task") != "implement the thing" || input.O("target").S("id") != agentID {
		t.Fatalf("run input must keep the task + target projection: %v", input)
	}
}

// TestAgentTargetMissingProjectAtomicRollback covers the IssueRun D2 project gate when the issue has no project, and its atomic rollback: a valid ACTIVE agent on
// an issue WITHOUT a project_ref is rejected with 409 issue_project_required AND the comment, activity,
// interaction and run all roll back together — nothing survives the aborted transaction.
func TestAgentTargetMissingProjectAtomicRollback(t *testing.T) {
	f := setup(t)
	sid := f.defaultSpaceID()
	agentID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	f.seedSpaceAgent(t, sid, f.tid, agentID, "official/hello-world", "Hello World", "active", "1.0.0")

	iid := f.issueForAgent(t, "No project", "", "n")
	status, body := f.agentCommentT(t, iid, agentID, "n-1")
	if status != 409 || body.S("code") != "issue_project_required" {
		t.Fatalf("missing-project agent comment must be 409 issue_project_required, got %d %v", status, body)
	}
	if f.commentCount(t, iid) != 0 || f.runCount(t, iid) != 0 || f.interactionCount(t, iid) != 0 {
		t.Fatalf("reject must be atomic: comment=%d run=%d interaction=%d",
			f.commentCount(t, iid), f.runCount(t, iid), f.interactionCount(t, iid))
	}
	if n := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1 AND tenant_id=$2", iid, f.tid); n != 0 {
		t.Fatalf("reject must leave no timeline activity: %d", n)
	}
}

// TestAgentTargetNonexistentProjectRejected covers the IssueRun D2 project gate for a project that does not exist: a project_ref that does not
// exist (even though syntactically a UUID) cannot host an agent run and is rejected the same atomic way.
func TestAgentTargetNonexistentProjectRejected(t *testing.T) {
	f := setup(t)
	sid := f.defaultSpaceID()
	agentID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	f.seedSpaceAgent(t, sid, f.tid, agentID, "official/hello-world", "Hello World", "active", "1.0.0")

	ghost := "00000000-0000-4000-8000-000000000001"
	iid := f.issueForAgent(t, "Ghost project", ghost, "g")
	status, body := f.agentCommentT(t, iid, agentID, "g-1")
	if status != 409 || body.S("code") != "issue_project_required" {
		t.Fatalf("nonexistent-project agent comment must be 409 issue_project_required, got %d %v", status, body)
	}
	if f.commentCount(t, iid) != 0 || f.runCount(t, iid) != 0 || f.interactionCount(t, iid) != 0 {
		t.Fatalf("nonexistent-project reject must be atomic: comment=%d run=%d interaction=%d",
			f.commentCount(t, iid), f.runCount(t, iid), f.interactionCount(t, iid))
	}
}

// TestAgentTargetRetiredNotResolvable covers IssueRun D1 for a retired agent: only ACTIVE space_agents resolve. A retired
// agent is not found (404 target_not_found) even with a perfectly valid project, and rolls back.
func TestAgentTargetRetiredNotResolvable(t *testing.T) {
	f := setup(t)
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "project-ret")
	agentID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	f.seedSpaceAgent(t, sid, f.tid, agentID, "official/retired-tool", "Old Tool", "retired", "1.0.0")

	iid := f.issueForAgent(t, "Retired target", f.projectID(t, sid), "r")
	status, body := f.agentCommentT(t, iid, agentID, "r-1")
	if status != 404 || body.S("code") != "target_not_found" {
		t.Fatalf("retired agent must be 404 target_not_found, got %d %v", status, body)
	}
	if f.commentCount(t, iid) != 0 || f.runCount(t, iid) != 0 {
		t.Fatalf("retired-agent reject must be atomic: comment=%d run=%d", f.commentCount(t, iid), f.runCount(t, iid))
	}
}

// TestAgentTargetForeignTenantIsolation covers IssueRun D1 across tenants (and, via one-space-per-tenant, wrong
// space): an agent that lives in another tenant's space can never be resolved from this tenant, so the
// run is rejected 404 and nothing leaks.
func TestAgentTargetForeignTenantIsolation(t *testing.T) {
	f := setup(t)
	other := f.createPluginTenant("Other", "other-space", "agent-other")
	otherSID := other.S("id")
	var otherTID string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id FROM collab_workspaces WHERE id=$1`, otherSID).Scan(&otherTID))
	foreignAgent := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	f.seedSpaceAgent(t, otherSID, otherTID, foreignAgent, "official/foreign", "Foreign", "active", "1.0.0")

	f.spaceProject(t, f.defaultSpaceID(), "project-local")
	iid := f.issueForAgent(t, "Foreign target", f.projectID(t, f.defaultSpaceID()), "f")
	status, body := f.agentCommentT(t, iid, foreignAgent, "f-1")
	if status != 404 || body.S("code") != "target_not_found" {
		t.Fatalf("foreign agent must be 404 target_not_found from another tenant, got %d %v", status, body)
	}
	if f.commentCount(t, iid) != 0 || f.runCount(t, iid) != 0 {
		t.Fatalf("foreign-agent reject must be atomic: comment=%d run=%d", f.commentCount(t, iid), f.runCount(t, iid))
	}
}

// TestAgentRunSnapshotPinsPluginVersion covers the IssueRun D1 snapshot: the run input snapshot fixes the plugin version at
// create time, so upgrading the same plugin after the run (1.0.0 -> 2.0.0) never rewrites the
// historical run's input; a NEW run pins the upgraded version.
func TestAgentRunSnapshotPinsPluginVersion(t *testing.T) {
	f := setup(t)
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "project-snap")
	pid := f.projectID(t, sid)
	agentID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	f.seedSpaceAgent(t, sid, f.tid, agentID, "official/hello-world", "Hello World", "active", "1.0.0")

	iid := f.issueForAgent(t, "Pinned", pid, "p")
	f.agentComment(t, iid, agentID, "p-1")
	f.drain()
	if got := f.singleRun(t, iid).O("input").S("agentPluginVersion"); got != "1.0.0" {
		t.Fatalf("run pinned on create = %q, want 1.0.0", got)
	}

	// Simulate a later upgrade of the SAME agent plugin (a business transition, allowed) to 2.0.0.
	// The pin a new run reads is the pin the roster names, `space_plugins.desired_version`, and
	// `selected_release` is moved with it so the plugin step still knows which release to name: the
	// roster row itself has no version of its own to advance.
	if _, err := f.store.Pool.Exec(`UPDATE space_plugins SET desired_version='2.0.0',selected_release=jsonb_set(selected_release,'{version}','"2.0.0"'),version=version+1 WHERE space_id=$1 AND source_namespace='official' AND identifier='hello-world'`, sid); err != nil {
		t.Fatalf("upgrade plugin: %v", err)
	}

	// A NEW run pin on the upgraded plugin; the historical run keeps its create-time snapshot.
	iid2 := f.issueForAgent(t, "Pinned 2", pid, "p2")
	f.agentComment(t, iid2, agentID, "p2-1")
	f.drain()
	if got := f.singleRun(t, iid2).O("input").S("agentPluginVersion"); got != "2.0.0" {
		t.Fatalf("second run must pin the upgraded version, got %q", got)
	}
	if got := f.singleRun(t, iid).O("input").S("agentPluginVersion"); got != "1.0.0" {
		t.Fatalf("historical run must stay pinned at create-time version, got %q", got)
	}
	// The fixture agent Task path (non-space_agents) is separately held green by
	// TestAgentTaskRunsThroughMockExecution: it still needs no project and completes.
}
