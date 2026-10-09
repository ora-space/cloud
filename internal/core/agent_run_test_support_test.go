package core

// Shared scaffolding for the B-side Agent IssueRun DB tests that live in package core
// (agent_run_settle_db_test.go, agent_run_release_db_test.go, agent_run_thread_end_db_test.go,
// agent_run_thread_lifecycle_db_test.go).
//
// They are white-box (package core) for exactly one reason: the A→B seam hands the business hooks the
// unexported *transaction, so a Store cannot be driven through the five On* callbacks from outside the
// package. There is no in-memory substitute for PostgreSQL anywhere here — every scene is a real
// isolated schema, and every transition under test runs on the shipped seams:
//
//	createRunWorkspace   the control plane's own Workspace + create_workspace operation creation.
//	startAgentSession    the B-owned Session Start core (seq=1, thread_state='pending', work enqueue).
//	Store.Control        the real control actions `clone_claim`, `clone_dispatch` and `thread_events`,
//	                     i.e. the shipped Node-registration and Thread-takeover path.
//	runWorkspaceSettled  the create_workspace terminal callback the control plane fires.
//
// Nothing here installs a stub control plane or a fake hook set: those were the pre-merge
// implementation's seams, and the merged architecture has one A side and one B side, wired by
// BindBusinessHooks. Scenes that stage an A-side precondition the test does not exercise (a
// connected Node, a completed clone, a reserved runtime control) write it directly, and say so.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// dispatcherDB creates an isolated PostgreSQL schema, migrates it, and returns a Store with Cloud's
// business lifecycle bound onto the control plane's five same-transaction callbacks — exactly the way
// cmd/server wires them at process start. A Store returned here is therefore the production Store as
// far as the A/B seam is concerned; only the sleepers and background loops are the caller's business.
func dispatcherDB(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required; agent run DB tests must not skip under REQUIRE_POSTGRES")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	admin := stdlib.OpenDB(*config)
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin: %v", err)
	}
	schema := "core_run_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		_ = pool.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	store := &Store{Pool: pool}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	BindBusinessHooks(store)
	return store
}

// commandStore is dispatcherDB with the control signal hub wired: the Thread command path publishes
// ThreadCommandAvailable, and a nil hub would silently swallow the signal.
func commandStore(t *testing.T) *Store {
	t.Helper()
	store := dispatcherDB(t)
	store.Signals = NewControlHub()
	return store
}

// dispSeed is the identity set of one seeded project + issue + active Space Agent + queued real-agent
// run, plus the project uuid needed to make it busy.
type dispSeed struct {
	tenant, project, issue, agent, run string
}

