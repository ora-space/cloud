package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

func revisionStorage(t *testing.T) *objectstore.Config {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		if os.Getenv("REQUIRE_S3") == "1" {
			t.Fatal("TEST_S3_ENDPOINT is required for real S3 acceptance")
		}
		t.Skip("real S3 acceptance: set TEST_S3_ENDPOINT and mounted test credential files")
	}
	access, err := os.ReadFile(os.Getenv("TEST_S3_ACCESS_KEY_FILE"))
	must(t, err)
	secret, err := os.ReadFile(os.Getenv("TEST_S3_SECRET_KEY_FILE"))
	must(t, err)
	return &objectstore.Config{Endpoint: endpoint, Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: strings.TrimSpace(string(access)), SecretAccessKey: strings.TrimSpace(string(secret)), UploadGrantTTL: time.Minute}
}

func (f *fixture) registeredDelivery(t *testing.T, cfg *objectstore.Config) (string, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	f.store.ObjectStore = cfg
	run, session, node, incarnation := f.registeredSession(t, "revision")
	ctx := asController(f.client.Subject)
	ended := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED}}}
	_, err := f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "session-end", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: session, Sequence: 1, Result: ended, Event: []byte("sealed-session")})
	must(t, err)
	var wid string
	must(t, f.store.Pool.QueryRow("SELECT id::text FROM workspaces WHERE issue_run_id=$1", run).Scan(&wid))
	sandbox, _, _ := f.liveNode(t, wid)
	input := core.Object{"kind": "deliver_revision", "sessionExecutionId": session, "checkoutExecutionId": f.checkoutOf(wid), "baseCommit": f.commit}
	target := core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}
	work, err := f.store.EnqueueExecutionWork(t.Context(), run, "deliver_revision", input, target, time.Time{})
	must(t, err)
	again, err := f.store.EnqueueExecutionWork(t.Context(), run, "deliver_revision", input, target, time.Time{})
	must(t, err)
	if work.S("id") != again.S("id") || !strings.Contains(work.O("input").S("historyKey"), "/"+work.S("id")+"/") {
		t.Fatal("keys were not fixed to the delivery work identity")
	}
	claim, err := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, err)
	_, err = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "delivery-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "revision-delivery", NodeId: node, Input: claim.GetItem().GetInput()})
	must(t, err)
	spec := claim.GetItem().GetInput().GetDeliverRevision()
	pending, err := f.executions.ListPendingDispatches(ctx, &controlpb.ListPendingDispatchesRequest{NodeId: node})
	must(t, err)
	found := false
	for _, record := range pending.GetRecords() {
		if record.GetExecutionId() == "revision-delivery" {
			found = proto.Equal(record.GetInput(), claim.GetItem().GetInput())
		}
	}
	if !found {
		t.Fatal("registered delivery was absent from recovery reads")
	}
	recovered, err := f.executions.GetDispatch(ctx, &controlpb.GetDispatchRequest{ExecutionId: "revision-delivery"})
	must(t, err)
	if !proto.Equal(recovered.GetRecord().GetInput(), claim.GetItem().GetInput()) {
		t.Fatal("GetDispatch changed the frozen delivery input")
	}
	// The Node pushed its final commit, here its unchanged baseline, to the attempt's delivery ref.
	f.pushedDelivery("revision-delivery", f.commit)
	history := []byte("{\"kind\":\"assistant\",\"text\":\"echo\"}\n")
	sum := sha256.Sum256(history)
	result := &controlpb.ExecutionResult{Node: ended.GetNode(), Outcome: &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{FinalCommit: f.commit, BaseCommit: f.commit, RevisionRef: spec.GetRevisionRef(), History: &controlpb.StoredObject{Key: spec.GetHistoryKey(), Size: uint64(len(history)), Sha256: hex.EncodeToString(sum[:])}}}}
	return wid, &controlpb.TakeOverNodeEventRequest{SubmissionId: "delivery-result", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "revision-delivery", Sequence: 1, Result: result, Event: []byte("original-node-evidence")}
}

