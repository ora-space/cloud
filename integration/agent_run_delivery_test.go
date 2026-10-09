package integration

// Phase 5 Batch 2 + Cloud Revision acceptance: Delivery → Revision → Release → Done, the second half
// of the Agent IssueRun lifecycle (IssueRun D3/D4/D5, Cloud Revision D1–D4, controller-integration
// D2/D6, operation D4; plan §5 Batch 2).
//
// The chain under test is the production one end to end:
//
//	a session ends             → the run `delivering` + one released deliver_revision work item (Batch 1)
//	clone_claim/clone_dispatch → a registered deliver_revision execution carrying D2's fixed spec
//	GrantRevisionUpload        → one presigned PUT per object key of that attempt's frozen input
//	TakeOverNodeEvent          → the `clone_takeover` action: the local input comparison runs in a
//	                             fence transaction, the object-store HEAD for every declared object
//	                             runs outside any transaction, and the receipt, the durable result,
//	                             the verification verdict, the `revisions` row and the settlement
//	                             then commit in ONE transaction
//	DeliverySettled            → a verified Revision releases the run `releasing` with
//	                             deliveryState=saved|unchanged and the Revision's id; a failure D5 has
//	                             not given up on releases a backoff retry and keeps the run `delivering`
//	the releasing transition   → exactly one delete_workspace operation, declared in the same commit
//	the delete reaching        → RunWorkspaceDeleted moves the run `releasing → done`
//	`succeeded`
//
// The object store is a real HTTP endpoint (revisionStore below), installed as store.ObjectStore:
// internal/objectstore signs a HEAD and the fixture answers it, so what these tests pin is the
// request Cloud actually sends — which object keys it spends a request on, and what a store that
// disagrees with the declaration does to the run — rather than a call into a Go stand-in for the
// client. That a real S3 accepts Cloud's signatures is not this file's concern: revision_test.go and
// revision_sandbox_test.go prove it against a real store (RustFS in CI and in cluster's acceptance).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

// controllerClaims is the lease holder the seeded Thread scenes install (`ctrl-a`), which every
// lease-validated control action in these tests speaks as.
func controllerClaims() *core.Claims {
	return &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}
}

// revisionFailedResult renders one delivery failure addressed to the scene's own Node, so the A
// layer's node-identity check is satisfied the way a real Node's result would be.
func revisionFailedResult(scene liveThreadScene, reason controlpb.RevisionFailureReason) *controlpb.ExecutionResult {
	return &controlpb.ExecutionResult{
		Node:    &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: reason}},
	}
}

// revisionStore is the S3 endpoint Cloud's object-store client talks to (Cloud Revision D1), served
// over real HTTP so the request Cloud signs is the request that is answered. It holds exactly the
// objects deliveredResult/unchangedResult declare, and answers a HEAD for one with that object's own
// size and stored SHA-256 — which is what makes "Cloud verified the right objects" a claim about the
// keys Cloud asked for, since internal/objectstore only reaches a verdict when the metadata comes
// back matching the declaration it was given.
//
// Every probe is recorded for the same reason: *which* objects Cloud spends a request on is an
// obligation in its own right, and a result Cloud can already tell contradicts its own frozen input
// must never reach the store at all (D4 step 1 precedes step 2). A key a case marks as broken is
// answered with metadata that disagrees with the declaration — the store's own verdict, not a
// transport failure — which is the shape D4 step 4 turns into Cloud's `verification_failed`.
type revisionStore struct {
	*httptest.Server
	mu sync.Mutex
	// probes are the objects Cloud asked about, in order; intruders are the requests that were not a
	// HEAD, which is the only way a real store would see Cloud hand out an upload capability.
	probes   []core.Object
	intruder int
	broken   map[string]bool
	config   *objectstore.Config
}

// storedMeasurements answers the size and digest a store that received the Node's upload would report
// for one key. Cloud fixes each key's shape per D2 (`.../revision.bundle`, `.../session.jsonl`), so
// the key itself says which object it is; the values are the ones this file's own result builders
// declare, which is what keeps the fixture's declaration and the store's metadata one fact.
func storedMeasurements(key string) (int64, string) {
	if strings.HasSuffix(key, "/revision.bundle") {
		return revisionBundleSize, revisionBundleSHA
	}
	return revisionHistorySize, revisionHistorySHA
}

func (s *revisionStore) head(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	if req.Method != http.MethodHead {
		s.intruder++
		s.mu.Unlock()
		http.Error(w, "only a HEAD reaches the store on this path", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Unlock()
	key := strings.TrimPrefix(req.URL.Path, "/revisions/")
	size, digest := storedMeasurements(key)
	s.mu.Lock()
	s.probes = append(s.probes, core.Object{"key": key, "size": size, "sha256": digest})
	broken := s.broken[key]
	s.mu.Unlock()
	if broken {
		// One byte more than declared: an object that exists but is not the object Cloud was told to
		// expect. A 404 would be the other definitive verdict, and is what an absent upload produces.
		size++
	}
	raw, e := hex.DecodeString(digest)
	if e != nil {
		http.Error(w, "fixture digest is not hex", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(raw))
	w.WriteHeader(http.StatusOK)
}

// probed returns the objects Cloud verified, in order.
func (s *revisionStore) probed() []core.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Object(nil), s.probes...)
}

// breakObject makes one object's stored metadata disagree with the declaration the Node reported.
func (s *revisionStore) breakObject(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken[key] = true
}

// nonHeadRequests returns how many requests reached the store that were not object probes. An upload
// grant is minted by local signing and never fetched by the fixture, so a non-zero count is the only
// sign that a path other than verification used Cloud's object-store capability.
func (s *revisionStore) nonHeadRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intruder
}

// grantsOf returns the object keys of an upload-grant reply, in the order Cloud signed them.
func grantKeys(grants []*controlpb.UploadGrant) []string {
	keys := make([]string, 0, len(grants))
	for _, g := range grants {
		keys = append(keys, g.GetObjectKey())
	}
	return keys
}

// useObjectStore starts the endpoint and installs its configuration on the store, once per fixture:
// repeating the call returns the same double, so a test that names the store and a helper that also
// needs storage configured cannot end up watching two different endpoints. A Store with a nil
// ObjectStore is the other legal state D1 defines — a deployment without object storage — and the
// tests that need it clear the field explicitly.
func (f *fixture) useObjectStore() *revisionStore {
	f.t.Helper()
	if f.objects != nil {
		f.store.ObjectStore = f.objects.config
		return f.objects
	}
	store := &revisionStore{broken: map[string]bool{}}
	store.Server = httptest.NewServer(http.HandlerFunc(store.head))
	f.t.Cleanup(store.Close)
	store.config = &objectstore.Config{
		Endpoint: store.URL, Region: "us-east-1", Bucket: "revisions", PathStyle: true,
		AccessKeyID: "test-access", SecretAccessKey: "test-secret",
	}
	f.objects = store
	f.store.ObjectStore = store.config
	return store
}

// deliveryTarget reads the target a run's declared delivery attempt carries, which is what a
// re-declaration of the same attempt has to repeat for its refusal to be about anything else.
func (f *fixture) deliveryTarget(runID string) core.Object {
	f.t.Helper()
	var raw string
	must(f.t, f.store.Pool.QueryRow(`SELECT target::text FROM execution_work WHERE run_id=$1 AND kind='deliver_revision'`, runID).Scan(&raw))
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	return out
}

// deliveryInputOf reads the frozen DeliverRevisionSpec one registered attempt carries. Every
// well-formed result below is measured against it rather than against invented keys, because D4 step
// 1 compares the two: a helper with hard-coded object keys could only ever exercise the refusal path.
func (f *fixture) deliveryInputOf(execution string) core.Object {
	f.t.Helper()
	var raw string
	must(f.t, f.store.Pool.QueryRow(`SELECT input::text FROM node_executions WHERE execution_id=$1`, execution).Scan(&raw))
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	return out
}

// deliveredResult renders the success outcome a Node reports after uploading a bundle, measured
// against the attempt's own input: the Revision ref and the baseline it was dispatched with, the two
// keys Cloud assigned it, and a size and digest for each.
func deliveredResult(scene liveThreadScene, in core.Object, final string) *controlpb.ExecutionResult {
	return &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{
			FinalCommit: final, BaseCommit: in.S("baseCommit"), RevisionRef: in.S("revisionRef"),
			Bundle:  &controlpb.StoredObject{Key: in.S("bundleKey"), Size: revisionBundleSize, Sha256: revisionBundleSHA},
			History: &controlpb.StoredObject{Key: in.S("historyKey"), Size: revisionHistorySize, Sha256: revisionHistorySHA},
		}},
	}
}

// unchangedResult renders the outcome for a session that changed nothing: the final commit equals the
// baseline and only the history was uploaded, so the result declares no bundle at all.
func unchangedResult(scene liveThreadScene, in core.Object) *controlpb.ExecutionResult {
	return &controlpb.ExecutionResult{
		Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{
			FinalCommit: in.S("baseCommit"), BaseCommit: in.S("baseCommit"), RevisionRef: in.S("revisionRef"),
			History: &controlpb.StoredObject{Key: in.S("historyKey"), Size: revisionHistorySize, Sha256: revisionHistorySHA},
		}},
	}
}

// The measurements every delivered/unchanged result declares, and the two commits the cases name. A
// digest is 64 hex digits by contract; the values themselves are arbitrary, because nothing in Cloud
// compares a digest against content — the object store does, and the fixture's double answers for it.
const (
	revisionBundleSize  = 4096
	revisionHistorySize = 512
	revisionFinalCommit = "1111111111111111111111111111111111111111"
)

var (
	revisionBundleSHA  = strings.Repeat("a", 64)
	revisionHistorySHA = strings.Repeat("b", 64)
)

// revisionRow is one `revisions` row in its typed column form, or nil when Cloud registered none.
// The columns are read individually rather than as JSON because the assertions compare them against
// the run's own state, and a JSON round trip would turn every bigint into a float64.
type revisionRow struct {
	ID            string
	TenantID      string
	RunID         string
	WorkspaceID   string
	ProjectID     string
	RepositoryURL string
	BaseCommit    string
	FinalCommit   string
	RevisionRef   string
	// The bundle exists exactly when the checkout changed: empty and nil together.
	BundleKey     string
	BundleSize    *int64
	BundleSHA256  string
	HistoryKey    string
	HistorySize   int64
	HistorySHA256 string
	// Always NULL in the first version: expiry and cleanup are an explicit non-goal (D4).
	ExpiresAt *time.Time
}

func (f *fixture) revisionRow(runID string) *revisionRow {
	f.t.Helper()
	if f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, runID) == 0 {
		return nil
	}
	var row revisionRow
	var bundleKey, bundleSHA sql.NullString
	var bundleSize sql.NullInt64
	var expires sql.NullTime
	must(f.t, f.store.Pool.QueryRow(`
		SELECT id::text, tenant_id::text, run_id::text, workspace_id::text, project_id::text,
		       repository_url, base_commit, final_commit, revision_ref,
		       bundle_key, bundle_size, bundle_sha256,
		       history_key, history_size, history_sha256, expires_at
		FROM revisions WHERE run_id=$1`, runID).
		Scan(&row.ID, &row.TenantID, &row.RunID, &row.WorkspaceID, &row.ProjectID,
			&row.RepositoryURL, &row.BaseCommit, &row.FinalCommit, &row.RevisionRef,
			&bundleKey, &bundleSize, &bundleSHA, &row.HistoryKey, &row.HistorySize, &row.HistorySHA256, &expires))
	row.BundleKey, row.BundleSHA256 = bundleKey.String, bundleSHA.String
	if bundleSize.Valid {
		size := bundleSize.Int64
		row.BundleSize = &size
	}
	if expires.Valid {
		at := expires.Time
		row.ExpiresAt = &at
	}
	return &row
}

// receiptEvent reads the event bytes one takeover acknowledged, exactly as the Node sent them. The
// merged control plane stores the Node's event text verbatim in `node_event_receipts.event` (it is
// `text`, not jsonb; the gRPC boundary carries the bytes base64-encoded, so the row does too) and keeps
// what the Node claimed about the result in `node_executions.result`. The decoded bytes are the copy
// Cloud must never rewrite: they are the basis for the EventAck, and they are what a replay is compared
// against — `recordNodeReceipt` refuses the same sequence carrying different bytes.
func (f *fixture) receiptEvent(execution string, sequence int64) string {
	f.t.Helper()
	var event string
	must(f.t, f.store.Pool.QueryRow(`
		SELECT event FROM node_event_receipts
		WHERE execution_id=$1 AND sequence=$2`, execution, sequence).Scan(&event))
	raw, e := base64.StdEncoding.DecodeString(event)
	must(f.t, e)
	return string(raw)
}

// nodeResult reads one execution's durable terminal result, or nil when it holds none. It is the
// Node's own payload as the control plane stored it, so a case can tell it apart from Cloud's verdict
// on the declared objects, which is a `revision_verifications` row and never a rewrite of this one.
func (f *fixture) nodeResult(execution string) core.Object {
	f.t.Helper()
	var raw sql.NullString
	must(f.t, f.store.Pool.QueryRow(`SELECT result::text FROM node_executions WHERE execution_id=$1`, execution).Scan(&raw))
	if !raw.Valid {
		return nil
	}
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw.String), &out))
	return out
}

// revisionVerification reads Cloud's verdict on one delivery execution's declared objects, or nil
// when the control plane recorded none — which is what both a refusal that settled nothing and a
// registration that rolled back leave behind. `outcome` is delivered|unchanged|failed, and `reason`
// is non-null exactly when the outcome is failed (D4 step 4, upstream's `revision_verifications`).
func (f *fixture) revisionVerification(execution string) core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`
		SELECT to_jsonb(v)::text FROM revision_verifications v WHERE v.execution_id=$1`, execution)
	must(f.t, e)
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var raw string
	must(f.t, rows.Scan(&raw))
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	return out
}

