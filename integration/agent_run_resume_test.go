package integration

// Resuming a new IssueRun from its Issue's latest Revision (specs
// decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md as changed by
// cloud/revision/20261017-git-native-delivery-refs-and-resume.md D7 and
// cloud/controller-integration/20261016-snapshot-clone-input-and-execution-ref-grants.md D8). Every
// case drives the production paths: session start through StartQueuedAgentSessionsOnce, dispatch
// through clone_claim and clone_dispatch, session end and delivery through TakeOverNodeEvent.

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
	// The base commit a follow-up run's Workspace was cloned at: the snapshot moved on since the first
	// run, which is what makes "the delivery stays relative to this run's own base" observable.
	resumedBaseCommit = "3333333333333333333333333333333333333333"
	// A commit the resumed Agent made on top of the prior Revision.
	resumedFinalCommit = "4444444444444444444444444444444444444444"
)

// savedRevision is a run whose delivery registered a Revision with its own verified delivery ref.
type savedRevision struct {
	scene       liveThreadScene
	revisionID  string
	deliveryRef string
}

// savedRun drives one run through session, end and a verified delivery that changed the checkout,
// and returns its Revision.
func savedRun(t *testing.T, f *fixture) savedRevision {
	t.Helper()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
	if _, e := f.deliveryTerminal(scene, execution, "", 1, deliveredResult(scene, in, revisionFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("the first run's delivery must register a Revision: %v", e)
	}
	row := f.revisionRow(scene.runID)
	if row == nil || row.DeliveryRef != in.S("deliveryRef") {
		t.Fatal("the first run must register its own delivery ref")
	}
	return savedRevision{scene: scene, revisionID: row.ID, deliveryRef: row.DeliveryRef}
}

// seedFollowUpRun seeds a second run of the prior scene's Issue in exactly the state
// seedStartingAgentRun leaves a first one: `starting`, with its own ready run Workspace cloned at
// resumedBaseCommit, a maintenance binding and a connected Node. With otherProject the Workspace
// belongs to a different project of the same tenant, as after the Issue was moved.
func seedFollowUpRun(t *testing.T, f *fixture, prior liveThreadScene, otherProject bool) liveThreadScene {
	t.Helper()
	return seedFollowUpRunFrom(t, f, prior, otherProject, "")
}

// seedFollowUpRunFrom is seedFollowUpRun whose Workspace was cloned from cloneBase instead of the
// configured Git Server, as after the deployment moved its Git Server; "" keeps the configured one.
func seedFollowUpRunFrom(t *testing.T, f *fixture, prior liveThreadScene, otherProject bool, cloneBase string) liveThreadScene {
	t.Helper()
	var agent, project, user, input string
	must(t, f.store.Pool.QueryRow(`
		SELECT r.executor_id::text, w.project_id::text, w.owner_user_id::text, r.input::text
		FROM issue_runs r JOIN workspaces w ON w.issue_run_id = r.id WHERE r.id=$1`, prior.runID).Scan(&agent, &project, &user, &input))
	runID, workspaceID, nodeID, sandboxID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	operationID, cloneExecution := uuid.NewString(), "exec-baseline-"+workspaceID[:8]
	cloneInput := seedCloneInput(prior.tenantID, project, workspaceID, resumedBaseCommit)
	tx, e := f.store.Pool.Begin()
	must(t, e)
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if otherProject {
		moved := uuid.NewString()
		repository := "https://example.invalid/moved.git"
		exec("project", `WITH p AS (INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'moved','active') RETURNING id,tenant_id) INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) SELECT gen_random_uuid(),tenant_id,id,$5,'main' FROM p`, moved, prior.tenantID, user, prior.spaceID, repository)
		exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), prior.tenantID, user, moved)
		exec("issue", `UPDATE issues SET project_ref=$2 WHERE id=$1`, prior.issueID, moved)
		project = moved
		cloneInput = seedCloneInput(prior.tenantID, project, workspaceID, resumedBaseCommit)
	}
	if cloneBase != "" {
		cloneInput["repositoryUrl"] = cloneBase + "/" + prior.tenantID + "/" + project + ".git"
	}
	exec("run", `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,phase,input) VALUES($1,$2,$3,'agent',$4,'dispatched','starting',$5)`, runID, prior.tenantID, prior.issueID, agent, input)
	exec("run-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,admission_open,runtime_generation,requested_ref,issue_run_id) VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','running','ready',true,1,'main',$5)`, workspaceID, prior.tenantID, user, project, runID)
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'follow-up task')`, uuid.NewString(), workspaceID)
	exec("run-workspace-bind", `UPDATE issue_runs SET workspace_id=$2, version=version+1, updated_at=now() WHERE id=$1`, runID, workspaceID)
	exec("run-workspace-operation", `INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,controller_epoch) VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}',$6,'baseline-hash',1)`,
		operationID, prior.tenantID, user, project, workspaceID, "run-workspace-"+workspaceID[:8])
	exec("run-workspace-clone", `INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,node_operation_id,input,result,dispatched_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,1)`,
		cloneExecution, operationID, workspaceID, nodeID, cloneExecution,
		mustJSON(t, cloneInput),
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

// resumedUnchangedResult is what a Node reports when a resumed run ends on its prior Revision's final
// commit: it still pushes that commit to its own delivery ref (contract decision D4, Revision D7).
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

// Resume D1/D4, Revision D7 and contract D8: a follow-up run fixes the Issue's latest Revision into its
// session spec and its first prompt, naming the Git Server repository its checkout was cloned from,
// the prior's own delivery ref and the prior's base commit.
func TestFollowUpRunResumesTheIssueLatestRevision(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	scene := seedFollowUpRun(t, f, first.scene, false)
	scene.start(t, f)
	if f.scalar("SELECT extract(epoch FROM startup_deadline_at-startup_ready_at)::bigint FROM issue_runs WHERE id=$1", scene.runID) != 360 ||
		f.scalar("SELECT extract(epoch FROM startup_deadline_at-startup_ready_at)::bigint FROM issue_runs WHERE id=$1", first.scene.runID) != 120 {
		t.Fatal("restore must use one ready-based 6m window; fresh startup must use 2m")
	}

	in := f.sessionInput(scene.executionID)
	prior := in.O("priorRevision")
	if prior.S("revisionId") != first.revisionID || prior.S("finalCommit") != revisionFinalCommit {
		t.Fatalf("priorRevision = %v, want the first run's Revision %s at %s", prior, first.revisionID, revisionFinalCommit)
	}
	var clone string
	must(t, f.store.Pool.QueryRow(`SELECT e.input->>'repositoryUrl' FROM clone_executions e JOIN issue_runs r ON r.workspace_id=e.workspace_id WHERE r.id=$1`, scene.runID).Scan(&clone))
	if prior.S("deliveryRef") != first.deliveryRef || prior.S("baseCommit") != seedRunWorkspaceCommit || prior.S("gitRepositoryUrl") != clone || prior["bundle"] != nil {
		t.Fatalf("priorRevision = %v, want the first run's delivery ref %s, its base and the clone's repository %s", prior, first.deliveryRef, clone)
	}
	if got := f.runResumeRevision(scene.runID); got != first.revisionID {
		t.Fatalf("resume_revision_id = %q, want %s", got, first.revisionID)
	}
	resume := f.firstPromptContext(scene.runID).O("resume")
	if resume.S("revisionId") != first.revisionID || resume.S("runId") != first.scene.runID ||
		resume.S("finalCommit") != revisionFinalCommit || resume.S("baseCommit") != seedRunWorkspaceCommit {
		t.Fatalf("the first prompt's resume context = %v", resume)
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
}

// Contract D4 and Revision D7: a resumed run that added nothing is unchanged, yet it pushed its final
// commit — the prior's — to its own delivery ref, so the next run resumes it directly with no hop to
// the prior's evidence.
func TestResumedRunWithoutNewCommitsHasItsOwnDeliveryRef(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)

	in := f.deliveryInputOf(execution)
	prior := in.O("priorRevision")
	if prior.S("revisionId") != first.revisionID || prior.S("finalCommit") != revisionFinalCommit || len(prior) != 2 {
		t.Fatalf("the delivery's priorRevision = %v, want only the session prior's identity and final commit", prior)
	}
	if in.S("baseCommit") != resumedBaseCommit {
		t.Fatalf("the delivery stays relative to this run's own base, got %q", in.S("baseCommit"))
	}
	f.push(in, revisionFinalCommit)
	// Ending anywhere else than base or prior is not "unchanged".
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, resumedFinalCommit), "unchanged-elsewhere"); status.Code(e) == codes.OK {
		t.Fatal("an unchanged result off both the base and the prior final commit must be refused")
	}
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, revisionFinalCommit), "unchanged-on-prior"); e != nil {
		t.Fatalf("a resumed run ending on the prior final commit is unchanged: %v", e)
	}
	row := f.revisionRow(second.runID)
	if row == nil || row.DeliveryRef != in.S("deliveryRef") || row.BundleKey != "" || row.FinalCommit != revisionFinalCommit || row.BaseCommit != resumedBaseCommit {
		t.Fatalf("the resumed unchanged Revision = %+v", row)
	}
	priorID, holder, _ := f.resumeColumns(second.runID)
	if priorID != first.revisionID || holder != "" {
		t.Fatalf("prior/holder = %s/%s, want %s and no borrowed bundle", priorID, holder, first.revisionID)
	}
	if got := f.runResult(second.runID).S("deliveryState"); got != "unchanged" {
		t.Fatalf("deliveryState = %q, want unchanged", got)
	}

	third := seedFollowUpRun(t, f, second, false)
	third.start(t, f)
	next := f.sessionInput(third.executionID).O("priorRevision")
	if next.S("revisionId") != row.ID || next.S("deliveryRef") != row.DeliveryRef || next.S("finalCommit") != revisionFinalCommit {
		t.Fatalf("the third run must resume the second Revision through its own delivery ref, got %v", next)
	}
}