func TestFailedDeliveryRetryUsesNewWorkKeysAndAtomicHook(t *testing.T) {
	f := setup(t)
	cfg := &objectstore.Config{Endpoint: "http://private.invalid:9000", PublicEndpoint: "http://public.invalid:9000", Region: "us-east-1", Bucket: "revisions", AccessKeyID: "fixture", SecretAccessKey: "fixture", PathStyle: true}
	wid, req := f.registeredDelivery(t, cfg)
	var before string
	must(t, f.store.Pool.QueryRow("SELECT input::text FROM node_executions WHERE execution_id=$1", req.ExecutionId).Scan(&before))
	for range 2 {
		_, err := controlpb.NewAgentRunServiceClient(f.controlConn).GrantRevisionUpload(asController(f.client.Subject), &controlpb.GrantRevisionUploadRequest{Epoch: f.controller.Epoch, ExecutionId: req.ExecutionId})
		must(t, err)
	}
	var after string
	must(t, f.store.Pool.QueryRow("SELECT input::text FROM node_executions WHERE execution_id=$1", req.ExecutionId).Scan(&after))
	if before != after {
		t.Fatal("grant refresh changed immutable execution input")
	}
	grantClient := controlpb.NewAgentRunServiceClient(f.controlConn)
	for _, checksums := range []map[string]string{{"outside-frozen-input": strings.Repeat("0", 64)}, {req.Result.GetRevisionUnchanged().History.Key: "not-sha256"}} {
		if _, err := grantClient.GrantRevisionUpload(asController(f.client.Subject), &controlpb.GrantRevisionUploadRequest{Epoch: f.controller.Epoch, ExecutionId: req.ExecutionId, Checksums: checksums}); err == nil {
			t.Fatal("grant accepted an unfrozen key or malformed checksum")
		}
	}
	_, err := f.store.Pool.Exec("CREATE TABLE failed_delivery_hook(execution_id text PRIMARY KEY)")
	must(t, err)
	fail := true
	f.store.OnDeliverySettled = func(ctx context.Context, tx *sql.Tx, _, execution string, settled core.Object) error {
		if settled.S("outcome") != "failed" || settled.S("reason") != "upload_failed" {
			return errors.New("unexpected failure evidence")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO failed_delivery_hook VALUES($1)", execution); err != nil {
			return err
		}
		if fail {
			return errors.New("rollback failure settlement")
		}
		return nil
	}
	oldHistory := req.Result.GetRevisionUnchanged().History.Key
	req.Result.Outcome = &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED}}
	_, err = f.executions.TakeOverNodeEvent(asController(f.client.Subject), req)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if f.scalar("SELECT count(*) FROM failed_delivery_hook") != 0 || f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", req.ExecutionId) != 0 {
		t.Fatal("failed hook committed a receipt or business transition")
	}
	fail = false
	_, err = f.executions.TakeOverNodeEvent(asController(f.client.Subject), req)
	must(t, err)
	req.SubmissionId = "failed-delivery-replay"
	_, err = f.executions.TakeOverNodeEvent(asController(f.client.Subject), req)
	must(t, err)
	if f.scalar("SELECT count(*) FROM failed_delivery_hook") != 1 {
		t.Fatal("failure replay invoked the hook twice")
	}
	var session string
	must(t, f.store.Pool.QueryRow("SELECT input->>'sessionExecutionId' FROM node_executions WHERE execution_id=$1", req.ExecutionId).Scan(&session))
	sandbox, node, _ := f.liveNode(t, wid)
	work, err := f.store.EnqueueExecutionWork(t.Context(), req.OperationId, "deliver_revision", core.Object{"kind": "deliver_revision", "sessionExecutionId": session, "checkoutExecutionId": f.checkoutOf(wid), "baseCommit": f.commit}, core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, err)
	if work.O("input").S("historyKey") == oldHistory || !strings.Contains(work.O("input").S("historyKey"), "/"+work.S("id")+"/") || f.scalar("SELECT count(*) FROM revisions") != 0 {
		t.Fatal("retry reused a previous work item's object key or fabricated a Revision")
	}
}

