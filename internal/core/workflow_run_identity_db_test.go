package core

// DB tests for the WorkflowRun execution identity (migration 0038, specs#99 D3/D8): a run's frozen
// identity drives the workspace lifecycle, the workspace binds to workflow_runs, and the control
// plane's seams — enqueue, dispatch, runtime permit and the settlement hooks — accept a WorkflowRun
// as the run of either kind. Session execution stays unwired (the scheduler still fails runs on
// unwired agent nodes); these tests exercise the seams the execution wiring lands on.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedWorkflowIdentityScene loads one tenant with an active project, a workflow, a frozen snapshot and
// a pending run carrying the frozen identity columns (created_by, project_id). withProject=false
// stages the editor-path run as it exists today — identity unbound — for the refusal test.
func seedWorkflowIdentityScene(t *testing.T, store *Store, withProject bool) (runID, project, actor string) {
	t.Helper()
	uid, tid := uuid.NewString(), uuid.NewString()
	wid, sid, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	project, space := uuid.NewString(), uuid.NewString()
	var projectID any
	if withProject {
		projectID = project
	}
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	exec := func(name, q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'wfi','active')`, uid)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'wfi','active')`, tid)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tid, uid)
	exec("space", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'wfi','wfi-'||left($4::text,8),$3)`, space, tid, uid, project)
	exec("project", `WITH p AS (INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'wfi','active') RETURNING id,tenant_id) INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) SELECT gen_random_uuid(),tenant_id,id,'https://example.invalid/wfi.git','main' FROM p`, project, tid, uid, space)
	// A live project must carry exactly one main workspace (deferred project_main trigger).
	exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), tid, uid, project)
	exec("workflow", `INSERT INTO workflows(id,tenant_id,name,graph) VALUES($1,$2,'wfi','{"nodes":[],"edges":[]}'::jsonb)`, wid, tid)
	exec("snapshot", `INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'v1','{"nodes":[],"edges":[]}'::jsonb)`, sid, tid, wid)
	exec("run", `INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_by,project_id) VALUES($1,$2,$3,$4,'disp','pending','{}'::jsonb,$5,$6)`, runID, tid, wid, sid, uid, projectID)
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return runID, project, uid
}

// The lifecycle a wired run will drive: the frozen identity provisions the Workspace (created_by is
// the creator), the create_workspace terminal hands runtime control to the run, and the declared
// delete settles — every step accepting the WorkflowRun as the Workspace's owner.
func TestWorkflowRunWorkspaceLifecycle(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, actor := seedWorkflowIdentityScene(t, store, true)

	var wsID, createOp string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("the project must be idle")
		}
		wsID, createOp = created.O("workspace").S("id"), created.O("operation").S("id")
		return Object{}
	}); err != nil {
		t.Fatalf("create workflow run workspace: %v", err)
	}
	var creator, boundRun, kind string
	if err := store.Pool.QueryRow(`SELECT creator_user_id,COALESCE(workflow_run_id::text,''),kind FROM workspaces WHERE id=$1`, wsID).
		Scan(&creator, &boundRun, &kind); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if creator != actor || boundRun != runID || kind != "isolated" {
		t.Fatalf("workspace creator=%q run=%q kind=%q; want the frozen actor, the owning run, isolated", creator, boundRun, kind)
	}

	// The create_workspace terminal step the control plane itself drives: the operation succeeds,
	// runtime control is handed back reserved for this run, and the settlement hook accepts a
	// workflow-owned workspace — writing no issue-run facts.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		settleCreateWorkspaceOperation(t, tx, createOp)
		runWorkspaceSettled(tx, runID, "ready")
		return Object{}
	}); err != nil {
		t.Fatalf("settle create_workspace terminal: %v", err)
	}
	var state, maintenanceRun string
	if err := store.Pool.QueryRow(`SELECT state,COALESCE(maintenance_run_id::text,'') FROM runtime_controls WHERE workspace_id=$1`, wsID).
		Scan(&state, &maintenanceRun); err != nil {
		t.Fatalf("read runtime control: %v", err)
	}
	if state != "maintenance" || maintenanceRun != runID {
		t.Fatalf("runtime control state=%q run=%q; want maintenance reserved for the workflow run", state, maintenanceRun)
	}

	// Deletion: declared like the scheduler declares it, settled through the terminal step, and
	// accepted by the delete hook — the terminal transaction must not abort on a workflow run id.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		declared := deleteWorkflowRunWorkspace(tx, runID)
		if declared.B("busy") {
			t.Fatal("declare delete: the project must be idle")
		}
		deleteOp := declared.O("operation").S("id")
		tx.exec(`UPDATE operations SET state='succeeded', step='done', version=version+1, updated_at=now() WHERE id=$1`, deleteOp)
		tx.exec(`UPDATE workspaces SET deleted_at=now(), observed_state='deleted', version=version+1 WHERE id=$1`, wsID)
		runWorkspaceDeleted(tx, runID)
		finishRuntimeMaintenance(tx, deleteOp)
		return Object{}
	}); err != nil {
		t.Fatalf("settle delete_workspace terminal: %v", err)
	}
	var deleted sql.NullString
	if err := store.Pool.QueryRow(`SELECT observed_state FROM workspaces WHERE id=$1`, wsID).Scan(&deleted); err != nil {
		t.Fatalf("read deleted workspace: %v", err)
	}
	if deleted.String != "deleted" {
		t.Fatalf("workspace observed_state=%q; want deleted", deleted.String)
	}
	if err := store.Pool.QueryRow(`SELECT state FROM runtime_controls WHERE workspace_id=$1`, wsID).Scan(&state); err != nil {
		t.Fatalf("read runtime control after delete: %v", err)
	}
	if state != "idle" {
		t.Fatalf("runtime control state=%q after the delete settled; want idle", state)
	}
}

// A run whose identity never learned a project — the editor path today — has no place to provision a
// Workspace in, and says so instead of guessing one.
func TestWorkflowRunWorkspaceRequiresProject(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _ := seedWorkflowIdentityScene(t, store, false)
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		createWorkflowRunWorkspace(tx, runID)
		return Object{}
	})
	if ErrorCode(err).Code != "workflow_run_project_required" {
		t.Fatalf("error = %v; want workflow_run_project_required", err)
	}
}

// Execution work names exactly one run of either kind: the discriminator constraint rejects a row
// carrying both owners and a row carrying neither, while the IssueRun-only shape keeps inserting.
func TestExecutionWorkOwnerExclusivity(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	wfRun, _, _ := seedWorkflowIdentityScene(t, store, true)
	doc := `'{"kind":"agent_session"}'::jsonb`
	if _, err := store.Pool.Exec(`INSERT INTO execution_work(id,run_id,workflow_run_id,kind,input,target) VALUES($1,$2,$3,'agent_session',`+doc+`,`+doc+`)`, uuid.NewString(), seed.run, wfRun); err == nil || !strings.Contains(err.Error(), "execution_work_owner_exclusive") {
		t.Fatalf("both owners: err = %v; want execution_work_owner_exclusive", err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO execution_work(id,kind,input,target) VALUES($1,'agent_session',`+doc+`,`+doc+`)`, uuid.NewString()); err == nil || !strings.Contains(err.Error(), "execution_work_owner_exclusive") {
		t.Fatalf("neither owner: err = %v; want execution_work_owner_exclusive", err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target) VALUES($1,$2,'agent_session',`+doc+`,`+doc+`)`, uuid.NewString(), seed.run); err != nil {
		t.Fatalf("issue-run-only work must still insert: %v", err)
	}
}

// The seam a wired run drives end to end: work registers against the workflow run, repeats of the
// same input are idempotent, the Node's dispatch is accepted under workflow ownership, and the run
// reserved on runtime control is permitted to bind the Node.
func TestWorkflowExecutionWorkDispatchSeam(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, actor := seedWorkflowIdentityScene(t, store, true)

	var wsID string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("the project must be idle")
		}
		wsID = created.O("workspace").S("id")
		settleCreateWorkspaceOperation(t, tx, created.O("operation").S("id"))
		runWorkspaceSettled(tx, runID, "ready")
		tx.exec(`UPDATE workspaces SET observed_state='ready', admission_open=true, version=version+1 WHERE id=$1`, wsID)
		return Object{}
	}); err != nil {
		t.Fatalf("stage ready workspace: %v", err)
	}
	sandbox, nodeService := bindConnectedNode(t, store, wsID)
	var nodeRow string
	if err := store.Pool.QueryRow(`SELECT id FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL`, wsID).Scan(&nodeRow); err != nil {
		t.Fatalf("read node row: %v", err)
	}

	input := Object{"kind": "agent_session"}
	target := Object{"workspaceId": wsID, "sandboxInstanceId": sandbox, "nodeId": nodeService}
	work, err := store.EnqueueExecutionWork(context.Background(), runID, "agent_session", input, target, time.Time{})
	if err != nil {
		t.Fatalf("enqueue workflow work: %v", err)
	}
	if work.S("workflowRunId") != runID || work.S("runId") != "" {
		t.Fatalf("work %v must register against the workflow run alone", work)
	}
	again, err := store.EnqueueExecutionWork(context.Background(), runID, "agent_session", input, target, time.Time{})
	if err != nil || again.S("id") != work.S("id") {
		t.Fatalf("same unregistered input returned %v (err %v); want the original item", again, err)
	}
	_, err = store.EnqueueExecutionWork(context.Background(), runID, "agent_session", input, Object{"workspaceId": wsID, "sandboxInstanceId": sandbox, "nodeId": "node-elsewhere"}, time.Time{})
	if ErrorCode(err).Code != "dispatch_conflict" {
		t.Fatalf("conflicting target: err = %v; want dispatch_conflict", err)
	}

	execution := "exec-" + runID[:8]
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		nodeDispatch(tx, &ControlRequest{Body: Object{
			"operationId": runID, "executionId": execution, "nodeId": nodeService,
			"input": Object{"kind": "agent_session"}, "epoch": int64(1),
		}})
		return Object{}
	}); err != nil {
		t.Fatalf("dispatch under workflow ownership: %v", err)
	}
	var boundExecution string
	if err := store.Pool.QueryRow(`SELECT COALESCE(execution_id::text,'') FROM execution_work WHERE id=$1`, work.S("id")).Scan(&boundExecution); err != nil {
		t.Fatalf("read work: %v", err)
	}
	if boundExecution != execution {
		t.Fatalf("work registered execution %q; want %q", boundExecution, execution)
	}

	// The Node acks its binding and takes the permit: the maintenance holder is the workflow run, and
	// the binding's actor is the run's frozen creator.
	var epoch, controlVersion int64
	if err := store.Pool.QueryRow(`SELECT control_epoch,version FROM runtime_controls WHERE workspace_id=$1`, wsID).Scan(&epoch, &controlVersion); err != nil {
		t.Fatalf("read control epoch: %v", err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{
			"workspaceId": wsID, "controlEpoch": epoch, "controlVersion": controlVersion, "nodeInstanceId": nodeRow,
			"inputClosed": false, "unfinishedExecutionIds": []any{},
		}})
		return Object{}
	}); err != nil {
		t.Fatalf("runtime ack: %v", err)
	}
	var permit Object
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		permit = runtimeControlCommand(tx, &ControlRequest{Action: "runtime_permit", Body: Object{
			"workspaceId": wsID, "controlEpoch": epoch, "nodeInstanceId": nodeRow, "executionId": execution,
		}})
		return Object{}
	}); err != nil {
		t.Fatalf("runtime permit: %v", err)
	}
	if permit.S("actorUserId") != actor || permit.S("operationId") != runID {
		t.Fatalf("permit binding actor=%q operation=%q; want the frozen creator and the workflow run", permit.S("actorUserId"), permit.S("operationId"))
	}
}

// The claim hands the Controller the run its work belongs to, and the dispatch that echoes that value
// back is accepted. This is the whole chain — clone_claim -> clone_dispatch -> node_executions —
// driven through the real control actions rather than by calling nodeDispatch directly.
//
// The seam test above dispatches directly and so cannot see this: a workflow work item carries
// workflow_run_id and leaves run_id NULL by 0038's mutual-exclusion CHECK, so a claim that projects
// the agent-only column hands the Controller the empty string, and every dispatch of that item then
// matches no row and is refused as a conflict it can never resolve.
func TestWorkflowExecutionWorkClaimCarriesItsRunID(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _ := seedWorkflowIdentityScene(t, store, true)
	seedControllerLease(t, store)

	var wsID string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("the project must be idle")
		}
		wsID = created.O("workspace").S("id")
		settleCreateWorkspaceOperation(t, tx, created.O("operation").S("id"))
		runWorkspaceSettled(tx, runID, "ready")
		tx.exec(`UPDATE workspaces SET observed_state='ready', admission_open=true, version=version+1 WHERE id=$1`, wsID)
		return Object{}
	}); err != nil {
		t.Fatalf("stage ready workspace: %v", err)
	}
	sandbox, nodeService := bindConnectedNode(t, store, wsID)

	input := Object{"kind": "agent_session"}
	target := Object{"workspaceId": wsID, "sandboxInstanceId": sandbox, "nodeId": nodeService}
	work, err := store.EnqueueExecutionWork(context.Background(), runID, "agent_session", input, target, time.Time{})
	if err != nil {
		t.Fatalf("enqueue workflow work: %v", err)
	}

	claims := controllerClaims()
	pick, err := store.Control(context.Background(), &ControlRequest{Action: "clone_claim", Body: Object{"epoch": 1}, Service: claims})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	claimed := pick.O("work")
	if claimed.S("id") != work.S("id") {
		t.Fatalf("claim returned work %v; want the declared workflow item", claimed)
	}
	if claimed.S("claimedRunId") != runID {
		t.Fatalf("claim projected run %q; want the workflow run %q — an empty value is refused at every dispatch", claimed.S("claimedRunId"), runID)
	}

	execution := "exec-claim-" + runID[:8]
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_dispatch", SubmissionID: "claim-dispatch", Service: claims,
		Body: Object{"epoch": int64(1), "operationId": claimed.S("claimedRunId"), "executionId": execution, "nodeId": nodeService, "input": input},
	}); err != nil {
		t.Fatalf("dispatch the claimed run id: %v", err)
	}
	var operation, workID string
	if err := store.Pool.QueryRow(`SELECT coalesce(operation_id::text,''),coalesce(work_id::text,'') FROM node_executions WHERE execution_id=$1`, execution).Scan(&operation, &workID); err != nil {
		t.Fatalf("read node execution: %v", err)
	}
	if operation != runID || workID != work.S("id") {
		t.Fatalf("node execution operation=%q work=%q; want the workflow run and the claimed item", operation, workID)
	}
}