// Revision D7: a Revision that ended on its own base commit holds nothing a later run could resume,
// and one registered before Git delivery has no ref to fetch; neither is a candidate.
func TestRevisionsWithoutNewCommitsOrRefsAreNotResumed(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	f.push(in, in.S("baseCommit"))
	if _, e := f.deliveryTerminal(scene, execution, "", 1, unchangedResult(scene, in), "unchanged"); e != nil {
		t.Fatalf("the unchanged delivery must register: %v", e)
	}
	next := seedFollowUpRun(t, f, scene, false)
	next.start(t, f)
	if prior := f.sessionInput(next.executionID)["priorRevision"]; prior != nil {
		t.Fatalf("a Revision that never left its base must not be resumed, got %v", prior)
	}
	if _, ok := f.firstPromptContext(next.runID)["resume"]; ok {
		t.Fatal("no candidate means no resume context at all")
	}
}

// Contract D8: a prior Revision is fetched from the repository the checkout was cloned from or not at
// all. A Workspace cloned from another Git Server address starts fresh and says why.
func TestRunClonedFromAnotherRepositoryStartsFresh(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	moved := seedFollowUpRunFrom(t, f, first.scene, false, "https://git-server.elsewhere.invalid")
	moved.start(t, f)
	if prior := f.sessionInput(moved.executionID)["priorRevision"]; prior != nil {
		t.Fatalf("a prior in another repository than the checkout's must not be resumed, got %v", prior)
	}
	resume := f.firstPromptContext(moved.runID).O("resume")
	if resume.S("skippedRevisionId") != first.revisionID || resume.S("reason") != "repository_changed" {
		t.Fatalf("resume context = %v, want repository_changed", resume)
	}
}