// seedDispatchScene loads a minimal but schema-valid scene in one transaction: an active tenant with
// an admin member, its collaboration workspace, an active project, a real Space Agent, an issue bound
// to the project, and one queued executor_type='agent' run referencing the Space Agent. When busy is
// true an in-flight operation is added so the project is busy. IssueRun D2's project gate is satisfied
// via issues.project_ref, and the run carries the `run.enqueued` activity the control plane resolves
// the run's trigger actor from (runTriggerActor).
func seedDispatchScene(t *testing.T, pool *sql.DB, busy bool) dispSeed {
	t.Helper()
	agent, run := uuid.NewString(), uuid.NewString()
	seed := dispSeed{tenant: uuid.NewString(), project: uuid.NewString(), issue: uuid.NewString(), agent: agent, run: run}
	user := uuid.NewString()
	space := uuid.NewString()
	tx, err := pool.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	exec := func(name, q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'disp','active')`, user)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'disp','active')`, seed.tenant)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, seed.tenant, user)
	exec("workspace", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'disp','disp-'||left($4::text,8),$3)`, space, seed.tenant, user, space)
	exec("project", `INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'disp','https://example.invalid/disp.git','main','active')`, seed.project, seed.tenant, user, space)
	// A live project must carry exactly one main workspace (deferred project_main trigger); the run
	// Workspace the seam creates is a separate kind='isolated' workspace later.
	exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), seed.tenant, user, seed.project)
	exec("agent", `INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) VALUES($1,$2,$3,'official/hello-world','Hello','active')`, seed.agent, space, seed.tenant)
	exec("issue", `INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number) VALUES($1,$2,$3,$4,'disp issue',1)`, seed.issue, seed.tenant, user, seed.project)
	exec("run", `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status) VALUES($1,$2,$3,'agent',$4,'queued')`, seed.run, seed.tenant, seed.issue, seed.agent)
	// The append-only enqueue evidence names the user whose request created the run; the control plane
	// resolves the create_workspace actor from exactly this row (runTriggerActor).
	exec("enqueue-activity", `INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details) VALUES($1,$2,$3,1,'user',$4,'run.enqueued',jsonb_build_object('runId',$5::text))`, uuid.NewString(), seed.tenant, seed.issue, user, seed.run)
	if busy {
		exec("busy-op", `INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,'start','running','ready','{}','disp-busy','disp-busy-hash')`, uuid.NewString(), seed.tenant, user, seed.project)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return seed
}

// runClaim reads a run's status/phase directly from PostgreSQL (the public run shape strips the agent
// lifecycle columns, so phase must be read at the storage boundary).
func runClaim(t *testing.T, store *Store, tid, runID string) (status string, phase sql.NullString) {
	t.Helper()
	if err := store.Pool.QueryRow(`SELECT status, phase FROM issue_runs WHERE id=$1 AND tenant_id=$2`, runID, tid).Scan(&status, &phase); err != nil {
		t.Fatalf("read run claim: %v", err)
	}
	return status, phase
}

// baseCommit is a well-formed 40-hex commit id for the Workspace baselines these scenes record. The
// schema constrains the column to a real object id, so a placeholder would be rejected before any
// business predicate ran.
const baseCommit = "0123456789abcdef0123456789abcdef01234567"

// sessionStartInput is a representative frozen run-create snapshot (IssueRun D1/D6): the business
// layer's buildRunContext fallback shape, the pinned plugin identity/version and the frozen git
// identity. It is what a run's immutable `input` holds by the time Session Start reads it.
func sessionStartInput() Object {
	return Object{
		"task":              "Fix the auth flow",
		"interactionValues": Object{"scope": "web", "assignee": "alice"},
		"target":            Object{"type": "issue", "id": "T-1"},
		"contextRefs": []any{
			Object{"refType": "commit", "refId": "abc123"},
			Object{"refType": "file", "refId": "internal/auth.go"},
		},
		"agentPluginId":      "official/hello-world",
		"agentPluginVersion": "1.0.0",
		"gitIdentity":        Object{"name": "disp", "email": "disp@users.noreply.ora.invalid"},
	}
}

// sessionScene is one seeded run bound to a live, admitted run Workspace.
type sessionScene struct {
	seed dispSeed
	ws   string
}

// seedStartingRun brings the seeded run to the state Session Start starts from: the run Workspace
// exists and is bound to the run, the create_workspace operation completed (so the runtime control is
// reserved for this run), the Workspace holds a recorded baseline produced by a successful checkout
// execution, and the run is `starting`/`dispatched` with the frozen input snapshot.
//
// The Workspace, its operation and the runtime-control handback come from the control plane's own
// `createRunWorkspace` and `finishRuntimeMaintenance`, which is the real path; only the completed
// clone (an A-side step this file does not exercise) is staged, and it is staged as the exact rows the
// clone step leaves behind.
func seedStartingRun(t *testing.T, store *Store, input Object) sessionScene {
	t.Helper()
	seed := seedDispatchScene(t, store.Pool, false)
	var ws string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createRunWorkspace(tx, seed.run)
		if created.B("busy") {
			t.Fatalf("seed starting run: the project must be idle")
		}
		ws = created.O("workspace").S("id")
		op := created.O("operation").S("id")
		// The create_workspace operation's own terminal step, as the Controller would drive it: the
		// operation succeeds, the runtime control is handed back reserved for this run
		// (finishRuntimeMaintenance) and the baseline clone is recorded against it. A run is `starting`
		// only in the transaction that writes this, so leaving the operation queued would stage a state
		// the shipped code cannot produce — and would keep the Project busy, which is a different
		// precondition than the one these tests are about.
		tx.exec(`UPDATE operations SET state='succeeded', step='done', version=version+1, updated_at=now() WHERE id=$1`, op)
		finishRuntimeMaintenance(tx, op)
		tx.exec(`UPDATE workspaces SET observed_state='ready', admission_open=true, base_commit_id=$2, version=version+1 WHERE id=$1`, ws, baseCommit)
		tx.exec(`INSERT INTO clone_executions(execution_id,operation_id,node_id,node_operation_id,workspace_id,input,result,dispatched_epoch)
			VALUES($1,$2,'node-checkout','op-checkout',$3,'{"kind":"clone"}',jsonb_build_object('outcome','clone_ready','commit',$4::text),1)`,
			"exec-checkout-"+ws[:8], op, ws, baseCommit)
		tx.exec(`UPDATE issue_runs SET phase='starting', status='dispatched', workspace_id=$2, input=$3, version=version+1, updated_at=now() WHERE id=$1`,
			seed.run, ws, jsonText(input))
		return Object{}
	}); err != nil {
		t.Fatalf("seed starting run: %v", err)
	}
	return sessionScene{seed: seed, ws: ws}
}

// bindConnectedNode gives the run Workspace a live sandbox + connected Node, which is the evidence the
// control plane's currentNode requires before it will hand that Workspace's Node a session or a
// delivery.
//
// The Workspace's generation is advanced exactly as the sandbox step does (control.go's `sandbox`
// case): the generation is the Workspace's own counter of "which sandbox incarnation is live", so a
// sandbox row on a generation the Workspace does not record would be invisible to every predicate.
func bindConnectedNode(t *testing.T, store *Store, ws string) (sandboxID, nodeID string) {
	t.Helper()
	sandboxID, nodeID = newID(), newID()
	if _, err := store.Pool.Exec(`UPDATE workspaces SET runtime_generation=runtime_generation+1, version=version+1 WHERE id=$1`, ws); err != nil {
		t.Fatalf("advance workspace generation: %v", err)
	}
	var generation int64
	if err := store.Pool.QueryRow(`SELECT runtime_generation FROM workspaces WHERE id=$1`, ws).Scan(&generation); err != nil {
		t.Fatalf("read workspace generation: %v", err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,$3,'running')`, sandboxID, ws, generation); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	// node_instances.node_id is the Node service's own identity (0016), which is what a Controller
	// names in a dispatch; the row id is Cloud's handle on the registration. Both the work target and
	// the dispatch body carry the former, so that is what this helper returns.
	nodeServiceID := "node-" + nodeID[:8]
	if _, err := store.Pool.Exec(`INSERT INTO node_instances(id,workspace_id,sandbox_instance_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id)
		VALUES($1,$2,$3,'node','connected',1,true,$4,$5)`, nodeID, ws, sandboxID, nodeServiceID, "inc-"+nodeID[:8]); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return sandboxID, nodeServiceID
}

