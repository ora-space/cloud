package integration

// Resuming a new IssueRun from its Issue's latest Revision (specs
// decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md, contract decision
// controller-integration/20261010-revision-restore-contract.md). Every case drives the production
// paths: session start through StartQueuedAgentSessionsOnce, dispatch through clone_claim and
// clone_dispatch, session end and delivery through TakeOverNodeEvent, grants through gRPC.

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

const (
	// The base commit a follow-up run's Workspace was cloned at: the remote moved on since the first
	// run, which is what makes "the bundle stays relative to this run's own base" observable.
	resumedBaseCommit = "3333333333333333333333333333333333333333"
	// A commit the resumed Agent made on top of the prior Revision.
	resumedFinalCommit = "4444444444444444444444444444444444444444"
)

// savedRevision is a run whose delivery registered a Revision with its own bundle.
type savedRevision struct {
	scene      liveThreadScene
	revisionID string
	bundleKey  string
}

// savedRun drives one run through session, end and a verified delivery that changed the checkout,
// and returns its Revision.
func savedRun(t *testing.T, f *fixture) savedRevision {
	t.Helper()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	if _, e := f.deliveryTerminal(scene, execution, "", 1, deliveredResult(scene, in, revisionFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("the first run's delivery must register a Revision: %v", e)
	}
	row := f.revisionRow(scene.runID)
	if row == nil || row.BundleKey == "" {
		t.Fatal("the first run must store a bundle of its own")
	}
	return savedRevision{scene: scene, revisionID: row.ID, bundleKey: row.BundleKey}
}

// seedFollowUpRun seeds a second run of the prior scene's Issue in exactly the state
// seedStartingAgentRun leaves a first one: `starting`, with its own ready run Workspace cloned at
// resumedBaseCommit, a maintenance binding and a connected Node. With otherProject the Workspace
// belongs to a different project of the same tenant, as after the Issue was moved.
func seedFollowUpRun(t *testing.T, f *fixture, prior liveThreadScene, otherProject bool) liveThreadScene {
	t.Helper()
	var agent, project, user, input string
	must(t, f.store.Pool.QueryRow(`
		SELECT r.executor_id::text, w.project_id::text, w.owner_user_id::text, r.input::text
		FROM issue_runs r JOIN workspaces w ON w.issue_run_id = r.id WHERE r.id=$1`, prior.runID).Scan(&agent, &project, &user, &input))
	runID, workspaceID, nodeID, sandboxID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	operationID, cloneExecution := uuid.NewString(), "exec-baseline-"+workspaceID[:8]
	tx, e := f.store.Pool.Begin()
	must(t, e)
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	repository := "https://example.invalid/takeover.git"
	if otherProject {
		moved := uuid.NewString()
		repository = "https://example.invalid/moved.git"
		exec("project", `INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'moved',$5,'main','active')`, moved, prior.tenantID, user, prior.spaceID, repository)
		exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), prior.tenantID, user, moved)
		exec("issue", `UPDATE issues SET project_ref=$2 WHERE id=$1`, prior.issueID, moved)
		project = moved
	}
	exec("run", `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,phase,input) VALUES($1,$2,$3,'agent',$4,'dispatched','starting',$5)`, runID, prior.tenantID, prior.issueID, agent, input)
	exec("run-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,admission_open,runtime_generation,requested_ref,issue_run_id) VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','running','ready',true,1,'HEAD',$5)`, workspaceID, prior.tenantID, user, project, runID)
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'follow-up task')`, uuid.NewString(), workspaceID)
	exec("run-workspace-bind", `UPDATE issue_runs SET workspace_id=$2, version=version+1, updated_at=now() WHERE id=$1`, runID, workspaceID)
	exec("run-workspace-operation", `INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,controller_epoch) VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}',$6,'baseline-hash',1)`,
		operationID, prior.tenantID, user, project, workspaceID, "run-workspace-"+workspaceID[:8])
	exec("run-workspace-clone", `INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,node_operation_id,input,result,dispatched_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,1)`,
		cloneExecution, operationID, workspaceID, nodeID, cloneExecution,
		mustJSON(t, core.Object{"kind": "clone", "repositoryUrl": repository, "branch": "main"}),
		mustJSON(t, core.Object{"node": core.Object{"nodeId": nodeID, "nodeIncarnationId": "inc-" + nodeID[:8]}, "outcome": "clone_ready", "path": "/work", "commit": resumedBaseCommit}))
	exec("run-workspace-baseline", `UPDATE workspaces SET base_commit_id=$2, version=version+1 WHERE id=$1`, workspaceID, resumedBaseCommit)
	exec("run-runtime-control", `INSERT INTO runtime_controls(workspace_id,state,maintenance_run_id,control_epoch,binding_confirmed,input_closed) VALUES($1,'maintenance',$2,2,false,false)`, workspaceID, runID)
	exec("sandbox", `INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,1,'running')`, sandboxID, workspaceID)
	exec("node", `INSERT INTO node_instances(id,workspace_id,sandbox_instance_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id) VALUES($1,$2,$3,'node','connected',1,true,$4,$5)`, nodeID, workspaceID, sandboxID, nodeID, "inc-"+nodeID[:8])
	must(t, tx.Commit())
	return liveThreadScene{
		threadScene: threadScene{tenantID: prior.tenantID, issueID: prior.issueID, runID: runID},
		spaceID:     prior.spaceID, nodeID: nodeID,
	}
}