// deliveryRows reads the attempt's two durable rows whole, as text: the execution and the work item's
// frozen input. Comparing them as text is what lets "this request wrote nothing" be asserted over
// every column, including ones a later change adds, rather than over a chosen list of fields.
func (f *fixture) deliveryRows(execution string) (executionRow, workInput string) {
	f.t.Helper()
	must(f.t, f.store.Pool.QueryRow(`SELECT to_jsonb(e)::text FROM node_executions e WHERE execution_id=$1`, execution).Scan(&executionRow))
	must(f.t, f.store.Pool.QueryRow(`SELECT to_jsonb(w)::text FROM execution_work w WHERE execution_id=$1`, execution).Scan(&workInput))
	return executionRow, workInput
}

// runPlacement reads the project and repository URL Cloud copies onto a Revision row from the run's
// own Workspace and project — never from the delivered payload, which names neither (D4).
func (f *fixture) runPlacement(runID string) (projectID, repositoryURL string) {
	f.t.Helper()
	must(f.t, f.store.Pool.QueryRow(`
		SELECT p.id, p.repository_url FROM issue_runs r
		JOIN workspaces w ON w.id = r.workspace_id
		JOIN projects p ON p.id = w.project_id
		WHERE r.id=$1`, runID).Scan(&projectID, &repositoryURL))
	return projectID, repositoryURL
}

// grantRevisionUpload asks for the attempt's upload grants over the real gRPC surface as the seeded
// lease holder, which is how a Controller obtains the presigned PUTs a Node uploads through.
func (f *fixture) grantRevisionUpload(execution string) (*controlpb.GrantRevisionUploadResponse, error) {
	return controlpb.NewAgentRunServiceClient(f.controlConn).GrantRevisionUpload(asController("ctrl-a"),
		&controlpb.GrantRevisionUploadRequest{Epoch: 1, ExecutionId: execution})
}

// grantErrOnly narrows one grant request to its error, for a refusal a test asserts on.
func grantErrOnly(f *fixture, execution string) error {
	_, e := f.grantRevisionUpload(execution)
	return e
}

// deliveryTerminal submits one delivery terminal event over the real gRPC surface as the seeded
// lease holder. The canonical event bytes are the caller's label: the A layer stores them verbatim
// and never parses them, so their only job is to make "same sequence, different bytes" detectable.
func (f *fixture) deliveryTerminal(scene liveThreadScene, execution, submission string, sequence uint64, res *controlpb.ExecutionResult, event string) (*controlpb.TakeOverNodeEventResponse, error) {
	return f.executions.TakeOverNodeEvent(asController("ctrl-a"), &controlpb.TakeOverNodeEventRequest{
		SubmissionId: submission, Epoch: 1, OperationId: scene.runID, ExecutionId: execution,
		Sequence: sequence, Result: res, Event: []byte(event),
	})
}

// deliveringScene drives the scene to exactly the state Batch 2 starts from: a live session that
// ended through the production takeover, the run `delivering`, and its delivery work item released.
func deliveringScene(t *testing.T, f *fixture) liveThreadScene {
	t.Helper()
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	if got := len(f.deliveryWork(scene.runID)); got != 1 {
		t.Fatalf("the terminal takeover must release one delivery work item, got %d", got)
	}
	return scene
}

// claimDelivery picks the run's released delivery work item the way a Controller does, through the
// real clone_claim action, and requires it to be a delivery.
func (f *fixture) claimDelivery(t *testing.T, scene liveThreadScene) core.Object {
	t.Helper()
	out, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "clone_claim", Body: core.Object{"epoch": 1}, Service: controllerClaims(),
	})
	must(t, e)
	work := out.O("work")
	if len(work) == 0 {
		t.Fatal("the run must have an unregistered delivery work item to claim")
	}
	if work.S("kind") != "deliver_revision" {
		t.Fatalf("claimed work kind = %q, want deliver_revision", work.S("kind"))
	}
	if work.S("runId") != scene.runID {
		t.Fatalf("claimed work belongs to run %s, want %s", work.S("runId"), scene.runID)
	}
	return work
}

// registerDelivery registers the claimed delivery work item through the real clone_dispatch action,
// which routes a non-clone input to the Node-execution path and is what makes a delivery execution
// addressable by a terminal result. The registered input is echoed back to Cloud unchanged because
// that is the A layer's own comparison: the work item's server-owned keys are frozen when it is
// declared, so a Controller holds them before it dispatches.
func (f *fixture) registerDelivery(t *testing.T, scene liveThreadScene, work core.Object, execution string) {
	t.Helper()
	_, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "clone_dispatch",
		Body: core.Object{
			"operationId": scene.runID, "executionId": execution, "nodeId": scene.nodeID,
			"input": work.O("input"), "epoch": 1,
		},
		Service: controllerClaims(),
	})
	must(t, e)
}

// deliverRevision claims and registers the run's delivery work item, returning the execution id a
// terminal result is addressed to.
func (f *fixture) deliverRevision(t *testing.T, scene liveThreadScene) string {
	t.Helper()
	work := f.claimDelivery(t, scene)
	execution := "exec-delivery-" + work.S("id")[:8]
	f.registerDelivery(t, scene, work, execution)
	return execution
}

// deliveryAttempts reads the run's delivery work items with the timestamps D5's backoff is expressed
// in, oldest first. `backoffSeconds` is the declared delay between the item's declaration and its
// eligibility, which is what makes a retry wait instead of spinning.
func (f *fixture) deliveryAttempts(runID string) []core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`
		SELECT id, input::text AS input, (execution_id IS NOT NULL) AS registered,
		       extract(epoch FROM (available_at - created_at)) AS backoff_seconds,
		       (available_at <= clock_timestamp()) AS available
		FROM execution_work WHERE run_id=$1 AND kind='deliver_revision' ORDER BY created_at, id`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []core.Object
	for rows.Next() {
		var id, input string
		var registered, available bool
		var backoff float64
		must(f.t, rows.Scan(&id, &input, &registered, &backoff, &available))
		o := core.Object{"id": id, "registered": registered, "backoffSeconds": backoff, "available": available}
		must(f.t, json.Unmarshal([]byte(input), &o))
		out = append(out, o)
	}
	must(f.t, rows.Err())
	return out
}

// runPhase reads the run's phase alone, which is all the lifecycle trace needs.
func (f *fixture) runPhase(runID string) string {
	f.t.Helper()
	var phase string
	must(f.t, f.store.Pool.QueryRow(`SELECT phase FROM issue_runs WHERE id=$1`, runID).Scan(&phase))
	return phase
}

// runStatusVersion reads the run's status and version: the two columns a terminal transition must
// leave alone or bump exactly once.
func (f *fixture) runStatusVersion(runID string) (string, int64) {
	f.t.Helper()
	var status string
	var version int64
	must(f.t, f.store.Pool.QueryRow(`SELECT status, version FROM issue_runs WHERE id=$1`, runID).Scan(&status, &version))
	return status, version
}

// runResult reads the run's durable result object (D4's `{revisionId|null, deliveryState}`).
func (f *fixture) runResult(runID string) core.Object {
	f.t.Helper()
	var raw string
	must(f.t, f.store.Pool.QueryRow(`SELECT COALESCE(result,'{}')::text FROM issue_runs WHERE id=$1`, runID).Scan(&raw))
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	return out
}

// runWorkspaceID reads the run's bound Workspace.
func (f *fixture) runWorkspaceID(runID string) string {
	f.t.Helper()
	var wid string
	must(f.t, f.store.Pool.QueryRow(`SELECT COALESCE(workspace_id::text,'') FROM issue_runs WHERE id=$1`, runID).Scan(&wid))
	return wid
}

// workspaceFields reads the Workspace columns the release path writes.
func (f *fixture) workspaceFields(wid string) core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`
		SELECT desired_state, observed_state, admission_open, admission_epoch, owner_user_id, project_id, deleted_at IS NOT NULL
		FROM workspaces WHERE id=$1`, wid)
	must(f.t, e)
	defer rows.Close()
	if !rows.Next() {
		f.t.Fatalf("workspace %s not found", wid)
	}
	var desired, observed, owner, project string
	var open, deleted bool
	var epoch int64
	must(f.t, rows.Scan(&desired, &observed, &open, &epoch, &owner, &project, &deleted))
	return core.Object{
		"desiredState": desired, "observedState": observed, "admissionOpen": open,
		"admissionEpoch": epoch, "ownerUserId": owner, "projectId": project, "deleted": deleted,
	}
}

// runOperations reads the run Workspace's operations oldest first, which is the durable set a
// declaration or a re-declaration must not duplicate.
func (f *fixture) runOperations(runID string) []core.Object {
	f.t.Helper()
	var wid string
	must(f.t, f.store.Pool.QueryRow(`SELECT COALESCE(workspace_id::text,'') FROM issue_runs WHERE id=$1`, runID).Scan(&wid))
	rows, e := f.store.Pool.Query(`
		SELECT id, kind, state, step, actor_user_id, idempotency_key, COALESCE(error_code,'') AS error_code, request::text AS request
		FROM operations WHERE workspace_id=$1 ORDER BY created_at, id`, wid)
	must(f.t, e)
	defer rows.Close()
	var out []core.Object
	for rows.Next() {
		var id, kind, state, step, actor, key, code, request string
		must(f.t, rows.Scan(&id, &kind, &state, &step, &actor, &key, &code, &request))
		out = append(out, core.Object{
			"id": id, "kind": kind, "state": state, "step": step,
			"actorUserId": actor, "idempotencyKey": key, "errorCode": code, "request": request,
		})
	}
	must(f.t, rows.Err())
	return out
}

// deleteOperations narrows runOperations to the delete intents.
func deleteOperations(ops []core.Object) []core.Object {
	var out []core.Object
	for _, o := range ops {
		if o.S("kind") == "delete_workspace" {
			out = append(out, o)
		}
	}
	return out
}

// activityDetails reads the newest issue activity of one action, which is where the release reports
// the two independent outcomes (how the session ended, whether the Revision was saved).
func (f *fixture) activityDetails(issueID, action string) core.Object {
	f.t.Helper()
	var raw string
	must(f.t, f.store.Pool.QueryRow(`
		SELECT COALESCE(details,'{}')::text FROM issue_activities
		WHERE issue_id=$1 AND action=$2 ORDER BY seq DESC LIMIT 1`, issueID, action).Scan(&raw))
	out := core.Object{}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	return out
}

// bindRunWorkspaceSandbox gives the seeded run Workspace's sandbox and Node the external identity a
// real deployment reaches through the create_workspace operation, and returns both row ids.
//
// The seeded scene's Workspace is synthetic: its sandbox and Node exist in Cloud's tables, but the
// Substrate — the durable external journal every effect is dispatched against — never saw them,
// because in production the `sandbox` step's effect creates them. Both fix-ups mirror what that step
// leaves behind:
//
//   - The Substrate journal entry. The `sandbox_ensure` effect's id and the sandbox row's id are the
//     same value (the operation machine draws one id for both), and `sandbox_terminate` looks the
//     sandbox up by the row id, so recording it under that id is what makes a real terminate/cleanup
//     executable rather than a stub. The Substrate's own effect API is used, so the journal's
//     conflict and replay rules apply to it like any other effect.
//   - substrate_sandbox_id and the Node's credential subject. nodeScope requires the former to be
//     set, and a Node credential is only accepted when its subject is the Node row's own
//     service_subject — the identity the `node` step's registration establishes.
func (f *fixture) bindRunWorkspaceSandbox(t *testing.T, runID string) (sandboxID, nodeID string) {
	t.Helper()
	wid := f.runWorkspaceID(runID)
	must(t, f.store.Pool.QueryRow(`
		SELECT s.id, n.id FROM sandbox_instances s JOIN node_instances n ON n.sandbox_instance_id=s.id
		WHERE s.workspace_id=$1 AND s.terminated_at IS NULL AND n.ended_at IS NULL
		ORDER BY s.generation DESC LIMIT 1`, wid).Scan(&sandboxID, &nodeID))
	f.recordSandbox(t, sandboxID, wid)
	_, e := f.store.Pool.Exec(`UPDATE sandbox_instances SET substrate_sandbox_id=id::text WHERE id=$1`, sandboxID)
	must(t, e)
	_, e = f.store.Pool.Exec(`UPDATE node_instances SET service_subject=id::text WHERE id=$1`, nodeID)
	must(t, e)
	return sandboxID, nodeID
}