// firstTurn reads the run's Thread entry seq=1, if present.
func firstTurn(t *testing.T, store *Store, runID string) (source, kind string, record Object, turnID interface{}, present bool) {
	t.Helper()
	var src, k string
	var rec []byte
	var id *string
	err := store.Pool.QueryRow(`SELECT source,kind,record,turn_id FROM thread_entries WHERE run_id=$1 AND seq=1`, runID).Scan(&src, &k, &rec, &id)
	if err == sql.ErrNoRows {
		return "", "", nil, nil, false
	}
	if err != nil {
		t.Fatalf("read first turn: %v", err)
	}
	return src, k, mustObject(t, rec), id, true
}

func mustObject(t *testing.T, b []byte) Object {
	t.Helper()
	var obj Object
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("decode jsonb: %v", err)
	}
	return obj
}

// runSessionStartPass drives one full Session Start pass exactly like StartQueuedAgentSessionsOnce but
// surfaces per-run errors, so a test reveals the true failure instead of the production loop's
// deliberate per-run isolation swallowing it.
func runSessionStartPass(t *testing.T, store *Store) {
	t.Helper()
	ids, err := store.scanStartingAgentRuns(context.Background(), agentSessionStartBatchSize)
	if err != nil {
		t.Fatalf("session-start scan: %v", err)
	}
	for _, id := range ids {
		if _, err := store.transact(context.Background(), func(tx *transaction) Object {
			if err := store.startAgentSession(tx, id); err != nil {
				panic(databaseFailure{err})
			}
			return Object{}
		}); err != nil {
			t.Fatalf("start run %s: %v", id, err)
		}
	}
}