// sessionInput reads the frozen AgentSession spec a registered session execution carries.
func (f *fixture) sessionInput(execution string) core.Object {
	f.t.Helper()
	return f.deliveryInputOf(execution)
}

// firstPromptContext parses the Context section of a run's seq=1 first prompt.
func (f *fixture) firstPromptContext(runID string) core.Object {
	f.t.Helper()
	var raw string
	must(f.t, f.store.Pool.QueryRow(`SELECT record->>'content' FROM thread_entries WHERE run_id=$1 AND seq=1`, runID).Scan(&raw))
	_, context, found := strings.Cut(raw, "\n\nContext\n")
	if !found {
		f.t.Fatalf("the first prompt has no Context section: %q", raw)
	}
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(context), &out))
	return out
}

// resumeColumns reads the resume-specific columns of a run's Revision.
func (f *fixture) resumeColumns(runID string) (prior, holder, refused string) {
	f.t.Helper()
	var p, h, r sql.NullString
	must(f.t, f.store.Pool.QueryRow(`SELECT prior_revision_id::text, bundle_revision_id::text, resume_refused_reason FROM revisions WHERE run_id=$1`, runID).Scan(&p, &h, &r))
	return p.String, h.String, r.String
}

func (f *fixture) runResumeRevision(runID string) string {
	f.t.Helper()
	var id sql.NullString
	must(f.t, f.store.Pool.QueryRow(`SELECT resume_revision_id::text FROM issue_runs WHERE id=$1`, runID).Scan(&id))
	return id.String
}

func (f *fixture) grantRevisionDownload(execution string) (*controlpb.GrantRevisionDownloadResponse, error) {
	return controlpb.NewAgentRunServiceClient(f.controlConn).GrantRevisionDownload(asController("ctrl-a"),
		&controlpb.GrantRevisionDownloadRequest{Epoch: 1, ExecutionId: execution})
}

// resumedUnchangedResult is what a Node reports when a resumed run ends on its prior Revision's final
// commit: no bundle, only the history (contract decision D4).
func resumedUnchangedResult(scene liveThreadScene, in core.Object, final string) *controlpb.ExecutionResult {
	return &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{
			FinalCommit: final, BaseCommit: in.S("baseCommit"), RevisionRef: in.S("revisionRef"),
			History: &controlpb.StoredObject{Key: in.S("historyKey"), Size: revisionHistorySize, Sha256: revisionHistorySHA},
		}},
	}
}