// substrateRun dispatches one effect to the Substrate's own effect API — the same PUT a Controller's
// operation machine issues — and returns the journal entry. The request is passed verbatim because
// the Substrate validates it before performing anything, so an entry it would not have accepted is
// not a faithful stand-in for one it did.
func (f *fixture) substrateRun(t *testing.T, effect core.Object, wantStatus int) core.Object {
	t.Helper()
	body, e := json.Marshal(effect.O("request"))
	must(t, e)
	req, e := http.NewRequest(http.MethodPut, f.external.URL+"/effects/"+effect.S("id"), bytes.NewReader(body))
	must(t, e)
	req.Header.Set("Content-Type", "application/json")
	resp, e := f.client.HTTP.Do(req)
	must(t, e)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("substrate PUT: want %d got %d", wantStatus, resp.StatusCode)
	}
	var out core.Object
	must(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// recordSandbox stages one Workspace's sandbox in the Substrate's own journal through the same effect
// API a Controller's `sandbox` step uses. The request is shaped exactly like planEffect's
// (`kind`/`projectId`/`workspaceId`) because the Substrate validates those fields before it performs
// anything: the journal entry is what the later `sandbox_terminate` reads back by sandbox id, and an
// entry the Substrate would not have accepted is not a faithful stand-in for one it did. Re-staging
// the same journal entry is a no-op there, so the helper is safe to call more than once.
func (f *fixture) recordSandbox(t *testing.T, sandboxID, wid string) {
	t.Helper()
	var projectID string
	must(t, f.store.Pool.QueryRow(`SELECT project_id FROM workspaces WHERE id=$1`, wid).Scan(&projectID))
	f.substrateRun(t, core.Object{
		"id": sandboxID,
		"request": core.Object{
			"kind": "sandbox_ensure", "projectId": projectID, "workspaceId": wid,
		},
	}, http.StatusOK)
}

// driveDelete hands the run Workspace's operations to the fixture's simulator Controller — the
// production executor of every operation — and drives them to their terminal state.
//
// One handover precedes it: the Thread scenes' seed installs Controller `ctrl-a` as the lease holder
// (every takeover above speaks as it), while the operation path speaks as the fixture's simulator
// Controller, so the lease is re-pointed to it with the epoch that Controller holds. That is exactly
// what a real deployment performs when a Controller takes over, and `leaseValid` matches on the
// holder, so nothing else about the lease changes.
func (f *fixture) driveDelete(t *testing.T, runID string) {
	t.Helper()
	f.bindRunWorkspaceSandbox(t, runID)
	_, e := f.store.Pool.Exec(
		`UPDATE controller_leases SET holder_id=$2, epoch=$1, expires_at=clock_timestamp()+interval '1 hour' WHERE name='global'`,
		f.controller.Epoch, f.client.Subject,
	)
	must(t, e)
	f.drain()
}

// lifecycleOrder is IssueRun D3's phase machine, oldest first. A run may only move to the next entry:
// a jump skips a stage and a move backwards re-opens a closed one, and both are the failures the
// forward-only assertions in this file exist to catch.
var lifecycleOrder = []string{"provisioning", "starting", "running", "delivering", "releasing", "done"}

// assertForwardOnly requires every observed step of a run's phase trace to be the immediate next
// stage of D3's machine, which is the whole "no backward lifecycle, no skipped stage" obligation —
// asserted over the run's own observed history rather than over a list of expected values.
func assertForwardOnly(t *testing.T, trace []string) {
	t.Helper()
	for i := 1; i < len(trace); i++ {
		from, to := trace[i-1], trace[i]
		fromAt, toAt := indexOf(lifecycleOrder, from), indexOf(lifecycleOrder, to)
		if fromAt < 0 || toAt < 0 {
			t.Fatalf("observed phase outside D3's machine: %q → %q (trace %v)", from, to, trace)
		}
		if toAt != fromAt+1 {
			t.Fatalf("the lifecycle must advance exactly one stage: %q → %q (trace %v)", from, to, trace)
		}
	}
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// P5-7, §3/§4 — the released delivery work item is claimable and registrable, its input is the fixed
// DeliverRevision snapshot D2 defines (read from durable Cloud state, never from a Thread identity),
// and registering it moves nothing: registration is not starting the delivery.
func TestDeliveryExecutionClaimCarriesTheFixedSpecAndMovesNothing(t *testing.T) {
	f := setup(t)
	scene := deliveringScene(t, f)
	before := f.runPhase(scene.runID)

	work := f.claimDelivery(t, scene)
	in := work.O("input")
	if got := in.S("kind"); got != "deliver_revision" {
		t.Fatalf("the frozen spec kind = %q, want deliver_revision", got)
	}
	if got := in.S("sessionExecutionId"); got != scene.executionID {
		t.Fatalf("sessionExecutionId = %q, want the ended session execution %q", got, scene.executionID)
	}
	if got := in.S("baseCommit"); got != seedRunWorkspaceCommit {
		t.Fatalf("baseCommit = %q, want the run Workspace's recorded baseline %q", got, seedRunWorkspaceCommit)
	}
	if got := in.S("revisionRef"); got != "refs/ora/revisions/"+scene.runID {
		t.Fatalf("revisionRef = %q, want the name Cloud owns for this run", got)
	}
	// The per-attempt object keys (D2): both live under the run's own prefix, so no two runs and no
	// two attempts can overwrite each other's objects.
	prefix := "revisions/" + scene.tenantID + "/" + scene.runID + "/"
	if got := in.S("bundleKey"); !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, "/revision.bundle") {
		t.Fatalf("bundleKey = %q, want a per-attempt key under %s", got, prefix)
	}
	if got := in.S("historyKey"); !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, "/session.jsonl") {
		t.Fatalf("historyKey = %q, want a per-attempt key under %s", got, prefix)
	}
	// The third segment is the delivery attempt Cloud drew before this work item existed (D2). Both
	// keys name the same one, and it is Cloud's own attempt id rather than the run or the execution:
	// the Controller picks the execution id later, so a key read here cannot contain it.
	attempt := strings.TrimSuffix(strings.TrimPrefix(in.S("bundleKey"), prefix), "/revision.bundle")
	if attempt == "" || attempt == scene.runID {
		t.Fatalf("the attempt segment %q must be Cloud's own attempt id, not the run", attempt)
	}
	if got := strings.TrimSuffix(strings.TrimPrefix(in.S("historyKey"), prefix), "/session.jsonl"); got != attempt {
		t.Fatalf("both object keys must name the same attempt: %q vs %q", got, attempt)
	}
	if got := work.O("target").S("nodeId"); got != scene.nodeID {
		t.Fatalf("the delivery target node = %q, want the run Workspace's Node %q", got, scene.nodeID)
	}

	execution := "exec-delivery-" + work.S("id")[:8]
	f.registerDelivery(t, scene, work, execution)
	// The keys were drawn before any execution existed and are never rewritten to name it: a key that
	// carried the execution id could not have been written into the frozen input at all.
	for _, key := range []string{in.S("bundleKey"), in.S("historyKey")} {
		if strings.Contains(key, execution) {
			t.Fatalf("the per-attempt key %q names the execution id, which did not exist when Cloud drew it", key)
		}
	}
	if got := f.runPhase(scene.runID); got != before {
		t.Fatalf("registering a delivery execution must not move the run off delivering, got %q", got)
	}
	if got := f.scalar(`SELECT count(*) FROM node_executions WHERE execution_id=$1 AND kind='deliver_revision'`, execution); got != 1 {
		t.Fatalf("the dispatch must register exactly one deliver_revision execution, got %d", got)
	}
	if got := f.deliveryWork(scene.runID); len(got) != 1 || !got[0].B("registered") {
		t.Fatalf("the delivery work item must be registered exactly once, got %v", got)
	}
	// With the only work item registered, the claim slot is empty: a Controller can never be handed
	// an execution it has already been given. The raw map is inspected rather than `O()`, which
	// answers with an empty object for an absent key and so cannot express "nothing was claimed".
	out, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "clone_claim", Body: core.Object{"epoch": 1}, Service: controllerClaims(),
	})
	must(t, e)
	if got := out["work"]; got != nil {
		t.Fatalf("a registered delivery must not be claimed twice, got %v", got)
	}
}