func putRevisionObject(t *testing.T, cfg *objectstore.Config, key string, data []byte, checksum string) int {
	t.Helper()
	if checksum == "" {
		sum := sha256.Sum256(data)
		checksum = base64.StdEncoding.EncodeToString(sum[:])
	}
	decoded, err := base64.StdEncoding.DecodeString(checksum)
	must(t, err)
	grant, err := objectstore.PresignPUTChecksum(cfg, key, hex.EncodeToString(decoded), time.Now())
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), "PUT", grant.URL, bytes.NewReader(data))
	must(t, err)
	for name, value := range grant.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("X-Amz-Checksum-Sha256", checksum)
	response, err := http.DefaultClient.Do(req)
	must(t, err)
	if response.StatusCode != http.StatusOK {
		var failure struct{ Code string }
		_ = xml.NewDecoder(response.Body).Decode(&failure)
		t.Logf("S3 PUT rejected: status=%d code=%s", response.StatusCode, failure.Code)
	}
	response.Body.Close()
	return response.StatusCode
}

// This matrix contacts real S3 storage. A synthetic successful HEAD cannot satisfy its boundary.
func TestRevisionS3VerificationAndAtomicTakeover(t *testing.T) {
	cfg := revisionStorage(t)
	for _, mode := range []string{"unchanged", "delivered", "missing", "digest", "size", "hook-rollback", "commit-rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			_, req := f.registeredDelivery(t, cfg)
			history := req.GetResult().GetRevisionUnchanged().GetHistory()
			data := []byte("{\"kind\":\"assistant\",\"text\":\"echo\"}\n")
			if mode != "missing" {
				if status := putRevisionObject(t, cfg, history.GetKey(), data, ""); status != 200 {
					t.Fatalf("S3 PUT status=%d", status)
				}
			}
			if mode == "digest" {
				history.Sha256 = strings.Repeat("a", 64)
			}
			if mode == "size" {
				history.Size++
			}
			if mode == "delivered" {
				// The Git push itself is exercised against a real Git Server in git_server_test.go.
				final := strings.Repeat("b", 40)
				f.pushedDelivery(req.ExecutionId, final)
				req.Result.Outcome = &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{BaseCommit: f.commit, FinalCommit: final, RevisionRef: req.Result.GetRevisionUnchanged().GetRevisionRef(), History: history}}
			}
			_, err := f.store.Pool.Exec("CREATE TABLE delivery_hook_evidence(outcome text, revision_id uuid)")
			must(t, err)
			fail := mode == "hook-rollback"
			f.store.OnDeliverySettled = func(ctx context.Context, tx *sql.Tx, _, execution string, settled core.Object) error {
				var receipts int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", execution).Scan(&receipts); err != nil {
					return err
				}
				if receipts != 1 {
					return errors.New("hook ran before event takeover")
				}
				var revision any
				if id := settled.O("revision").S("id"); id != "" {
					revision = id
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO delivery_hook_evidence VALUES($1,$2)", settled.S("outcome"), revision); err != nil {
					return err
				}
				if fail {
					return errors.New("rollback business transition")
				}
				return nil
			}
			if mode == "commit-rollback" {
				_, err = f.store.Pool.Exec(`CREATE FUNCTION fail_revision_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected deferred commit failure'; END $$;
				 CREATE CONSTRAINT TRIGGER fail_revision_commit AFTER INSERT ON revisions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_revision_commit()`)
				must(t, err)
			}
			ctx := asController(f.client.Subject)
			_, err = f.executions.TakeOverNodeEvent(ctx, req)
			if fail || mode == "commit-rollback" {
				if err == nil {
					t.Fatal("injected failure committed")
				}
				for _, table := range []string{"revisions", "revision_verifications", "delivery_hook_evidence", "node_event_receipts"} {
					query := "SELECT count(*) FROM " + table
					if table == "node_event_receipts" {
						query += " WHERE execution_id='revision-delivery'"
					}
					if f.scalar(query) != 0 {
						t.Fatal("rollback left state in", table)
					}
				}
				fail = false
				if mode == "commit-rollback" {
					_, err = f.store.Pool.Exec("DROP TRIGGER fail_revision_commit ON revisions")
					must(t, err)
				}
				_, err = f.executions.TakeOverNodeEvent(ctx, req)
			}
			must(t, err)
			negative := mode == "missing" || mode == "digest" || mode == "size"
			want := 1
			if negative {
				want = 0
			}
			if f.scalar("SELECT count(*) FROM revisions") != want || f.scalar("SELECT count(*) FROM delivery_hook_evidence") != 1 {
				t.Fatal("incorrect verification settlement")
			}
			if negative && f.scalar("SELECT count(*) FROM revision_verifications WHERE outcome='failed' AND reason='verification_failed'") != 1 {
				t.Fatal("negative proof was not settled as verification_failed")
			}
			if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result->>'outcome'=$2", req.ExecutionId, map[bool]string{true: "revision_delivered", false: "revision_unchanged"}[mode == "delivered"]) != 1 {
				t.Fatal("Cloud overwrote original Node result")
			}
			// Both submission replay and fresh submission replay work without storage being available.
			f.store.ObjectStore = &objectstore.Config{}
			_, err = f.executions.TakeOverNodeEvent(ctx, req)
			must(t, err)
			req.SubmissionId = "fresh-replay"
			_, err = f.executions.TakeOverNodeEvent(ctx, req)
			must(t, err)
			if f.scalar("SELECT count(*) FROM delivery_hook_evidence") != 1 || f.scalar("SELECT count(*) FROM revisions") != want {
				t.Fatal("replay settled twice")
			}
			req.Sequence++
			req.SubmissionId = "late-terminal"
			_, err = f.executions.TakeOverNodeEvent(ctx, req)
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
		})
	}
}