// endResumedSession takes the follow-up run from start to a claimed, registered delivery.
func (f *fixture) endResumedSession(t *testing.T, scene liveThreadScene) string {
	t.Helper()
	f.runningThread(t, scene)
	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	return f.deliverRevision(t, scene)
}

// Resume D1/D4 and contract D1/D2: a follow-up run fixes the Issue's latest Revision into its session
// spec and its first prompt, and only that session may read exactly that bundle.
func TestFollowUpRunResumesTheIssueLatestRevision(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	scene := seedFollowUpRun(t, f, first.scene, false)
	scene.start(t, f)

	in := f.sessionInput(scene.executionID)
	prior := in.O("priorRevision")
	if prior.S("revisionId") != first.revisionID || prior.S("finalCommit") != revisionFinalCommit {
		t.Fatalf("priorRevision = %v, want the first run's Revision %s at %s", prior, first.revisionID, revisionFinalCommit)
	}
	bundle := prior.O("bundle")
	if bundle.S("key") != first.bundleKey || bundle.N("size") != revisionBundleSize || bundle.S("sha256") != revisionBundleSHA {
		t.Fatalf("priorRevision.bundle = %v, want the verified object %s", bundle, first.bundleKey)
	}
	if got := f.runResumeRevision(scene.runID); got != first.revisionID {
		t.Fatalf("resume_revision_id = %q, want %s", got, first.revisionID)
	}
	resume := f.firstPromptContext(scene.runID).O("resume")
	if resume.S("revisionId") != first.revisionID || resume.S("runId") != first.scene.runID ||
		resume.S("finalCommit") != revisionFinalCommit || resume.S("baseCommit") != seedRunWorkspaceCommit {
		t.Fatalf("the first prompt's resume context = %v", resume)
	}

	grants, e := f.grantRevisionDownload(scene.executionID)
	must(t, e)
	if len(grants.GetGrants()) != 1 {
		t.Fatalf("want exactly one download grant, got %d", len(grants.GetGrants()))
	}
	grant := grants.GetGrants()[0]
	if grant.GetObjectKey() != first.bundleKey || grant.GetMethod() != "GET" || !strings.Contains(grant.GetUrl(), first.bundleKey) || grant.GetExpiresAt() == nil {
		t.Fatalf("download grant = key %q method %q", grant.GetObjectKey(), grant.GetMethod())
	}
	if _, ok := grant.GetHeaders()["if-none-match"]; ok {
		t.Fatal("a read grant must not carry the create-only condition")
	}
	// The first run's ended session has a result, and has no prior: neither may read anything.
	if _, e := f.grantRevisionDownload(first.scene.executionID); status.Code(e) == codes.OK {
		t.Fatal("a session execution with a result must not receive a download grant")
	}
}

// Resume D1/D4: a run of an Issue without any Revision keeps today's prompt and spec exactly.
func TestRunWithoutHistoryStartsFresh(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	if prior := f.sessionInput(scene.executionID)["priorRevision"]; prior != nil {
		t.Fatalf("a run without history must not carry priorRevision, got %v", prior)
	}
	if _, ok := f.firstPromptContext(scene.runID)["resume"]; ok {
		t.Fatal("a run without history must keep its first prompt free of a resume context")
	}
	if _, e := f.grantRevisionDownload(scene.executionID); status.Code(e) == codes.OK {
		t.Fatal("a session without priorRevision must not receive a download grant")
	}
}