// P5-8, §10/§11 — a failed delivery keeps the run `delivering` (the sandbox holds the work the
// Revision needs) and releases exactly one new attempt with D5's backoff. The retry is the same
// logical delivery: same run, same session, same baseline and same Revision ref — only the per-attempt
// object keys differ, so the failed attempt's objects can never be overwritten.
func TestFailedDeliveryKeepsDeliveringAndReleasesOneBackoffRetry(t *testing.T) {
	f := setup(t)
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	first := f.deliveryAttempts(scene.runID)
	rows := f.threadRows(scene.runID)

	// With neither give-up limit configured, the pass must be a no-op: an unconfigured window is "no
	// give-up", never "give up immediately".
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if got := f.runPhase(scene.runID); got != "delivering" {
		t.Fatalf("an unconfigured give-up window must release nothing, got phase %q", got)
	}

	if _, e := f.deliveryTerminal(scene, execution, "p5-delivery-fail", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
		"revision_failed/upload_failed"); e != nil {
		t.Fatalf("the failed delivery result must be taken over: %v", e)
	}

	phase, status, state := f.threadRunState(scene.runID)
	if phase != "delivering" || status != "running" || state != "ended" {
		t.Fatalf("a failed delivery must leave the run delivering/running with its Thread ended, got %s/%s/%s", phase, status, state)
	}
	// The terminal fact is durable even though the delivery did not settle: the receipt is the ack
	// basis and the result is what the retry's spec and the eventual status are read from.
	if got := f.nodeSequence(execution); got != 1 {
		t.Fatalf("the failed result must advance the execution's sequence once, got %d", got)
	}
	if got := f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1 AND sequence=1`, execution); got != 1 {
		t.Fatalf("the failed result must be receipted once, got %d", got)
	}
	if got := fmt.Sprint(f.threadRows(scene.runID)); got != fmt.Sprint(rows) {
		t.Fatalf("a failed delivery must not touch the Thread log, got %s", got)
	}
	if len(first) != 1 {
		t.Fatalf("the scene must hold exactly one delivery attempt before the failure, got %d", len(first))
	}

	attempts := f.deliveryAttempts(scene.runID)
	if len(attempts) != 2 {
		t.Fatalf("a failed delivery must release exactly one retry, got %d attempts", len(attempts))
	}
	if !attempts[0].B("registered") || attempts[1].B("registered") {
		t.Fatalf("only the first attempt may be registered, got %v", attempts)
	}
	if got := attempts[1].N("backoffSeconds"); got < 29 || got > 31 {
		t.Fatalf("D5's first retry must wait 30s, got %ds", got)
	}
	if attempts[1].B("available") {
		t.Fatalf("the released retry must not be claimable before its backoff elapses, got %v", attempts[1])
	}
	// One logical delivery: everything but the per-attempt keys is identical. The frozen input is the
	// control plane's own spelling (proto DeliverRevisionSpec), which is what a Controller reads.
	for _, key := range []string{"sessionExecutionId", "checkoutExecutionId", "baseCommit", "revisionRef"} {
		if attempts[0].S(key) != attempts[1].S(key) {
			t.Fatalf("a retry must not become a new logical delivery: %s changed (%q → %q)", key, attempts[0].S(key), attempts[1].S(key))
		}
	}
	if attempts[0].S("bundleKey") == attempts[1].S("bundleKey") || attempts[0].S("historyKey") == attempts[1].S("historyKey") {
		t.Fatalf("a retry must not reuse the failed attempt's object keys, got %q / %q",
			attempts[0].S("bundleKey"), attempts[0].S("historyKey"))
	}
	// The run stayed delivering, so nothing released it: no delete intent exists yet.
	if got := deleteOperations(f.runOperations(scene.runID)); len(got) != 0 {
		t.Fatalf("a run still delivering must have no delete intent, got %v", got)
	}
}

// P5-9, §9/§15 — D5's two give-up limits release the run: `delivering → releasing` with
// `deliveryState=failed`, D4's status derived from the session's own end reason, and a Timeline
// activity that reports the two outcomes independently. `done` is never reached here, and the status
// is never read off the delivery outcome.
func TestDeliveryGiveUpReleasesTheRunWithD5StateAndD4Status(t *testing.T) {
	cases := []struct {
		name   string
		reason controlpb.AgentSessionEndReason
		status string
		limit  func(f *fixture)
		// age reports whether the case reaches D5's limit through the run Workspace's Node heartbeat
		// rather than through elapsed time: the unreachability window is measured from the Node's last
		// report, so the case has to move that instant.
		age bool
	}{
		{
			// D5's continuous-failure window: the run has been failing to deliver for longer than the
			// configured window, so the next terminal failure releases it.
			name: "continuous failure window", reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, status: "completed",
			limit: func(f *fixture) { f.store.DeliveryGiveUpAfter = time.Nanosecond },
		},
		{
			// D5's unreachability window: the Workspace's Node has been unknown long enough that
			// waiting out the full failure window would only hold the sandbox longer.
			name: "Workspace unreachable", reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, status: "completed",
			limit: func(f *fixture) { f.store.DeliveryUnreachableAfter = time.Nanosecond }, age: true,
		},
		{
			// D4's status is derived from the session end reason, not from the delivery: an Agent that
			// failed ends `failed` even though the delivery path is what releases the run.
			name: "failed session", reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_AGENT_FAILED, status: "failed",
			limit: func(f *fixture) { f.store.DeliveryGiveUpAfter = time.Nanosecond },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedLiveThreadScene(t, f)
			scene.start(t, f)
			f.runningThread(t, scene)
			f.sessionEndOK(t, scene, "", 2, c.reason)
			execution := f.deliverRevision(t, scene)
			c.limit(f)
			if c.age {
				// The Node's heartbeat is the run Workspace's only evidence of liveness; one older
				// than the window is exactly the "state unknown" D5 measures. A terminal result can
				// no longer be taken over once the Node is that stale — the control plane's own fence
				// refuses events from a Node it cannot reach — so Cloud applies this window on its own
				// clock, through the give-up pass, rather than on an event the Node cannot deliver.
				must(t, f.ageRunWorkspaceNode(scene.runID))
				must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
			} else if _, e := f.deliveryTerminal(scene, execution, "p5-give-up", 1,
				revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
				"revision_failed/upload_failed"); e != nil {
				t.Fatalf("the give-up result must be taken over: %v", e)
			}

			phase, status, state := f.threadRunState(scene.runID)
			if phase != "releasing" || state != "ended" {
				t.Fatalf("the give-up must release the run, got phase=%q thread=%q", phase, state)
			}
			if status != c.status {
				t.Fatalf("status must come from the session end reason: got %q want %q", status, c.status)
			}
			result := f.runResult(scene.runID)
			if got := result.S("deliveryState"); got != "failed" {
				t.Fatalf("deliveryState = %q, want failed", got)
			}
			// D4 spells the result `{revisionId | null, deliveryState}`: an absent key would be
			// indistinguishable from a Revision whose id Cloud failed to record.
			if v, ok := result["revisionId"]; !ok || v != nil {
				t.Fatalf("revisionId must be an explicit null on the release path, got %v (present=%v)", v, ok)
			}
			details := f.activityDetails(scene.issueID, "run."+c.status)
			if got := details.S("deliveryState"); got != "failed" {
				t.Fatalf("the timeline activity must report the delivery outcome, got %v", details)
			}
			if got := details.S("sessionEndReason"); got != endReasonName(c.reason) {
				t.Fatalf("the timeline activity must report the session end reason, got %v", details)
			}
			// The released retry the earlier failure had declared is not a reason to hold the run: the
			// release is the end of the delivery, and no second attempt is declared.
			if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
				t.Fatalf("the give-up must not release another attempt, got %d", got)
			}
			if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
				t.Fatalf("the release must declare exactly one delete intent, got %d", got)
			}
		})
	}
}

// endReasonName is the stored snake_case spelling the release path derives D4's status from.
func endReasonName(r controlpb.AgentSessionEndReason) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "AGENT_SESSION_END_REASON_"))
}

// ageRunWorkspaceNode pushes the run Workspace's Node heartbeat past any meaningful window, which is
// the durable form of "the Node's state is unknown".
func (f *fixture) ageRunWorkspaceNode(runID string) error {
	f.t.Helper()
	return f.ageRunWorkspaceNodeBy(runID, time.Hour)
}

// P5-9 continued, §10 — the same give-up is reached by Cloud's own clock when no Node ever reports
// again: the background pass applies D5's limits to a run the delivery path already left delivering,
// so a lost or endlessly failing Node cannot hold a Workspace forever.
func TestGiveUpStaleDeliveriesPassReleasesOnItsOwnClock(t *testing.T) {
	f := setup(t)
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	if _, e := f.deliveryTerminal(scene, execution, "", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
		"revision_failed/upload_failed"); e != nil {
		t.Fatalf("the failed delivery result must be taken over: %v", e)
	}
	if got := f.runPhase(scene.runID); got != "delivering" {
		t.Fatalf("with no limits configured the run must keep delivering, got %q", got)
	}

	f.store.DeliveryGiveUpAfter = time.Nanosecond
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("the give-up pass must release the stale delivery, got phase %q", got)
	}
	// The whole of D5's settlement, in the three places it is observable: the run's decision, the
	// absence of a Revision, and the release's own delete. Nothing about it is `skipped` — that spelling
	// is reserved for D6's sessionless cancellation, and a delivery that failed must never borrow it.
	result := f.runResult(scene.runID)
	if got := result.S("deliveryState"); got != "failed" {
		t.Fatalf("the pass must record deliveryState=failed, got %q", got)
	}
	if v, ok := result["revisionId"]; !ok || v != nil {
		t.Fatalf("an abandoned delivery must record an explicit null revisionId, got %v (present=%v)", v, ok)
	}
	if row := f.revisionRow(scene.runID); row != nil {
		t.Fatalf("an abandoned delivery must register no Revision, got %v", row)
	}
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
		t.Fatalf("the pass must declare exactly one delete intent, got %d", got)
	}
	// A second tick changes nothing: the run has left `delivering`, so it is out of the scan.
	version := f.runVersion(scene.runID)
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("a repeated tick must not release the run twice: version %d → %d", version, got)
	}
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
		t.Fatalf("a repeated tick must not declare a second delete, got %d", got)
	}
}

// runVersion reads the run's optimistic-concurrency version, the cheapest witness of "this commit
// wrote nothing".
func (f *fixture) runVersion(runID string) int64 {
	f.t.Helper()
	var version int64
	must(f.t, f.store.Pool.QueryRow(`SELECT version FROM issue_runs WHERE id=$1`, runID).Scan(&version))
	return version
}

// givingUpScene is P5-8's end state promoted to a starting point: the run released (`releasing`),
// `deliveryState=failed`, with its delete intent declared. It is the D5 settlement with the ordinary
// session ending; the D8 suite's driveToReleasing builds the same state for any outcome.
func givingUpScene(t *testing.T, f *fixture) (liveThreadScene, string) {
	t.Helper()
	run := driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	return run.liveThreadScene, run.execution
}

// P5-10, §12 — the releasing transition declares the run Workspace's delete through the approved
// seam, in the same commit, with the identity and actor the operation contract fixes. The delete is
// declared, never executed: no transaction reaches a workspace provider.
func TestReleasingDeclaresExactlyOneDeleteOperation(t *testing.T) {
	f := setup(t)
	scene, execution := givingUpScene(t, f)

	ops := deleteOperations(f.runOperations(scene.runID))
	if len(ops) != 1 {
		t.Fatalf("the release must declare exactly one delete_workspace operation, got %d (%v)", len(ops), ops)
	}
	op := ops[0]
	if op.S("state") != "queued" || op.S("step") != "quiesce" {
		t.Fatalf("the declared delete must be a fresh queued quiesce, got state=%q step=%q", op.S("state"), op.S("step"))
	}
	wid := f.runWorkspaceID(scene.runID)
	ws := f.workspaceFields(wid)
	if op.S("actorUserId") != ws.S("ownerUserId") {
		t.Fatalf("the delete must act as the Workspace's owner %q, got %q", ws.S("ownerUserId"), op.S("actorUserId"))
	}
	// Operation D4's identity for a run-Workspace operation is (issue_run_id, kind), generated by
	// Cloud: the Workspace binds one run uniquely, so its id is the same set, and the key must never
	// come from a caller-supplied idempotency key. The spelling is the seam's own run-scoped one,
	// paired with the create key the same seam declares, so a retried declaration converges.
	if got := op.S("idempotencyKey"); got != "run-workspace-"+scene.runID+"-delete" {
		t.Fatalf("idempotency key = %q, want Cloud's own run-scoped key", got)
	}
	if !strings.Contains(op.S("request"), wid) {
		t.Fatalf("the delete request must carry the Workspace snapshot a refusal restores from, got %s", op.S("request"))
	}
	// Admission is closed by the declaration, not by the operation's first step.
	if ws.B("admissionOpen") || ws.S("desiredState") != "deleted" {
		t.Fatalf("the declaration must close admission for deletion, got %v", ws)
	}

	// A delivery execution settles once: the takeover writes its terminal result together with the
	// receipt that accompanies it, and the A layer then accepts only an exact replay of the committed
	// event. Both other shapes a Controller's recovery can produce are refused as conflicts with a
	// durable fact — nothing new may be appended to a settled execution, and no second result may
	// overwrite the settled one. Neither may write the run, move the phase or declare a second delete.
	version := f.runVersion(scene.runID)
	if _, e := f.deliveryTerminal(scene, execution, "", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
		"revision_failed/upload_failed"); e != nil {
		t.Fatalf("an exact replay of the committed delivery event must be a no-op: %v", e)
	}
	refusals := []struct {
		what     string
		sequence uint64
		result   *controlpb.ExecutionResult
		event    string
	}{
		{
			"a later sequence restating the settled result", 2,
			revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED), "revision_failed/upload_failed",
		},
		{
			"a different result at the settled sequence", 1,
			revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_CHECKOUT_UNAVAILABLE), "revision_failed/checkout_unavailable",
		},
	}
	for _, c := range refusals {
		if _, e := f.deliveryTerminal(scene, execution, "", c.sequence, c.result, c.event); e == nil {
			t.Fatalf("%s must be refused: a settled delivery execution accepts only a replay of the committed event", c.what)
		} else {
			expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
		}
	}
	if got := f.nodeSequence(execution); got != 1 {
		t.Fatalf("a refused post-release event must not be receipted, got sequence %d", got)
	}
	if got := len(f.receipts(execution)); got != 1 {
		t.Fatalf("a refused post-release event must not add a receipt, got %v", got)
	}
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("a post-release delivery result must not write the run: version %d → %d", version, got)
	}
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("a post-release delivery result must not move the phase, got %q", got)
	}
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
		t.Fatalf("a post-release delivery result must not declare a second delete, got %d", got)
	}
}

// P5-11, §13/§14 — the delete operation's terminal state is what settles the run, in the same
// transaction: a *succeeded* delete with the run still releasing is the state the whole hook exists
// to prevent. `done` then carries the status D4 derived, never a status of its own.
func TestDeleteTerminalStateSettlesTheRunToDone(t *testing.T) {
	f := setup(t)
	scene, _ := givingUpScene(t, f)
	wid := f.runWorkspaceID(scene.runID)
	status, version := f.runStatusVersion(scene.runID)

	f.driveDelete(t, scene.runID)

	ops := deleteOperations(f.runOperations(scene.runID))
	if len(ops) != 1 || ops[0].S("state") != "succeeded" || ops[0].S("step") != "done" {
		t.Fatalf("the delete operation must reach succeeded/done, got %v", ops)
	}
	if got := f.runPhase(scene.runID); got != "done" {
		t.Fatalf("a succeeded delete must settle the run to done, got %q", got)
	}
	gotStatus, gotVersion := f.runStatusVersion(scene.runID)
	if gotStatus != status {
		t.Fatalf("`done` must not rewrite the status D4 derived: got %q want %q", gotStatus, status)
	}
	if gotVersion <= version {
		t.Fatalf("the terminal transition must advance the run's version, got %d (before %d)", gotVersion, version)
	}
	// The Workspace the run held is gone, and nothing may be started in it again.
	ws := f.workspaceFields(wid)
	if !ws.B("deleted") || ws.B("admissionOpen") {
		t.Fatalf("the run Workspace must be deleted with admission closed, got %v", ws)
	}
	// The two recovery passes are no-ops on a finished run: `done` is out of both scans.
	done := f.runVersion(scene.runID)
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
	if got := f.runVersion(scene.runID); got != done {
		t.Fatalf("a finished run must be inert: version %d → %d", done, got)
	}
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
		t.Fatalf("a finished run must not gain a second delete, got %d", got)
	}
}

// P5-12, §3/§6/§16 — the whole mainline in one run: session end → delivery claim/registration →
// delivery settlement → release → delete → done, with every phase observed in order and none
// skipped. This is the batch's end-to-end acceptance: the phases the approved ADRs define are all
// reachable through production code, in one uninterrupted trace.
func TestFullLifecycleSessionEndToDoneSkipsNoPhase(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	f.useObjectStore()
	scene := seedLiveThreadScene(t, f)
	trace := []string{f.runPhase(scene.runID)}
	if trace[0] != "starting" {
		t.Fatalf("the seeded run must start in `starting`, got %q", trace[0])
	}

	scene.start(t, f)
	// The session declaration alone does not run the session: the first taken-over Node record is
	// what moves the run (D-017/D-019), and that is the stage boundary asserted here.
	if got := f.runPhase(scene.runID); got != "starting" {
		t.Fatalf("declaring a session must not run it, got phase %q", got)
	}
	f.runningThread(t, scene)
	trace = append(trace, f.runPhase(scene.runID))
	if got := f.runPhase(scene.runID); got != "running" {
		t.Fatalf("the first taken-over record must run the session, got %q", got)
	}

	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	trace = append(trace, f.runPhase(scene.runID))
	execution := f.deliverRevision(t, scene)

	// The delivery succeeds the way the contract defines it: the Node reports a Revision and Cloud
	// confirms the objects it declared before registering the row. The success is what releases the
	// run here — the give-up path is D5's fallback, not the mainline.
	in := f.deliveryInputOf(execution)
	if _, e := f.deliveryTerminal(scene, execution, "", 1,
		deliveredResult(scene, in, revisionFinalCommit), "revision_delivered"); e != nil {
		t.Fatalf("the verified delivery must be taken over: %v", e)
	}
	trace = append(trace, f.runPhase(scene.runID))

	f.driveDelete(t, scene.runID)
	trace = append(trace, f.runPhase(scene.runID))

	want := []string{"starting", "running", "delivering", "releasing", "done"}
	if fmt.Sprint(trace) != fmt.Sprint(want) {
		t.Fatalf("the mainline must visit every stage in order:\n got %v\nwant %v", trace, want)
	}
	assertForwardOnly(t, trace)

	// And the end state is the whole end state: Thread ended, the Revision saved and named by the
	// run's own result, status from the session's own end reason, Workspace released.
	phase, status, state := f.threadRunState(scene.runID)
	if phase != "done" || status != "completed" || state != "ended" {
		t.Fatalf("terminal state = %s/%s/%s, want done/completed/ended", phase, status, state)
	}
	row := f.revisionRow(scene.runID)
	if row == nil {
		t.Fatal("the mainline must end with the run's Revision registered")
	}
	result := f.runResult(scene.runID)
	if got := result.S("deliveryState"); got != "saved" {
		t.Fatalf("deliveryState = %q, want saved", got)
	}
	// D4 spells the run's result `{revisionId | null, deliveryState}`; the null is reserved for the
	// give-up path, so a saved delivery must name the row it registered.
	if got := result.S("revisionId"); got != row.ID {
		t.Fatalf("revisionId = %q, want the registered Revision %q", got, row.ID)
	}
	if got := deleteOperations(f.runOperations(scene.runID)); len(got) != 1 || got[0].S("state") != "succeeded" {
		t.Fatalf("the run must end with one succeeded delete, got %v", got)
	}
}

// P5-13, §8/§11/§16 — the delivery mainline of Cloud Revision D4: a Node reports a Revision and Cloud
// confirms the objects it declared, registers the row, releases the run and declares the delete — all
// in the one takeover transaction, with the two object checks that precede it.
//
// Both wire shapes are covered, because they differ in the one thing the row has to get right: a
// delivered checkout carries a bundle, an unchanged one carries none and its final commit IS the
// baseline it was dispatched with.
func TestRevisionDeliveredIsVerifiedRegisteredAndReleasesTheRun(t *testing.T) {
	cases := []struct {
		name          string
		outcome       string
		deliveryState string
		bundle        bool
		result        func(liveThreadScene, core.Object) *controlpb.ExecutionResult
		// verified are the objects Cloud must spend a HEAD on, in the merged control plane's own
		// order: the history first, because every outcome declares one, and the bundle second, only
		// for a changed checkout. D4 fixes the *set* of objects to verify ("对声明的每个对象调用
		// HEAD"), not the sequence, so what this pins is which keys Cloud asks about.
		verified func(core.Object) []core.Object
	}{
		{
			name: "a changed checkout declares the bundle and the history", outcome: "revision_delivered",
			deliveryState: "saved", bundle: true,
			result: func(s liveThreadScene, in core.Object) *controlpb.ExecutionResult {
				return deliveredResult(s, in, revisionFinalCommit)
			},
			verified: func(in core.Object) []core.Object {
				return []core.Object{
					{"key": in.S("historyKey"), "size": int64(revisionHistorySize), "sha256": revisionHistorySHA},
					{"key": in.S("bundleKey"), "size": int64(revisionBundleSize), "sha256": revisionBundleSHA},
				}
			},
		},
		{
			name: "an unchanged checkout declares only the history", outcome: "revision_unchanged",
			deliveryState: "unchanged",
			result:        unchangedResult,
			verified: func(in core.Object) []core.Object {
				return []core.Object{{"key": in.S("historyKey"), "size": int64(revisionHistorySize), "sha256": revisionHistorySHA}}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			objects := f.useObjectStore()
			scene := deliveringScene(t, f)
			execution := f.deliverRevision(t, scene)
			in := f.deliveryInputOf(execution)
			before := f.runVersion(scene.runID)

			if _, e := f.deliveryTerminal(scene, execution, "p5-revision-saved", 1, c.result(scene, in), c.outcome); e != nil {
				t.Fatalf("a verified delivery must be taken over: %v", e)
			}

			// Step 2 ran before the transaction opened, over exactly the objects the result declares —
			// and only those: the grants path is a different action and must not have run.
			if got, want := fmt.Sprint(objects.probed()), fmt.Sprint(c.verified(in)); got != want {
				t.Fatalf("Cloud must verify exactly the declared objects:\n got %s\nwant %s", got, want)
			}
			// Signing a grant is local cryptography and sends nothing, so the store's only requests are
			// the probes above: the takeover never asked the store for an upload capability, and the
			// grant action is the only path that could have.
			if got := objects.nonHeadRequests(); got != 0 {
				t.Fatalf("the store must see probes only, got %d other requests", got)
			}

			// Step 3: one row per run, carrying the run's own placement and the attempt's own input.
			row := f.revisionRow(scene.runID)
			if row == nil {
				t.Fatal("a verified delivery must register a Revision")
			}
			if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 1 {
				t.Fatalf("one logical delivery registers exactly one Revision, got %d", got)
			}
			wid := f.runWorkspaceID(scene.runID)
			projectID, repositoryURL := f.runPlacement(scene.runID)
			if row.TenantID != scene.tenantID || row.RunID != scene.runID {
				t.Fatalf("the Revision must belong to the run's own tenant and run, got tenant=%s run=%s", row.TenantID, row.RunID)
			}
			if row.WorkspaceID != wid || row.ProjectID != projectID || row.RepositoryURL != repositoryURL {
				t.Fatalf("the Revision must carry the run Workspace's placement: got ws=%s project=%s repo=%q, want %s/%s/%q",
					row.WorkspaceID, row.ProjectID, row.RepositoryURL, wid, projectID, repositoryURL)
			}
			if row.BaseCommit != in.S("baseCommit") || row.RevisionRef != in.S("revisionRef") {
				t.Fatalf("the Revision must carry the attempt's own baseline and ref, got %s/%s", row.BaseCommit, row.RevisionRef)
			}
			// Invariant 5: the session history is saved for every Revision, including an unchanged one.
			if row.HistoryKey != in.S("historyKey") || row.HistorySHA256 != revisionHistorySHA || row.HistorySize != revisionHistorySize {
				t.Fatalf("the history measured by the Node must be recorded verbatim, got %v", row)
			}
			if row.ExpiresAt != nil {
				t.Fatalf("expiry is an explicit non-goal: expires_at must stay NULL, got %v", row.ExpiresAt)
			}
			if c.bundle {
				if row.BundleKey != in.S("bundleKey") || row.BundleSHA256 != revisionBundleSHA {
					t.Fatalf("a changed checkout must record the bundle it declared, got %v", row)
				}
				if row.BundleSize == nil || *row.BundleSize != revisionBundleSize {
					t.Fatalf("bundle_size = %v, want %d", row.BundleSize, revisionBundleSize)
				}
				if row.FinalCommit != revisionFinalCommit {
					t.Fatalf("final_commit = %q, want the delivered commit %q", row.FinalCommit, revisionFinalCommit)
				}
			} else {
				// The bundle columns are all-or-nothing and NULL together when nothing changed, so "no
				// bundle" is a fact of the row rather than a sentinel size or an empty digest.
				if row.BundleKey != "" || row.BundleSHA256 != "" || row.BundleSize != nil {
					t.Fatalf("an unchanged delivery must record no bundle, got %v", row)
				}
				if row.FinalCommit != in.S("baseCommit") {
					t.Fatalf("an unchanged delivery's final commit IS the baseline, got %q", row.FinalCommit)
				}
			}

			// The settlement: phase, D4's result pair and D3's status, all from the same transaction.
			phase, status, state := f.threadRunState(scene.runID)
			if phase != "releasing" || status != "completed" || state != "ended" {
				t.Fatalf("a saved delivery must release the run, got %s/%s/%s", phase, status, state)
			}
			result := f.runResult(scene.runID)
			if got := result.S("revisionId"); got != row.ID {
				t.Fatalf("revisionId = %q, want the Revision this transaction registered %q", got, row.ID)
			}
			if got := result.S("deliveryState"); got != c.deliveryState {
				t.Fatalf("deliveryState = %q, want %q", got, c.deliveryState)
			}
			if got := f.runVersion(scene.runID); got <= before {
				t.Fatalf("the release must advance the run's version, got %d (before %d)", got, before)
			}
			// The durable result is the Node's own payload: nothing was rewritten, because everything it
			// declared was confirmed.
			if got := f.nodeResult(execution).S("outcome"); got != c.outcome {
				t.Fatalf("a verified delivery must be recorded as reported, got outcome %q want %q", got, c.outcome)
			}
			if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
				t.Fatalf("a settled delivery must release no retry, got %d attempts", got)
			}
			// §12/§17: the delete is declared in the releasing transition's own transaction, and D4's
			// timeline activity reports the two independent outcomes.
			ops := deleteOperations(f.runOperations(scene.runID))
			if len(ops) != 1 || ops[0].S("state") != "queued" || ops[0].S("step") != "quiesce" {
				t.Fatalf("the release must declare exactly one delete intent, got %v", ops)
			}
			details := f.activityDetails(scene.issueID, "run.completed")
			if got := details.S("deliveryState"); got != c.deliveryState {
				t.Fatalf("the timeline activity must report the delivery outcome, got %v", details)
			}
			if got := details.S("sessionEndReason"); got != "user_ended" {
				t.Fatalf("the timeline activity must report the session end reason, got %v", details)
			}
		})
	}
}

// P5-14, §8/§16/§31 — when Cloud cannot confirm the declared objects it records its OWN verdict, never
// the Node's claim (D4 step 4): the delivery is settled as `failed{verification_failed}`, no Revision
// row exists, and the run stays `delivering` under D5's retry policy. That is the whole reason
// `verification_failed` is outside the wire's closed set — a Node may not assert a verdict about
// objects Cloud has not looked at.
//
// The merged control plane keeps the two facts apart in the schema rather than by rewriting one:
// `node_executions.result` stays exactly what the Node reported, and Cloud's verdict is its own row
// in `revision_verifications` (upstream's table: `outcome` in delivered|unchanged|failed, with
// `reason` non-null exactly when the outcome is failed). The settlement the B side sees is built from
// that verdict, so the run still retries. Asserting the durable result *still holds the Node's
// payload* is therefore part of the contract here and not a detail: it is what makes an exact replay
// of the original payload compare equal and stay a no-op.
//
// The store's verdict is definitive here, not a transport failure: it holds every declared object but
// answers for the bundle with metadata that disagrees with the declaration, so the object Cloud was
// told to expect is not the object that is there. A store that is merely unreachable is the other
// case, and is a refusal that settles nothing (see the transient test below).
func TestRevisionVerificationFailureIsCloudsOwnVerdict(t *testing.T) {
	f := setup(t)
	objects := f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	objects.breakObject(in.S("bundleKey"))

	res := deliveredResult(scene, in, revisionFinalCommit)
	if _, e := f.deliveryTerminal(scene, execution, "p5-verification-failed", 1, res, "revision_delivered"); e != nil {
		t.Fatalf("an unverifiable delivery must be taken over as Cloud's own verdict: %v", e)
	}

	if row := f.revisionRow(scene.runID); row != nil {
		t.Fatalf("no Revision may be registered for objects Cloud could not confirm, got %v", row)
	}
	verdict := f.revisionVerification(execution)
	if verdict == nil {
		t.Fatal("Cloud's verdict on the declared objects must be recorded in revision_verifications")
	}
	if verdict.S("outcome") != "failed" || verdict.S("reason") != "verification_failed" {
		t.Fatalf("Cloud's verdict must be failed{verification_failed}, got %v", verdict)
	}
	stored := f.nodeResult(execution)
	if stored.S("outcome") != "revision_delivered" {
		t.Fatalf("the durable result must stay the Node's own payload, with Cloud's verdict beside it, got %v", stored)
	}
	// The receipt keeps the Node's bytes untouched: it is the basis for the EventAck, and it is what the
	// replay below is compared against.
	if got := f.receiptEvent(execution, 1); got != "revision_delivered" {
		t.Fatalf("the receipt must keep the Node's own event bytes, got %q", got)
	}
	if got := f.nodeSequence(execution); got != 1 {
		t.Fatalf("the verdict must be receipted once, got sequence %d", got)
	}

	// A failure Cloud recorded is a failure the delivery retries (D5): the run keeps its sandbox
	// and gets one new attempt with D5's backoff — it is not released.
	if got := f.runPhase(scene.runID); got != "delivering" {
		t.Fatalf("a failed verification must keep the run delivering, got %q", got)
	}
	if got := f.runResult(scene.runID).S("deliveryState"); got != "" {
		t.Fatalf("a run still delivering must have decided no delivery state, got %q", got)
	}
	if got := len(f.deliveryAttempts(scene.runID)); got != 2 {
		t.Fatalf("a failed verification must release exactly one retry, got %d attempts", got)
	}
	if got := deleteOperations(f.runOperations(scene.runID)); len(got) != 0 {
		t.Fatalf("a run still delivering must have no delete intent, got %v", got)
	}

	// The Node's retry loop is bounded by the receipt: replaying the very payload that was
	// rejected is a deterministic no-op, not a conflict against the rewritten durable result.
	if _, e := f.deliveryTerminal(scene, execution, "", 1, res, "revision_delivered"); e != nil {
		t.Fatalf("an exact replay of a rewritten result must be a no-op, not a fault: %v", e)
	}
	if got := f.nodeSequence(execution); got != 1 {
		t.Fatalf("a replay must not advance the sequence, got %d", got)
	}
	if got := f.receipts(execution); len(got) != 1 {
		t.Fatalf("a replay must not write a receipt, got %v", got)
	}
	if got := len(f.deliveryAttempts(scene.runID)); got != 2 {
		t.Fatalf("a replay must not release a second attempt, got %d", got)
	}
}

// P5-14b, §4/§8 — a store Cloud cannot reach is a different verdict from a store that disagrees. The
// endpoint is contacted between two transactions, so a delivery result that arrives while the store is
// down must be refused as transient: nothing is settled, nothing is receipted, and the Node's next
// attempt with the very same submission is a clean first request. Recording a failure instead would
// turn an outage into durable evidence that the objects are wrong, and D5 would then retry a delivery
// whose objects nobody ever looked at.
//
// The store is removed after the delivery was dispatched, which is what an outage mid-delivery looks
// like from Cloud's side: the work item and its frozen keys were drawn while storage was healthy.
func TestDeliveryWithAnUnreachableObjectStoreIsRefusedTransiently(t *testing.T) {
	f := setup(t)
	objects := f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	res := deliveredResult(scene, in, revisionFinalCommit)
	before := f.runVersion(scene.runID)

	f.store.ObjectStore = nil
	_, e := f.deliveryTerminal(scene, execution, "p5-store-down", 1, res, "revision_delivered")
	expectStatus(t, e, codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNAVAILABLE)
	// Nothing was written: no verdict, no durable result, no receipt, no Revision, no retry. In
	// particular the run's status is untouched, so the refusal did not move the lifecycle either.
	if got := f.nodeResult(execution); len(got) != 0 {
		t.Fatalf("a refused verification must write no durable result, got %v", got)
	}
	if got := f.receipts(execution); len(got) != 0 {
		t.Fatalf("a refused verification must write no receipt, got %v", got)
	}
	if row := f.revisionRow(scene.runID); row != nil {
		t.Fatalf("a refused verification must register no Revision, got %v", row)
	}
	if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
		t.Fatalf("a refusal that settled nothing must release no retry, got %d attempts", got)
	}
	if got := f.runVersion(scene.runID); got != before {
		t.Fatalf("a refused verification must not touch the run, version %d want %d", got, before)
	}
	// The store never saw a request at all: Cloud short-circuits on the missing configuration rather
	// than signing a probe it knows cannot be answered.
	if got := objects.probed(); len(got) != 0 {
		t.Fatalf("an unconfigured store must not be contacted, got %v", got)
	}

	// The same submission, unchanged, is a first request once storage is reachable again — the refusal
	// left no trace for it to conflict with.
	f.store.ObjectStore = objects.config
	if _, e := f.deliveryTerminal(scene, execution, "p5-store-down", 1, res, "revision_delivered"); e != nil {
		t.Fatalf("the retry after the store came back must settle the delivery: %v", e)
	}
	if row := f.revisionRow(scene.runID); row == nil {
		t.Fatal("the retry must register the Revision")
	}
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("the retry must release the run, got phase %q", got)
	}
}

// P5-17, §8/§16 — the local input comparison is I/O-free and it precedes the object-store probes, so a
// result Cloud can already tell contradicts its own frozen input never reaches the object store: the
// HEADs would be requests against a store holding objects Cloud has already decided it will not
// register.
//
// The merged control plane answers this contradiction as a *refusal* rather than as a settlement. The
// approved Revision ADR D4 step 1/4 asks for the event to be taken over and the delivery recorded as
// `failed{verification_failed}` on this path too, and upstream's authoritative control plane does not
// do that: `validateDeliveryDeclaration` runs inside the fence transaction of `prepareRevision` and
// rejects the call — 409 `result_conflict` for a field that disagrees with the input or with the
// declared shape, 400 `invalid_result` for a malformed stored-object declaration — so nothing is
// receipted, nothing is settled and no verdict row is written. This test pins what the merged service
// actually does. The obligations that survive are the ones it asserts: the store is never contacted,
// no Revision is registered, nothing at all commits (no receipt, no durable result, no
// `revision_verifications` row, no retry, no run write), and the refusal is deterministic on replay.
//
// The divergence is reported rather than papered over. It is inherited from upstream `main` — the
// step-1 verdict lived in functionB's own A side, which the integration decision retires — and it is
// narrower than it looks: a contradictory declaration cannot wedge the run, because the attempt it
// never settles is still released by D5's give-up pass on the clock, which the last block drives. What
// is lost is only the *retry* an ADR-conformant verdict would have bought before that window closes.
func TestRevisionResultContradictingItsInputNeverReachesTheObjectStore(t *testing.T) {
	cases := []struct {
		name string
		// contradict rewrites one address the delivered result and the frozen input must agree on.
		contradict func(r *controlpb.RevisionDelivered, in core.Object)
		// wantCode and wantError are the refusal the merged control plane classifies this
		// contradiction as. The split follows upstream's own declaration validator, which has the
		// request never reach the store in both halves: a contradiction *about the attempt* is a
		// result conflict, while a malformed object declaration is a bad request.
		wantCode  codes.Code
		wantError controlpb.ErrorCode
	}{
		{"revision_ref", func(r *controlpb.RevisionDelivered, _ core.Object) {
			r.RevisionRef = "refs/ora/revisions/someone-else"
		}, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT},
		{"base_commit", func(r *controlpb.RevisionDelivered, _ core.Object) {
			r.BaseCommit = strings.Repeat("f", 40)
		}, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT},
		{"bundle_key", func(r *controlpb.RevisionDelivered, in core.Object) {
			r.Bundle.Key = in.S("historyKey")
		}, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT},
		{"history_key", func(r *controlpb.RevisionDelivered, in core.Object) {
			r.History.Key = in.S("bundleKey")
		}, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			objects := f.useObjectStore()
			scene := deliveringScene(t, f)
			execution := f.deliverRevision(t, scene)
			in := f.deliveryInputOf(execution)
			before := f.runVersion(scene.runID)

			res := deliveredResult(scene, in, revisionFinalCommit)
			c.contradict(res.GetRevisionDelivered(), in)
			expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "p5-input-mismatch", 1, res, "revision_delivered")),
				c.wantCode, c.wantError)

			if probed := objects.probed(); len(probed) != 0 {
				t.Fatalf("the input comparison must precede the probes: %d object(s) probed for a result Cloud could already refuse (%v)", len(probed), probed)
			}
			if row := f.revisionRow(scene.runID); row != nil {
				t.Fatalf("no Revision may be registered for a result that contradicts its input, got %v", row)
			}
			// A refusal settles nothing: the whole takeover — receipt, durable result, verdict and
			// settlement — is refused before it opens, so there is no half-recorded attempt to reconcile.
			if got := f.nodeResult(execution); len(got) != 0 {
				t.Fatalf("a refused declaration must write no durable result, got %v", got)
			}
			if got := f.revisionVerification(execution); got != nil {
				t.Fatalf("a refused declaration must record no verdict, got %v", got)
			}
			if got := f.receipts(execution); len(got) != 0 {
				t.Fatalf("a refused declaration must write no receipt, got %v", got)
			}
			if got := f.nodeSequence(execution); got != 0 {
				t.Fatalf("a refused declaration must not advance the sequence, got %d", got)
			}
			if got := f.runPhase(scene.runID); got != "delivering" {
				t.Fatalf("a refused input comparison must keep the run delivering, got %q", got)
			}
			if got := f.runVersion(scene.runID); got != before {
				t.Fatalf("a refused declaration must not write the run: version %d → %d", before, got)
			}
			if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
				t.Fatalf("a refusal that settled nothing must release no retry, got %d attempts", got)
			}
			if got := deleteOperations(f.runOperations(scene.runID)); len(got) != 0 {
				t.Fatalf("a run still delivering must have no delete intent, got %v", got)
			}
			// The refusal is deterministic: replaying the very same payload is refused the same way and
			// still commits nothing, so a Node's retry loop cannot talk Cloud into a different answer.
			expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "", 1, res, "revision_delivered")),
				c.wantCode, c.wantError)
			if got := f.nodeSequence(execution); got != 0 {
				t.Fatalf("a replay must not advance the sequence, got %d", got)
			}
			if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 0 {
				t.Fatalf("a replay must not register a Revision, got %d rows", got)
			}

			// Because the attempt never settles, the run does not wait on it forever: D5's give-up pass
			// releases it on the clock, with an explicit null revisionId. That is the whole reason the
			// refusal above is bounded — a Node that keeps contradicting its own input costs the run its
			// retries, not its convergence.
			f.store.DeliveryGiveUpAfter = time.Nanosecond
			must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
			phase, status, _ := f.threadRunState(scene.runID)
			if phase != "releasing" || status != "completed" {
				t.Fatalf("the give-up pass must release a run whose delivery never settled, got %s/%s", phase, status)
			}
			settled := f.runResult(scene.runID)
			if got := settled.S("deliveryState"); got != "failed" || settled["revisionId"] != nil {
				t.Fatalf("the give-up must record deliveryState=failed and no Revision, got %v", settled)
			}
		})
	}
}

// P5-19, D4 step 4 — the verdict on a delivery's declared objects is Cloud's alone: the Node declares
// what it uploaded, Cloud decides whether the store agrees. The wire enum has to name
// `VERIFICATION_FAILED` because Cloud stores and renders that verdict, so the enum cannot be the
// enforcement point — the merged control plane enforces the rule in `revisionFailureReasons`, and a
// Node that reports the verdict anyway is refused as a malformed result rather than recorded as having
// verified anything.
//
// This is the migrated half of functionB's `internal/controlgrpc/executions_test.go`, which pinned the
// same obligation where functionB's own A side enforced it. The rule and every obligation it carries
// are unchanged; the enforcement point is the merged control plane, so the test asserts it at the real
// gRPC boundary. The refusal is proved by what did *not* happen — no receipt, no durable result, no
// verdict row, no Revision, no retry, no run write — and by a replay being refused identically.
func TestNodeMayNotReportCloudsOwnVerificationVerdict(t *testing.T) {
	f := setup(t)
	objects := f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	before := f.runVersion(scene.runID)

	res := revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_VERIFICATION_FAILED)
	refusal := func() error {
		return errOnly(f.deliveryTerminal(scene, execution, "", 1, res, "revision_failed"))
	}
	expectStatus(t, refusal(), codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)

	// Nothing about the attempt was recorded and nothing was verified: the refusal precedes both the
	// store probe and the takeover, so the run is exactly where it was.
	if probed := objects.probed(); len(probed) != 0 {
		t.Fatalf("a malformed result must not reach the object store, probed %v", probed)
	}
	if got := f.nodeResult(execution); len(got) != 0 {
		t.Fatalf("a refused result must write no durable result, got %v", got)
	}
	if got := f.revisionVerification(execution); got != nil {
		t.Fatalf("a Node's verdict must never be recorded, got %v", got)
	}
	if got := f.receipts(execution); len(got) != 0 {
		t.Fatalf("a refused result must write no receipt, got %v", got)
	}
	if got := f.nodeSequence(execution); got != 0 {
		t.Fatalf("a refused result must not advance the sequence, got %d", got)
	}
	if got := f.runPhase(scene.runID); got != "delivering" {
		t.Fatalf("a refused result must keep the run delivering, got %q", got)
	}
	if got := f.runVersion(scene.runID); got != before {
		t.Fatalf("a refused result must not write the run: version %d → %d", before, got)
	}
	if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
		t.Fatalf("a refusal that settled nothing must release no retry, got %d attempts", got)
	}
	if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 0 {
		t.Fatalf("a refused result must register no Revision, got %d rows", got)
	}
	// The refusal is deterministic, so a Node's retry loop cannot talk Cloud into a different answer.
	expectStatus(t, refusal(), codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
}

// P5-18, §20 — D4's last clause and invariant 9 at the integration boundary: a delivery result that
// arrives after the run left `delivering` decides nothing. The delivered shape is the one that
// matters, because it is the only one that would otherwise register a Revision.
//
// The scene is the one D5's give-up describes: the pass releases a run whose delivery attempt is
// still open because the Node never reported. The delivered result therefore arrives at a run that has
// already been released, with its delete declared and its result decided — and the control plane's own
// fence stops it there, before verification: a released run's Workspace is no longer `ready` with
// admission open, so the takeover is refused with the classified conflict and never reaches the
// object store or the registration step. The refusal is asserted rather than assumed, because
// "registers nothing" is only meaningful if the call really was refused.
func TestLateDeliveredRevisionRegistersNothingOnAReleasedRun(t *testing.T) {
	f := setup(t)
	objects := f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	f.store.DeliveryGiveUpAfter = time.Nanosecond
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("the give-up pass must release the run before the late result arrives, got %q", got)
	}
	in := f.deliveryInputOf(execution)
	version := f.runVersion(scene.runID)
	status, _ := f.runStatusVersion(scene.runID)

	res := deliveredResult(scene, in, revisionFinalCommit)
	expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "p5-late-delivered", 1, res, "revision_delivered")),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if probed := objects.probed(); len(probed) != 0 {
		t.Fatalf("a released run's late delivery must not reach the object store, probed %v", probed)
	}

	if row := f.revisionRow(scene.runID); row != nil {
		t.Fatalf("a run that already decided its delivery must register no Revision, got %v", row)
	}
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("a late delivered result must not move the phase, got %q", got)
	}
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("a late delivered result must not write the run: version %d → %d", version, got)
	}
	if got, _ := f.runStatusVersion(scene.runID); got != status {
		t.Fatalf("a late delivered result must not rewrite the status, got %q", got)
	}
	result := f.runResult(scene.runID)
	if got := result.S("deliveryState"); got != "failed" {
		t.Fatalf("the release's own verdict must stand, got %v", result)
	}
	if v, ok := result["revisionId"]; !ok || v != nil {
		t.Fatalf("a released run's revisionId must stay an explicit null, got %v (present=%v)", v, ok)
	}
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
		t.Fatalf("a late delivered result must not declare a second delete, got %d", got)
	}
	if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
		t.Fatalf("a late delivered result must not release another attempt, got %d", got)
	}
	// The refused event is receipted not at all: the fence refuses it before the takeover commits
	// anything, including the receipt that would otherwise tell the Node it may forget the event.
	if got := f.nodeSequence(execution); got != 0 {
		t.Fatalf("a refused late result must not be receipted, got sequence %d", got)
	}
	if got := len(f.receipts(execution)); got != 0 {
		t.Fatalf("a refused late result must not write a receipt, got %v", got)
	}
	// The refusal is deterministic: the Node's replay gets the same answer and commits nothing more.
	expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "", 1, res, "revision_delivered")),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("a replay of the refused result must not write the run: version %d → %d", version, got)
	}
}

// deliveryRevisionRow is a `revisions` row as this file seeds one, so a case can state which outcome
// an earlier attempt already registered without restating fifteen columns.
type deliveryRevisionRow struct {
	finalCommit string
	// withBundle selects the changed-checkout shape: the three bundle columns set together, from the
	// same input the delivered attempt carries.
	withBundle bool
}

// seedRevision writes the Revision an earlier attempt left for this run, through the same schema the
// registration writes, and returns its id. The row is seeded rather than reached through a second
// delivery: under the phase machine the first registration moves the run off `delivering` in the same
// transaction that writes the receipt, so the branches below are backstops — and a backstop can only
// be exercised by putting the state it defends against in place.
//
// The merged schema chains the Revision to Cloud's verdict, which is chained to the execution that
// produced it (`revisions.execution_id → revision_verifications.execution_id → node_executions`), so a
// believable earlier attempt is three rows. The execution carries a settled result — which is also
// what keeps it off the `one_pending_run_execution` index — and no work item, because nothing below
// reads the earlier attempt's dispatch: only the Revision row it left behind matters here.
func (f *fixture) seedRevision(t *testing.T, scene liveThreadScene, in core.Object, row deliveryRevisionRow) string {
	t.Helper()
	id := uuid.NewString()
	wid := f.runWorkspaceID(scene.runID)
	projectID, repositoryURL := f.runPlacement(scene.runID)
	var bundleKey, bundleSHA *string
	var bundleSize *int64
	outcome := "unchanged"
	if row.withBundle {
		key, size, sha := in.S("bundleKey"), int64(revisionBundleSize), revisionBundleSHA
		bundleKey, bundleSize, bundleSHA = &key, &size, &sha
		outcome = "delivered"
	}
	prior := "exec-prior-" + uuid.NewString()
	input, e := json.Marshal(in)
	must(t, e)
	_, e = f.store.Pool.Exec(`
		INSERT INTO node_executions(execution_id, kind, operation_id, workspace_id, node_id,
		                            node_operation_id, input, result, dispatched_epoch)
		VALUES ($1,'deliver_revision',$2,$3,$4,$1,$5,'{"outcome":"revision_delivered"}',1)`,
		prior, scene.runID, wid, scene.nodeID, string(input))
	must(t, e)
	_, e = f.store.Pool.Exec(`
		INSERT INTO revision_verifications(execution_id, outcome, reason) VALUES ($1,$2,NULL)`, prior, outcome)
	must(t, e)
	_, e = f.store.Pool.Exec(`
		INSERT INTO revisions(id, execution_id, tenant_id, run_id, workspace_id, project_id, repository_url,
		                      base_commit, final_commit, revision_ref,
		                      bundle_key, bundle_size, bundle_sha256,
		                      history_key, history_size, history_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		id, prior, scene.tenantID, scene.runID, wid, projectID, repositoryURL,
		in.S("baseCommit"), row.finalCommit, in.S("revisionRef"),
		bundleKey, bundleSize, bundleSHA,
		in.S("historyKey"), revisionHistorySize, revisionHistorySHA)
	must(t, e)
	return id
}