// Resume D5 as changed by Revision D7: a resumed run that committed delivers its own ref and names
// its prior.
func TestResumedRunWithNewCommitsDeliversItsOwnRef(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	f.push(in, resumedFinalCommit)
	if _, e := f.deliveryTerminal(second, execution, "", 1, deliveredResult(second, in, resumedFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("the resumed delivery must register: %v", e)
	}
	row := f.revisionRow(second.runID)
	if row == nil || row.DeliveryRef != in.S("deliveryRef") || row.FinalCommit != resumedFinalCommit {
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

// Resume D3 as changed by Revision D7: only a prior delivery ref the Git Server could not serve
// refuses the prior Revision, and not when the run had already asked to cancel — the Git Server
// refuses a cancelled run's reads with the same answer. The next run then starts fresh and says why.
// Any other restore failure leaves it selectable.
func TestRestoreFailuresAndTheNextRunSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		detail    string
		cancelled bool
		refused   bool
	}{
		{"prior_revision_base_unavailable", "prior_revision_base_unavailable", false, true},
		{"prior_revision_unavailable", "prior_revision_unavailable", false, false},
		{"base unavailable after a cancel request", "prior_revision_base_unavailable", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			first := savedRun(t, f)
			second := seedFollowUpRun(t, f, first.scene, false)
			second.start(t, f)
			if tc.cancelled {
				_, e := f.store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1`, second.runID)
				must(t, e)
			}
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
			if tc.cancelled {
				// A cancelled run delivers nothing worth resuming; the next run is what matters.
				_, e := f.store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=NULL WHERE id=$1`, second.runID)
				must(t, e)
			}
			execution := f.deliverRevision(t, second)
			in := f.deliveryInputOf(execution)
			f.push(in, in.S("baseCommit"))
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

// Resume D4 and Revision D8: the public projection says whether a run delivered new commits and what
// it resumed; a Git Revision has no bundle size.
func TestPublicRunProjectsResume(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
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
	if revision.B("changed") || revision.S("priorRevisionId") != first.revisionID || revision["bundleSize"] != nil {
		t.Fatalf("revision projection = %v, want unchanged with prior %s and no bundle size", revision, first.revisionID)
	}
}

// Migrations 0033 and 0037: the database itself refuses a Git Revision that also names a bundle or a
// borrowed one, a malformed delivery ref, a second Revision claiming the same delivery ref, and any
// refusal reason other than the prior being unavailable.
func TestRevisionShapeKeepsGitAndBundleRowsApart(t *testing.T) {
	f := setup(t)
	first := savedRun(t, f)
	second := seedFollowUpRun(t, f, first.scene, false)
	second.start(t, f)
	execution := f.endResumedSession(t, second)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
	if _, e := f.deliveryTerminal(second, execution, "", 1, resumedUnchangedResult(second, in, revisionFinalCommit), "unchanged-on-prior"); e != nil {
		t.Fatalf("delivery: %v", e)
	}
	resumed := f.revisionRow(second.runID).ID
	for name, q := range map[string]string{
		"git row with a bundle":                            `UPDATE revisions SET bundle_key='k', bundle_size=1, bundle_sha256='` + strings.Repeat("a", 64) + `' WHERE id='` + first.revisionID + `'`,
		"git row borrowing a bundle":                       `UPDATE revisions SET bundle_revision_id='` + first.revisionID + `' WHERE id='` + resumed + `'`,
		"malformed delivery ref":                           `UPDATE revisions SET delivery_ref='refs/heads/main' WHERE id='` + first.revisionID + `'`,
		"two rows claiming one ref":                        `UPDATE revisions SET delivery_ref=(SELECT delivery_ref FROM revisions WHERE id='` + first.revisionID + `') WHERE id='` + resumed + `'`,
		"unknown refusal reason":                           `UPDATE revisions SET resume_refused_at=now(), resume_refused_reason='other' WHERE id='` + first.revisionID + `'`,
		"refusal without reason":                           `UPDATE revisions SET resume_refused_at=now() WHERE id='` + first.revisionID + `'`,
		"git shape without its ref and a changed checkout": `UPDATE revisions SET delivery_ref=NULL WHERE id='` + first.revisionID + `'`,
	} {
		if _, e := f.store.Pool.Exec(q); e == nil {
			t.Fatalf("%s must violate a revisions constraint", name)
		}
	}
}