// Resume D5 and contract D4: a resumed run that added nothing is unchanged and reuses the prior
// bundle; the next run reaches that bundle in one hop.
func TestResumedRunWithoutNewCommitsReusesThePriorBundle(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)

	in := f.deliveryInputOf(execution)
	prior := in.O("priorRevision")
	if prior.S("revisionId") != first.revisionID || prior.S("finalCommit") != revisionFinalCommit || prior["bundle"] != nil {
		t.Fatalf("the delivery's priorRevision = %v, want the session's prior without bundle", prior)
	}
	if in.S("baseCommit") != resumedBaseCommit {
		t.Fatalf("the delivery stays relative to this run's own base, got %q", in.S("baseCommit"))
	}
	// Ending anywhere else than base or prior is not "unchanged".
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, resumedFinalCommit), "unchanged-elsewhere"); status.Code(e) == codes.OK {
		t.Fatal("an unchanged result off both the base and the prior final commit must be refused")
	}
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, revisionFinalCommit), "unchanged-on-prior"); e != nil {
		t.Fatalf("a resumed run ending on the prior final commit is unchanged: %v", e)
	}
	row := f.revisionRow(second.runID)
	if row == nil || row.BundleKey != "" || row.FinalCommit != revisionFinalCommit || row.BaseCommit != resumedBaseCommit {
		t.Fatalf("the resumed unchanged Revision = %+v", row)
	}
	priorID, holder, _ := f.resumeColumns(second.runID)
	if priorID != first.revisionID || holder != first.revisionID {
		t.Fatalf("prior/holder = %s/%s, want both %s", priorID, holder, first.revisionID)
	}
	if got := f.runResult(second.runID).S("deliveryState"); got != "unchanged" {
		t.Fatalf("deliveryState = %q, want unchanged", got)
	}

	third := seedFollowUpRun(t, f, second, false)
	third.start(t, f)
	next := f.sessionInput(third.executionID).O("priorRevision")
	if next.S("revisionId") != row.ID || next.O("bundle").S("key") != first.bundleKey {
		t.Fatalf("the third run must resume the second Revision through the first's bundle, got %v", next)
	}
}

// Resume D5: a resumed run that committed stores its own bundle and names its prior.
func TestResumedRunWithNewCommitsStoresItsOwnBundle(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	if _, e := f.deliveryTerminal(second, execution, "", 1, deliveredResult(second, in, resumedFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("the resumed delivery must register: %v", e)
	}
	row := f.revisionRow(second.runID)
	if row == nil || row.BundleKey != in.S("bundleKey") || row.FinalCommit != resumedFinalCommit {
		t.Fatalf("the resumed changed Revision = %+v", row)
	}
	priorID, holder, _ := f.resumeColumns(second.runID)
	if priorID != first.revisionID || holder != "" {
		t.Fatalf("prior/holder = %s/%s, want %s and none", priorID, holder, first.revisionID)
	}
	if got := f.runResult(second.runID).S("deliveryState"); got != "saved" {
		t.Fatalf("deliveryState = %q, want saved", got)
	}
}

// Resume D3: only a base commit gone from the remote refuses the prior Revision; the next run then
// starts fresh and says why. Any other restore failure leaves it selectable.
func TestRestoreFailuresAndTheNextRunSelection(t *testing.T) {
	for _, tc := range []struct {
		detail  string
		refused bool
	}{
		{"prior_revision_base_unavailable", true},
		{"prior_revision_unavailable", false},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			f := setup(t)
			first := savedRun(t, f)
			second := seedFollowUpRun(t, f, first.scene, false)
			second.start(t, f)
			if _, e := f.sessionEnd(second, "", 1, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_AGENT_FAILED, tc.detail, ""); e != nil {
				t.Fatalf("a restore failure is a session end: %v", e)
			}
			var refused sql.NullString
			must(t, f.store.Pool.QueryRow(`SELECT resume_refused_reason FROM revisions WHERE id=$1`, first.revisionID).Scan(&refused))
			if refused.Valid != tc.refused {
				t.Fatalf("resume_refused_reason = %v, want refused=%v", refused, tc.refused)
			}
			// The failed run's own delivery ends on its base: an ordinary unchanged Revision without a
			// prior, which is never a candidate.
			execution := f.deliverRevision(t, second)
			in := f.deliveryInputOf(execution)
			if _, e := f.deliveryTerminal(second, execution, "", 1, unchangedResult(second, in), "unchanged"); e != nil {
				t.Fatalf("the failed run's delivery must settle: %v", e)
			}
			if priorID, _, _ := f.resumeColumns(second.runID); priorID != "" {
				t.Fatalf("a run that never left its base records no prior, got %s", priorID)
			}

			third := seedFollowUpRun(t, f, second, false)
			third.start(t, f)
			prior := f.sessionInput(third.executionID)["priorRevision"]
			resume := f.firstPromptContext(third.runID).O("resume")
			if tc.refused {
				if prior != nil || resume.S("skippedRevisionId") != first.revisionID || resume.S("reason") != "resume_refused" {
					t.Fatalf("after a refused restore the next run starts fresh and says why: prior %v resume %v", prior, resume)
				}
				return
			}
			if f.sessionInput(third.executionID).O("priorRevision").S("revisionId") != first.revisionID {
				t.Fatalf("a transient restore failure keeps the Revision selectable, got %v", prior)
			}
		})
	}
}