// P5-15, §19/§21 — invariant 7 ("at most one Revision row per run") has one writer and two guards.
// The control plane's own guard is what a Controller meets in practice: once a run has a Revision no
// further delivery attempt is declared for it at all (`delivery_already_settled`), which is the first
// block below. `UNIQUE (run_id)` is the database backstop behind it, and this case is a backstop test:
// an attempt that was already registered when another one's Revision landed is put in place directly,
// because no sequence of control actions can produce it.
//
// The takeover must refuse rather than register a second row, and the refusal must roll the whole
// transaction back — receipt, durable result, Revision and settlement — because keeping any of it
// would leave Cloud believing half of a delivery it could not reconcile. The pre-existing row is left
// exactly as it was: whichever payload shape the late attempt reports, and whether or not it agrees
// with the row already there, a registration never overwrites one.
//
// Two of the three shapes below agree with the stored Revision exactly. They are still refusals: this
// model settles a run on the Revision its own successful attempt registered, and a second attempt's
// registration is not an idempotent re-registration of it — the run has already left `delivering`, and
// a delivery settlement that could adopt another attempt's row would be a second writer.
func TestRevisionRegistrationIsUniquePerRunAndRollsBackAConflictingAttempt(t *testing.T) {
	cases := []struct {
		name string
		// registered is what an earlier attempt already left for this run.
		registered deliveryRevisionRow
		// delivered is the terminal result this attempt reports.
		delivered func(scene liveThreadScene, in core.Object) *controlpb.ExecutionResult
	}{
		{
			name:       "the same outcome an earlier attempt already registered",
			registered: deliveryRevisionRow{finalCommit: revisionFinalCommit, withBundle: true},
			delivered: func(scene liveThreadScene, in core.Object) *controlpb.ExecutionResult {
				return deliveredResult(scene, in, revisionFinalCommit)
			},
		},
		{
			name:       "a different final commit",
			registered: deliveryRevisionRow{finalCommit: strings.Repeat("c", 40), withBundle: true},
			delivered: func(scene liveThreadScene, in core.Object) *controlpb.ExecutionResult {
				return deliveredResult(scene, in, revisionFinalCommit)
			},
		},
		{
			name:       "an unchanged delivery against a Revision that saved a bundle",
			registered: deliveryRevisionRow{finalCommit: revisionFinalCommit, withBundle: true},
			delivered: func(scene liveThreadScene, in core.Object) *controlpb.ExecutionResult {
				return unchangedResult(scene, in)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.useObjectStore()
			scene := deliveringScene(t, f)
			execution := f.deliverRevision(t, scene)
			in := f.deliveryInputOf(execution)
			before := f.runVersion(scene.runID)
			registeredID := f.seedRevision(t, scene, in, c.registered)
			res := c.delivered(scene, in)

			takeover := errOnly(f.deliveryTerminal(scene, execution, "p5-revision-conflict", 1, res, "revision_delivered"))
			// A constraint violation carries no Fault of its own, so it takes the mapping's safe
			// default: the caller is told persistence refused the write, never which constraint and
			// never any SQL. What the Node must act on is the rollback asserted below, not the code.
			expectStatus(t, takeover, codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNAVAILABLE)

			// Nothing committed: no receipt, no durable result, no sequence move, no retry, no release.
			if got := f.receipts(execution); len(got) != 0 {
				t.Fatalf("a conflicting Revision must roll the receipt back, got %v", got)
			}
			if got := f.nodeResult(execution); got != nil {
				t.Fatalf("a conflicting Revision must roll the durable result back, got %v", got)
			}
			if got := f.nodeSequence(execution); got != 0 {
				t.Fatalf("a conflicting Revision must not advance the sequence, got %d", got)
			}
			if got := f.scalar(`SELECT count(*) FROM revision_verifications WHERE execution_id=$1`, execution); got != 0 {
				t.Fatalf("a conflicting Revision must roll its verification row back, got %d", got)
			}
			if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
				t.Fatalf("a conflicting Revision must release no retry, got %d attempts", got)
			}
			if got := f.runPhase(scene.runID); got != "delivering" {
				t.Fatalf("a conflicting Revision must not move the run, got phase %q", got)
			}
			if got := f.runVersion(scene.runID); got != before {
				t.Fatalf("a conflicting Revision must not write the run: version %d → %d", before, got)
			}
			if got := deleteOperations(f.runOperations(scene.runID)); len(got) != 0 {
				t.Fatalf("a conflicting Revision must declare no delete, got %v", got)
			}
			if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 1 {
				t.Fatalf("the existing Revision must not be duplicated, got %d rows", got)
			}
			row := f.revisionRow(scene.runID)
			if row == nil || row.ID != registeredID || row.FinalCommit != c.registered.finalCommit {
				t.Fatalf("the existing Revision must be left untouched, got %v", row)
			}
			// The refusal is deterministic: replaying the very same result is refused the same way and
			// still commits nothing, so a Node's retry loop cannot talk Cloud into a second row.
			expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "", 1, res, "revision_delivered")),
				codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNAVAILABLE)
			if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 1 {
				t.Fatalf("a replay must not add a Revision, got %d rows", got)
			}
		})
	}

	// The control plane's own guard, which is the one a Controller actually meets: with a Revision
	// already on the run, the settlement declares no further delivery attempt. The re-declaration below
	// repeats the attempt that is already on the run, whole — same frozen input, same target — so the
	// refusal is about the Revision and not about anything the caller got wrong.
	t.Run("the control plane declares no further attempt once a Revision exists", func(t *testing.T) {
		f := setup(t)
		f.useObjectStore()
		scene := deliveringScene(t, f)
		execution := f.deliverRevision(t, scene)
		in := f.deliveryInputOf(execution)
		registeredID := f.seedRevision(t, scene, in, deliveryRevisionRow{finalCommit: revisionFinalCommit, withBundle: true})
		target := f.deliveryTarget(scene.runID)

		if _, e := f.store.EnqueueExecutionWork(context.Background(), scene.runID, "deliver_revision", in, target, time.Time{}); e == nil {
			t.Fatal("a run that already has a Revision must not be handed a second delivery attempt")
		}
		if got := f.scalar(`SELECT count(*) FROM execution_work WHERE run_id=$1 AND kind='deliver_revision'`, scene.runID); got != 1 {
			t.Fatalf("the refused attempt must not be declared, got %d delivery work items", got)
		}
		if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 1 {
			t.Fatalf("the refusal must leave the run's single Revision alone, got %d rows", got)
		}
		if row := f.revisionRow(scene.runID); row == nil || row.ID != registeredID {
			t.Fatalf("the run's Revision must be unchanged, got %v", row)
		}
	})
}