// runVersion reads the run's optimistic version, which every issue_runs write in this codebase bumps.
// It is the discriminating witness for "this transaction wrote the row" when the written value would
// otherwise be unchanged.
func runVersion(t *testing.T, store *Store, runID string) int64 {
	t.Helper()
	var v int64
	if err := store.Pool.QueryRow(`SELECT version FROM issue_runs WHERE id=$1`, runID).Scan(&v); err != nil {
		t.Fatalf("read run version: %v", err)
	}
	return v
}

// controllerClaims is the verified service principal the control-plane actions below are driven with.
// The lease seeder always registers the same subject, so the two stay in step.
func controllerClaims() *Claims {
	return &Claims{Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}
}

// seedControllerLease secures the global controller lease every state-changing control action requires.
func seedControllerLease(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(`INSERT INTO controller_leases(name,holder_id,epoch,expires_at)
		VALUES('global','ctrl-a',1,clock_timestamp()+interval '30 minutes')`); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
}

// claimAgentWork claims the run's one declared work item through the real `clone_claim` action and
// returns the work row the controller is handed.
func claimAgentWork(t *testing.T, store *Store, claims *Claims) Object {
	t.Helper()
	pick, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_claim", Body: Object{"epoch": 1}, Service: claims,
	})
	if err != nil {
		t.Fatalf("claim work: %v", err)
	}
	work := pick.O("work")
	if work == nil {
		t.Fatalf("claim must discover the declared agent_session work")
	}
	return work
}

// dispatchableScene is one claimed work item plus the scene identity needed to dispatch and assert it.
type dispatchableScene struct {
	seed sessionScene
	ws   string
	node string
	work Object
	run  string
}

// seedDispatchableWork binds a Controller lease and claims the one declared agent_session work item of
// a freshly started run. Everything before the claim is shipped code: createRunWorkspace,
// startAgentSession and the control plane's own claim read.
func seedDispatchableWork(t *testing.T, store *Store) (dispatchableScene, *Claims) {
	t.Helper()
	scene := seedStartingRun(t, store, sessionStartInput())
	_, nodeID := bindConnectedNode(t, store, scene.ws)
	runSessionStartPass(t, store) // declares one agent_session work item
	seedControllerLease(t, store)
	claims := controllerClaims()
	work := claimAgentWork(t, store, claims)
	return dispatchableScene{seed: scene, ws: scene.ws, node: nodeID, work: work, run: scene.seed.run}, claims
}

// dispatchAgentWork registers one work item through the real `clone_dispatch` action. node/input
// override the work's immutable target/input when empty.
func dispatchAgentWork(t *testing.T, store *Store, claims *Claims, work Object, execution, node string, input Object) (Object, error) {
	t.Helper()
	if node == "" {
		node = work.O("target").S("nodeId")
	}
	if len(input) == 0 {
		input = work.O("input")
	}
	return store.Control(context.Background(), &ControlRequest{
		Action: "clone_dispatch",
		Body: Object{
			"epoch":       1,
			"operationId": work.S("runId"),
			"executionId": execution,
			"nodeId":      node,
			"input":       input,
		},
		Service: claims,
	})
}

// execOf returns the Controller-supplied deterministic execution id used in a scene.
func execOf(work Object, tag string) string { return "exec-" + tag + "-" + work.S("id")[:8] }

// takeoverScene is one registered agent_session execution of a starting run, ready to be taken over.
type takeoverScene struct {
	dispatchableScene
	claims        *Claims
	execution     string
	initialTurnID string
}

// seedTakeoverScene starts a run through Session Start, registers its session execution through the
// control plane's own dispatch action, and ends in exactly the state the Thread's `pending` state
// describes: execution registered, no Node record taken over yet.
func seedTakeoverScene(t *testing.T, store *Store) takeoverScene {
	t.Helper()
	scene, claims := seedDispatchableWork(t, store)
	execution := execOf(scene.work, "a")
	if _, err := dispatchAgentWork(t, store, claims, scene.work, execution, "", nil); err != nil {
		t.Fatalf("register the session execution: %v", err)
	}
	_, _, _, turnID, present := firstTurn(t, store, scene.run)
	id, ok := turnID.(*string)
	if !present || !ok || id == nil {
		t.Fatalf("Session Start must have written the first prompt as seq=1 with a turn id")
	}
	return takeoverScene{dispatchableScene: scene, claims: claims, execution: execution, initialTurnID: *id}
}