// Resume D1: a Revision of another project or repository is never restored.
func TestRunOfAMovedIssueStartsFresh(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	moved := seedFollowUpRun(t, f, first.scene, true)
	moved.start(t, f)
	if prior := f.sessionInput(moved.executionID)["priorRevision"]; prior != nil {
		t.Fatalf("a moved Issue must not resume another repository's Revision, got %v", prior)
	}
	resume := f.firstPromptContext(moved.runID).O("resume")
	if resume.S("skippedRevisionId") != first.revisionID || resume.S("reason") != "project_changed" {
		t.Fatalf("resume context = %v, want project_changed", resume)
	}
	if got := f.runResumeRevision(moved.runID); got != "" {
		t.Fatalf("resume_revision_id = %q, want none", got)
	}
}

// Resume D4/D5: the public projection says whether a run stored a bundle and what it resumed.
func TestPublicRunProjectsResume(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, revisionFinalCommit), "unchanged-on-prior"); e != nil {
		t.Fatalf("delivery: %v", e)
	}
	code, out, e := f.threadRequest("GET", "/api/v1/tenants/"+second.tenantID+"/issues/"+second.issueID+"/runs/"+second.runID, "", "")
	must(t, e)
	if code != 200 {
		t.Fatalf("GET run: want 200 got %d %v", code, out)
	}
	if out.S("resumeRevisionId") != first.revisionID {
		t.Fatalf("resumeRevisionId = %v", out["resumeRevisionId"])
	}
	revision := out.O("revision")
	if revision.B("changed") || revision.S("priorRevisionId") != first.revisionID {
		t.Fatalf("revision projection = %v, want unchanged with prior %s", revision, first.revisionID)
	}
}

// Migration 0033: the database itself refuses a reused bundle without a prior, a row that both owns
// and reuses a bundle, and any refusal reason other than the base being gone.
func TestRevisionResumeColumnsKeepTheBundleShape(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, revisionFinalCommit), "unchanged-on-prior"); e != nil {
		t.Fatalf("delivery: %v", e)
	}
	reused := f.revisionRow(second.runID).ID
	for name, q := range map[string]string{
		"reused bundle without prior": `UPDATE revisions SET prior_revision_id=NULL WHERE id='` + reused + `'`,
		"own and reused bundle":       `UPDATE revisions SET bundle_revision_id='` + reused + `' WHERE id='` + first.revisionID + `'`,
		"unknown refusal reason":      `UPDATE revisions SET resume_refused_at=now(), resume_refused_reason='other' WHERE id='` + first.revisionID + `'`,
		"refusal without reason":      `UPDATE revisions SET resume_refused_at=now() WHERE id='` + first.revisionID + `'`,
	} {
		if _, e := f.store.Pool.Exec(q); e == nil {
			t.Fatalf("%s must violate a 0033 constraint", name)
		}
	}
}