func TestRevisionNetworkFailureRemainsReplayable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	f := setup(t)
	cfg := &objectstore.Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "revisions", AccessKeyID: "fixture", SecretAccessKey: "fixture", PathStyle: true}
	_, req := f.registeredDelivery(t, cfg)
	_, err := f.executions.TakeOverNodeEvent(asController(f.client.Subject), req)
	expectStatus(t, err, codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNAVAILABLE)
	if f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", req.ExecutionId) != 0 || f.scalar("SELECT count(*) FROM revision_verifications") != 0 || f.scalar("SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result IS NOT NULL", req.ExecutionId) != 0 {
		t.Fatal("network failure became ACKable")
	}
}

func TestRevisionLeaseAndStateAreRecheckedAfterExternalVerification(t *testing.T) {
	for _, change := range []string{"lease", "cancel", "delete", "node"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			var req *controlpb.TakeOverNodeEventRequest
			var wid string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				query := map[string]string{"lease": "UPDATE controller_leases SET expires_at=clock_timestamp()", "cancel": "UPDATE issue_runs SET status='cancelled' WHERE id=$1", "delete": "UPDATE workspaces SET admission_open=false WHERE issue_run_id=$1", "node": "UPDATE node_instances SET connection_state='ended' WHERE workspace_id=$1"}[change]
				var err error
				switch change {
				case "lease":
					_, err = f.store.Pool.Exec(query)
				case "node":
					_, err = f.store.Pool.Exec(query, wid)
				default:
					_, err = f.store.Pool.Exec(query, req.OperationId)
				}
				if err != nil {
					t.Error(err)
				}
				w.WriteHeader(404)
			}))
			defer server.Close()
			wid, req = f.registeredDelivery(t, &objectstore.Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "revisions", AccessKeyID: "fixture", SecretAccessKey: "fixture", PathStyle: true})
			_, err := f.executions.TakeOverNodeEvent(asController(f.client.Subject), req)
			if err == nil || f.scalar("SELECT count(*) FROM revision_verifications") != 0 || f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", req.ExecutionId) != 0 {
				t.Fatal("external verification bypassed current fencing")
			}
		})
	}
}

func TestS3RejectsIncorrectChecksumAndExpiredGrant(t *testing.T) {
	cfg := revisionStorage(t)
	prefix := "acceptance/" + uuid.NewString()
	if status := putRevisionObject(t, cfg, prefix+"/checksum-invalid", []byte("bytes"), base64.StdEncoding.EncodeToString(make([]byte, 32))); status >= 200 && status < 300 {
		t.Fatal("S3 accepted an incorrect checksum")
	}
	sum := sha256.Sum256([]byte("bytes"))
	grant, err := objectstore.PresignPUTChecksum(cfg, prefix+"/expired", hex.EncodeToString(sum[:]), time.Now().Add(-2*time.Minute))
	must(t, err)
	req, err := http.NewRequestWithContext(t.Context(), "PUT", grant.URL, bytes.NewReader([]byte("bytes")))
	must(t, err)
	for name, value := range grant.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("S3 accepted expired grant", resp.StatusCode)
	}
	// Refresh keeps the same object key and input while receiving a usable signature.
	if status := putRevisionObject(t, cfg, prefix+"/expired", []byte("bytes"), ""); status != 200 {
		t.Fatal("refreshed grant failed", status)
	}
}