// threadRecord builds a settled `ora-history` line as the Node sends it: the HistoryLine's `at` and
// its own `seq`, plus the flattened HistoryRecord with its `type` tag. The Cloud-visible `kind` is
// that tag; the inner `seq` is the Node's line number and stays deliberately unrelated to both the
// Node event sequence and the Cloud-assigned Thread seq.
func threadRecord(tag string, line int64, text string) Object {
	return Object{
		"at":   "2026-09-30T10:00:00+00:00",
		"seq":  line,
		"type": tag,
		"update": Object{
			"sessionUpdate": "agent_message_chunk",
			"content":       Object{"type": "text", "text": text},
		},
	}
}

// userTurnRecord is the user message record the Node writes when it starts a user turn — the only
// record that echoes a turn Cloud wrote. Agent replies in the same turn carry the turn id as well
// (Node protocol D2) and are built with threadRecord.
func userTurnRecord(line int64, text string) Object {
	record := threadRecord("update", line, text)
	record["update"] = Object{"sessionUpdate": "user_message_chunk", "content": Object{"type": "text", "text": text}}
	return record
}

// threadEvent builds one wire ThreadEvent in the canonical shape the control plane's takeover action
// requires: the record travels as the opaque JSON *string* desktop `ora-history` owns, which is why
// the business seam parses it once at the boundary.
func threadEvent(sequence int64, record Object, turnID string) Object {
	return Object{"sequence": sequence, "turnId": turnID, "record": jsonText(record), "truncated": false}
}

// takeOver drives the real Thread takeover control action for one batch.
func takeOver(t *testing.T, store *Store, scene takeoverScene, execution string, events []Object) (Object, error) {
	t.Helper()
	return store.Control(context.Background(), &ControlRequest{
		Action:  "thread_events",
		Body:    Object{"epoch": 1, "operationId": scene.run, "executionId": execution, "events": events},
		Service: scene.claims,
	})
}

// threadSeqs lists the run's Thread entry seqs in order, so a test can assert continuity and the
// absence of duplicates in one comparison.
func threadSeqs(t *testing.T, store *Store, runID string) []int64 {
	t.Helper()
	rows, err := store.Pool.Query(`SELECT seq FROM thread_entries WHERE run_id=$1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("list thread seqs: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan thread seq: %v", err)
		}
		out = append(out, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate thread seqs: %v", err)
	}
	return out
}

// nodeEntry reads the Thread entry a Node record produced, keyed by the business duplicate guard.
func nodeEntry(t *testing.T, store *Store, runID string, sequence int64) (seq int64, source, kind string, record Object, turnID *string, present bool) {
	t.Helper()
	var raw []byte
	err := store.Pool.QueryRow(`SELECT seq,source,kind,record,turn_id FROM thread_entries WHERE run_id=$1 AND node_sequence=$2`, runID, sequence).Scan(&seq, &source, &kind, &raw, &turnID)
	if err != nil {
		return 0, "", "", nil, nil, false
	}
	return seq, source, kind, mustObject(t, raw), turnID, true
}

func countReceipts(t *testing.T, store *Store, execution string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, execution).Scan(&n); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	return n
}

func runThreadState(t *testing.T, store *Store, runID string) sql.NullString {
	t.Helper()
	var state sql.NullString
	if err := store.Pool.QueryRow(`SELECT thread_state FROM issue_runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read thread_state: %v", err)
	}
	return state
}

// countDeleteOperations reports how many delete_workspace operations a run's Workspace holds, which is
// the durable form of "the release declared the delete" now that the declaration goes through the
// control plane's own seam rather than through a test double.
func countDeleteOperations(t *testing.T, store *Store, runID string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`
		SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.issue_run_id=$1 AND o.kind='delete_workspace'`, runID).Scan(&n); err != nil {
		t.Fatalf("count delete operations: %v", err)
	}
	return n
}

// countCommands reports how many Thread commands a run holds, whatever their kind and delivery state.
func countCommands(t *testing.T, store *Store, runID string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("count thread commands: %v", err)
	}
	return n
}

// runWorkspaceID reads the run Workspace a run is bound to, through the control plane's own binding.
func runWorkspaceID(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var wid sql.NullString
	if err := store.Pool.QueryRow(`SELECT id FROM workspaces WHERE issue_run_id=$1`, runID).Scan(&wid); err != nil {
		t.Fatalf("read run workspace: %v", err)
	}
	return wid.String
}