// P5-16, §18/§22 — D2/D3's upload grants over the contract the Controller actually speaks. Cloud signs
// one presigned PUT per object key of that attempt's frozen input, and each grant names exactly one
// key and one method: the URL carries the capability, so nothing the Node does can widen it to another
// object, and nothing is persisted — the grants exist only in the reply.
//
// The refusals are D3's ordered set, asserted here because the Controller's retry policy depends on
// which one it gets: UNAVAILABLE means "a capability is missing, keep the attempt", 404 means "Cloud
// never registered this execution", and 409 means "this attempt can no longer upload anything".
func TestGrantRevisionUploadSignsOneGrantPerObjectKey(t *testing.T) {
	f := setup(t)
	objects := f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	client := controlpb.NewAgentRunServiceClient(f.controlConn)
	grant := func(executionID string) (*controlpb.GrantRevisionUploadResponse, error) {
		return client.GrantRevisionUpload(asController("ctrl-a"), &controlpb.GrantRevisionUploadRequest{Epoch: 1, ExecutionId: executionID})
	}
	grantErr := func(executionID string) error {
		_, e := grant(executionID)
		return e
	}

	out, e := grant(execution)
	must(t, e)
	if len(out.GetGrants()) != 2 {
		t.Fatalf("one attempt gets one grant per object key of its input, got %d", len(out.GetGrants()))
	}
	wantKeys := []string{in.S("bundleKey"), in.S("historyKey")}
	for i, g := range out.GetGrants() {
		if g.GetObjectKey() != wantKeys[i] {
			t.Fatalf("grant %d names %q, want the input's own key %q", i, g.GetObjectKey(), wantKeys[i])
		}
		if g.GetMethod() != http.MethodPut {
			t.Fatalf("grant %d method = %q, want PUT", i, g.GetMethod())
		}
		if !strings.Contains(g.GetUrl(), wantKeys[i]) {
			t.Fatalf("grant %d URL %q must be bound to its own key %q", i, g.GetUrl(), wantKeys[i])
		}
		// The signature covers a create-only condition and the endpoint's own host. Both are headers
		// the Node must send back verbatim, so a grant that omitted them would be refused by S3 — and
		// the create-only condition is what stops a second capability from replacing an object Cloud
		// has already verified.
		if got := g.GetHeaders()["if-none-match"]; got != "*" {
			t.Fatalf("grant %d must bind the create-only condition, got headers %v", i, g.GetHeaders())
		}
		if g.GetHeaders()["host"] == "" {
			t.Fatalf("grant %d must bind the host it was signed for, got headers %v", i, g.GetHeaders())
		}
		// D1 caps the lifetime. Nothing configured one here, so the object-store default applies; the
		// reply must report the instant the server will actually enforce, not a rounded one.
		expires := g.GetExpiresAt().AsTime()
		if age := time.Until(expires); age < 14*time.Minute || age > 15*time.Minute {
			t.Fatalf("grant %d expires in %s, want the object-store default of 15m", i, age)
		}
	}
	// The store saw the same keys Cloud signed, in the same order: the grant path is local signing and
	// writes nothing, so it is the reply itself that has to name the attempt's own keys.
	if got := grantKeys(out.GetGrants()); got == nil || fmt.Sprint(got) != fmt.Sprint(wantKeys) {
		t.Fatalf("Cloud must sign the attempt's own keys in order:\n got %v\nwant %v", got, wantKeys)
	}
	if got := objects.nonHeadRequests(); got != 0 {
		t.Fatalf("signing a grant must not contact the store, got %d request(s)", got)
	}
	// A grant is a capability, not a record: signing one writes nothing. The two durable rows the
	// attempt is made of are compared whole across a second, repeated grant, because "the input is
	// unchanged" is a claim about every column of both — a grant path that stamped a nonce into the
	// work item would break the attempt's idempotency and no narrower assertion would catch it.
	executionRow, workInput := f.deliveryRows(execution)
	repeat, e := grant(execution)
	must(t, e)
	if len(repeat.GetGrants()) != 2 {
		t.Fatalf("an attempt that has not reported can ask for its grants again, got %d", len(repeat.GetGrants()))
	}
	if got, want := repeat.GetGrants()[0].GetObjectKey(), wantKeys[0]; got != want {
		t.Fatalf("a repeated grant names %q, want the same key %q", got, want)
	}
	if afterExecution, afterInput := f.deliveryRows(execution); afterExecution != executionRow || afterInput != workInput {
		t.Fatalf("granting must not write the attempt's own rows:\n execution %s → %s\n work input %s → %s",
			executionRow, afterExecution, workInput, afterInput)
	}
	if got := f.scalar(`SELECT count(*) FROM revisions WHERE run_id=$1`, scene.runID); got != 0 {
		t.Fatalf("granting must register nothing, got %d Revisions", got)
	}
	// And nothing in the schema could hold one: a grant is minted per request and never stored, so no
	// table may have a column an upload URL or a credential could live in.
	if got := f.scalar(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND (column_name ILIKE '%grant%' OR column_name ILIKE '%presign%')`); got != 0 {
		t.Fatalf("the schema must have nowhere to persist an upload grant, got %d column(s)", got)
	}

	// Unknown execution: Cloud never registered it. A session execution gets the same answer, because
	// the merged control plane resolves an upload subject as "a registered deliver_revision execution"
	// and reports anything else as not-found rather than distinguishing "Cloud has no such execution"
	// from "this execution is not a delivery" — which is also what keeps the call from telling a
	// Controller which execution identities exist for other purposes. What D3 requires is that no
	// grant is signed, and neither execution gets one.
	expectStatus(t, grantErr("exec-delivery-nobody"), codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)
	expectStatus(t, grantErr(scene.executionID), codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// An attempt that already has a result is over: its objects can no longer be referred to by any
	// registration, so authorizing a write would only create garbage.
	if _, e := f.deliveryTerminal(scene, execution, "p5-grant-after-result", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
		"revision_failed/upload_failed"); e != nil {
		t.Fatalf("the failed delivery must be taken over: %v", e)
	}
	expectStatus(t, grantErr(execution), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// A registered attempt that has NOT reported yet, while the run has already been released: the
	// logical delivery is settled, so no further attempt exists to upload for. This is reachable only
	// through D5's give-up pass, which releases the run while an attempt is still outstanding — the
	// attempt that would otherwise wait out its backoff is never granted, and neither is one that was
	// already registered when the window closed.
	//
	// The pass is driven on a run whose only attempt is still live, and it is driven directly rather
	// than through a terminal failure, because that is the state D3's refusal defends against: the
	// execution is addressable and result-free, and the run behind it has moved on.
	f2 := setup(t)
	f2.useObjectStore()
	client2 := controlpb.NewAgentRunServiceClient(f2.controlConn)
	grantErr2 := func(executionID string) error {
		_, e := client2.GrantRevisionUpload(asController("ctrl-a"), &controlpb.GrantRevisionUploadRequest{Epoch: 1, ExecutionId: executionID})
		return e
	}
	scene2 := deliveringScene(t, f2)
	live := f2.deliverRevision(t, scene2)
	f2.store.DeliveryGiveUpAfter = time.Nanosecond
	must(t, f2.store.GiveUpStaleDeliveriesOnce(context.Background()))
	if got := f2.runPhase(scene2.runID); got != "releasing" {
		t.Fatalf("the give-up must release the run, got %q", got)
	}
	expectStatus(t, grantErr2(live), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// A deployment that loses its object store cannot sign anything, so the refusal is the same for
	// every execution — and it is a refusal, not an empty reply: a grant-less answer would read as
	// "this attempt has nothing left to upload". The store is removed after a healthy dispatch so the
	// attempt itself is valid and the missing capability is the only reason left; granting succeeds
	// again once it is back, which is what shows the refusal was about the capability.
	f3 := setup(t)
	objects3 := f3.useObjectStore()
	live3 := f3.deliverRevision(t, deliveringScene(t, f3))
	f3.store.ObjectStore = nil
	expectStatus(t, grantErrOnly(f3, live3), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	f3.store.ObjectStore = objects3.config
	if _, e := f3.grantRevisionUpload(live3); e != nil {
		t.Fatalf("granting must work again once the object store is back: %v", e)
	}
}

// P5-14, §19 — the replay matrix. Every shape a Controller's retry can take is one effect: a
// submission replay, a byte-identical re-send, and a re-send whose payload disagrees. The conflict
// cases are asserted alongside because they are the same identity: the receipt is what makes a
// re-send a no-op or a conflict, never a second settlement.
func TestDeliveryReplayMatrix(t *testing.T) {
	f := setup(t)
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	result := revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED)
	const event = "revision_failed/upload_failed"

	if _, e := f.deliveryTerminal(scene, execution, "p5-replay-1", 1, result, event); e != nil {
		t.Fatalf("the first delivery result must be taken over: %v", e)
	}
	attempts := f.deliveryAttempts(scene.runID)
	if len(attempts) != 2 {
		t.Fatalf("the failure must release exactly one retry, got %d", len(attempts))
	}
	version := f.runVersion(scene.runID)
	receipts := f.receipts(execution)

	// (a) Same submission identity: the recorded response is replayed and nothing runs.
	if _, e := f.deliveryTerminal(scene, execution, "p5-replay-1", 1, result, event); e != nil {
		t.Fatalf("a submission replay must succeed, got %v", e)
	}
	// (b) No submission identity, same bytes and result: the receipt makes it a deterministic no-op.
	if _, e := f.deliveryTerminal(scene, execution, "", 1, result, event); e != nil {
		t.Fatalf("a byte-identical replay must be a no-op, not a fault: %v", e)
	}
	// (c) Same sequence, different bytes, and same bytes with a different result.
	expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "", 1, result, "revision_failed/something_else")),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	expectStatus(t, errOnly(f.deliveryTerminal(scene, execution, "", 1,
		revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_BUNDLE_FAILED), event)),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	if got := fmt.Sprint(f.deliveryAttempts(scene.runID)); got != fmt.Sprint(attempts) {
		t.Fatalf("a replay must not release a second attempt:\n got %v\nwant %v", got, attempts)
	}
	if got := fmt.Sprint(f.receipts(execution)); got != fmt.Sprint(receipts) {
		t.Fatalf("a replay must not write a receipt: got %v want %v", got, receipts)
	}
	if got := f.nodeSequence(execution); got != 1 {
		t.Fatalf("a replay must not advance the sequence, got %d", got)
	}
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("a replay must not write the run: version %d → %d", version, got)
	}
	if got := f.runPhase(scene.runID); got != "delivering" {
		t.Fatalf("a replay must not move the phase, got %q", got)
	}
}

// P5-15, §20 — the serialization matrix. Each case starts every writer from one barrier and asserts
// the complete set of legal outcomes rather than the most common one; the invariant under all of them
// is that one release is one release: one delete intent, one retry, one receipt.
func TestDeliverySerializesWithConcurrentWriters(t *testing.T) {
	t.Run("the same failed result sent twice", func(t *testing.T) {
		f := setup(t)
		scene := deliveringScene(t, f)
		execution := f.deliverRevision(t, scene)

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		wg.Add(2)
		for i := range errs {
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.deliveryTerminal(scene, execution, "", 1,
					revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
					"revision_failed/upload_failed")
			}(i)
		}
		close(start)
		wg.Wait()

		// Both may succeed: one commits and the other is the byte-identical replay of a committed
		// event. Neither may fault, because a fault would make the Node replay forever.
		for i, e := range errs {
			if e != nil {
				t.Fatalf("concurrent duplicate delivery result %d must be a no-op, not a fault: %v", i, e)
			}
		}
		if got := f.receipts(execution); len(got) != 1 {
			t.Fatalf("the duplicate must produce exactly one receipt, got %v", got)
		}
		if got := f.nodeSequence(execution); got != 1 {
			t.Fatalf("the duplicate must advance the sequence once, got %d", got)
		}
		if got := len(f.deliveryAttempts(scene.runID)); got != 2 {
			t.Fatalf("the duplicate must release exactly one retry, got %d attempts", got)
		}
		if got := f.runPhase(scene.runID); got != "delivering" {
			t.Fatalf("the run must stay delivering, got %q", got)
		}
	})

	t.Run("the give-up pass ticks while the delivery settles", func(t *testing.T) {
		f := setup(t)
		scene := deliveringScene(t, f)
		execution := f.deliverRevision(t, scene)
		f.store.DeliveryGiveUpAfter = time.Nanosecond

		var wg sync.WaitGroup
		start := make(chan struct{})
		var passErr, takeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			passErr = f.store.GiveUpStaleDeliveriesOnce(context.Background())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, takeErr = f.deliveryTerminal(scene, execution, "", 1,
				revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
				"revision_failed/upload_failed")
		}()
		close(start)
		wg.Wait()

		must(t, passErr)
		// The two writers race under the same predicate, and either may win. A takeover that arrives
		// after the pass released the run loses to the control plane's own fence — `liveDelivery`
		// requires a Workspace that is still `ready` with admission open, and the release's delete
		// declaration has already closed it — so it is refused with the classified conflict instead of
		// being receipted against a run that is no longer delivering. That is a legal loss, and the Node
		// learns it must stop reporting through the same conflict every late event gets; what it must
		// never become is an unclassified fault.
		taken := takeErr == nil
		if !taken {
			expectStatus(t, takeErr, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
		}
		// Whichever writer won, the outcome is the same single release: one phase move, one delete
		// intent, one recorded failure, and never a second attempt declared after the release.
		//
		// The attempt count is exactly one, not "one or two", because both writers evaluate the same
		// give-up predicate under the same serialization: the pass releases the run, and the takeover
		// either settles it while the window is already open (release, no retry) or arrives after the
		// release and is refused (also no retry). The retry branch is unreachable while
		// `DeliveryGiveUpAfter` is open, so a count that varied would be a real bug.
		if got := f.runPhase(scene.runID); got != "releasing" {
			t.Fatalf("the run must end releasing, got %q", got)
		}
		if got := f.runResult(scene.runID).S("deliveryState"); got != "failed" {
			t.Fatalf("deliveryState = %q, want failed", got)
		}
		// The event is a fact about the attempt exactly when it was taken over: one receipt and one
		// durable result, never two, and none at all when the fence refused it before the takeover.
		wantReceipts := int64(0)
		if taken {
			wantReceipts = 1
		}
		if got := f.nodeSequence(execution); got != wantReceipts {
			t.Fatalf("a taken-over failure must be receipted exactly once and a refused one not at all, got sequence %d (taken=%v)", got, taken)
		}
		if got := int64(len(f.receipts(execution))); got != wantReceipts {
			t.Fatalf("receipts must match the takeover's outcome, got %d (taken=%v)", got, taken)
		}
		if got := len(f.deliveryAttempts(scene.runID)); got != 1 {
			t.Fatalf("the release must not leave a second attempt behind, got %d", got)
		}
		if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
			t.Fatalf("exactly one delete intent may exist, got %d", got)
		}
	})

	t.Run("the delete reconciliation ticks while the run is released", func(t *testing.T) {
		f := setup(t)
		scene := deliveringScene(t, f)
		execution := f.deliverRevision(t, scene)
		f.store.DeliveryGiveUpAfter = time.Nanosecond

		var wg sync.WaitGroup
		start := make(chan struct{})
		var passErr, takeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			passErr = f.store.RedeclareRunWorkspaceDeletesOnce(context.Background())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, takeErr = f.deliveryTerminal(scene, execution, "", 1,
				revisionFailedResult(scene, controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED),
				"revision_failed/upload_failed")
		}()
		close(start)
		wg.Wait()

		must(t, passErr)
		if takeErr != nil {
			t.Fatalf("the delivery takeover must win or lose legally, got %v", takeErr)
		}
		if got := f.runPhase(scene.runID); got != "releasing" {
			t.Fatalf("the run must end releasing, got %q", got)
		}
		if got := len(deleteOperations(f.runOperations(scene.runID))); got != 1 {
			t.Fatalf("two writers must still declare exactly one delete, got %d", got)
		}
	})
}

// P5-16, §12/§13 — a Node that refuses to quiesce fails the operation and restores the Workspace
// (operation D4's "quiesce 失败并按原规则重试"), and for a run Workspace the party that retries is
// Cloud: the run has no public API and no user. Without the reconciliation pass such a run would sit
// in `releasing` forever with no operation and no hook.
func TestRefusedQuiesceIsRedeclaredAndStillReachesDone(t *testing.T) {
	f := setup(t)
	scene, _ := givingUpScene(t, f)
	wid := f.runWorkspaceID(scene.runID)

	// The scene's Node is seeded as the sandbox's Node; give it the substrate identity and the
	// service subject a real Node credential carries, so the refusal below travels the production
	// node-control path rather than a stub.
	f.bindRunWorkspaceSandbox(t, scene.runID)

	deleteOp := deleteOperations(f.runOperations(scene.runID))
	if len(deleteOp) != 1 {
		t.Fatalf("the scene must hold one delete intent, got %d", len(deleteOp))
	}
	opID := deleteOp[0].S("id")

	// The Node refuses: it still has work in the Workspace. Node control answers the refusal as a
	// well-formed verdict rather than a transport fault — HTTP 200 with `accepted: false` — which is
	// exactly why the operation must interpret it, not merely observe a non-200. The operation fails
	// and the Workspace returns to the state its snapshot recorded.
	body, status, e := f.refuseQuiesce(opID, wid)
	must(t, e)
	if status != http.StatusOK {
		t.Fatalf("a Node's refusal is a verdict, not a fault: got HTTP %d (%v)", status, body)
	}
	if body.B("accepted") || body.S("errorCode") != "resource_in_use" {
		t.Fatalf("the refusal must report resource_in_use, got %v", body)
	}
	ops := deleteOperations(f.runOperations(scene.runID))
	if len(ops) != 1 || ops[0].S("state") != "failed" {
		t.Fatalf("a refused quiesce must fail the operation, got %v", ops)
	}
	if got := ops[0].S("errorCode"); got != "resource_in_use" {
		t.Fatalf("the failed operation must record the Node's own refusal code, got %q", got)
	}
	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("the run must stay releasing, got %q", got)
	}
	ws := f.workspaceFields(wid)
	if !ws.B("admissionOpen") || ws.B("deleted") {
		t.Fatalf("a refused quiesce must restore the Workspace, got %v", ws)
	}

	// Cloud re-declares: the intent is durable, only its operation was refused. A second pass with an
	// operation already in flight declares nothing, so the reconciliation is idempotent.
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
	ops = deleteOperations(f.runOperations(scene.runID))
	if len(ops) != 2 {
		t.Fatalf("the reconciliation must declare a fresh delete, got %d", len(ops))
	}
	if ops[1].S("state") != "queued" || ops[1].S("idempotencyKey") != ops[0].S("idempotencyKey") {
		t.Fatalf("the re-declaration must be the same intent, got %v", ops[1])
	}
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
	if got := len(deleteOperations(f.runOperations(scene.runID))); got != 2 {
		t.Fatalf("a second pass with an operation in flight must declare nothing, got %d", got)
	}

	// And the run still finishes: this time the Node confirms idle.
	f.driveDelete(t, scene.runID)
	if got := f.runPhase(scene.runID); got != "done" {
		t.Fatalf("the re-declared delete must settle the run, got %q", got)
	}
	ops = deleteOperations(f.runOperations(scene.runID))
	if ops[len(ops)-1].S("state") != "succeeded" {
		t.Fatalf("the re-declared delete must reach succeeded, got %v", ops)
	}
}

// refuseQuiesce reports a Node's refusal through the production node-control action: the credential
// is the Node's own, the admission epoch is the current one, and the request names the operation
// whose quiesce it is refusing. It returns the endpoint's own body and status so the caller asserts
// the contract's answer rather than a transport symptom.
func (f *fixture) refuseQuiesce(opID, wid string) (core.Object, int, error) {
	f.t.Helper()
	var version int64
	must(f.t, f.store.Pool.QueryRow(`SELECT version FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL`, wid).Scan(&version))
	body := core.Object{"version": version, "admissionEpoch": f.workspaceFields(wid).N("admissionEpoch"), "operationId": opID, "idle": false}
	return f.client.Call(context.Background(), "POST", "/internal/v1/nodes/idle", "node", f.node(wid), nil, "", body)
}
