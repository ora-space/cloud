package integration

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// Expire the first signature in transit while keeping the response's advertised expiry. The
// genuine S3 endpoint must reject it, and the Controller must ask Cloud for a fresh capability.
type expiryFirstGrantClient struct {
	controlpb.AgentRunServiceClient
	cfg         *objectstore.Config
	requests    int
	key, digest string
}

// Forward the first signed PUT to real S3, then cancel before the uploader can observe its response.
// The next Controller must reuse the exact prepared bytes rather than regenerate a snapshot.
type cancelAfterUploadClient struct {
	controlpb.AgentRunServiceClient
	proxyURL string
	targets  chan string
}

func (c *cancelAfterUploadClient) GrantRevisionUpload(ctx context.Context, req *controlpb.GrantRevisionUploadRequest, opts ...grpc.CallOption) (*controlpb.GrantRevisionUploadResponse, error) {
	response, err := c.AgentRunServiceClient.GrantRevisionUpload(ctx, req, opts...)
	if err == nil {
		grant := response.GetGrants()[0]
		c.targets <- grant.GetUrl()
		grant.Url = c.proxyURL
	}
	return response, err
}

func (c *expiryFirstGrantClient) GrantRevisionUpload(ctx context.Context, req *controlpb.GrantRevisionUploadRequest, opts ...grpc.CallOption) (*controlpb.GrantRevisionUploadResponse, error) {
	response, err := c.AgentRunServiceClient.GrantRevisionUpload(ctx, req, opts...)
	if err != nil {
		return nil, err
	}
	c.requests++
	if c.requests == 1 {
		grant := response.GetGrants()[0]
		c.key, c.digest = grant.ObjectKey, req.Checksums[grant.ObjectKey]
		expired, err := objectstore.PresignPUTChecksum(c.cfg, c.key, c.digest, time.Now().Add(-2*time.Minute))
		if err != nil {
			return nil, err
		}
		grant.Url = expired.URL
	} else if c.requests == 2 && req.Checksums[c.key] != c.digest {
		return nil, errors.New("refresh changed object key or checksum")
	}
	return response, nil
}

// Real PostgreSQL, Git and S3 surround disk-backed Controller/Node doubles. Business callbacks
// remain explicit test adapters: this proves A's hooks and recovery, not B's production phase loop.
func TestSimulatorDeliversRevisionToS3AcrossRestart(t *testing.T) {
	cfg := revisionStorage(t)
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed"}[changed], func(t *testing.T) {
			f := setup(t)
			f.store.ObjectStore = cfg
			p := f.create("echo-revision")
			f.drain()
			run := f.insertAgentRun(t, p.O("resource").S("id"))
			wid := f.readyRunWorkspace(t, run)
			var checkoutExecution, checkout string
			must(t, f.store.Pool.QueryRow("SELECT execution_id,result->>'path' FROM clone_executions WHERE workspace_id=$1 AND result->>'outcome'='clone_ready'", wid).Scan(&checkoutExecution, &checkout))
			sandbox, node, _ := f.liveNode(t, wid)
			target := core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}
			input := sessionInput()
			input["checkoutExecutionId"] = checkoutExecution
			_, err := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", input, target, time.Time{})
			must(t, err)
			_, err = f.controller.Step(t.Context())
			must(t, err)
			var session string
			must(t, f.store.Pool.QueryRow("SELECT execution_id FROM node_executions WHERE operation_id=$1 AND kind='agent_session'", run).Scan(&session))
			_, err = f.store.EnqueueThreadCommand(t.Context(), run, "submit_user_turn", core.Object{"turnId": "echo-followup", "content": []core.Object{{"text": "persist this conversation"}}})
			must(t, err)
			_, err = f.store.EnqueueThreadCommand(t.Context(), run, "end_session", core.Object{"reason": "user_ended"})
			must(t, err)
			_, err = f.controller.Step(t.Context())
			must(t, err)
			if changed {
				must(t, os.WriteFile(filepath.Join(checkout, "result.txt"), []byte("saved agent result\n"), 0o600))
			}
			branchBefore := runGit(t, "-C", checkout, "rev-parse", "HEAD")
			statusBefore := runGit(t, "-C", checkout, "status", "--porcelain")
			work, err := f.store.EnqueueExecutionWork(t.Context(), run, "deliver_revision", core.Object{"kind": "deliver_revision", "sessionExecutionId": session, "checkoutExecutionId": checkoutExecution, "baseCommit": f.commit}, target, time.Time{})
			must(t, err)
			claim, err := f.executions.ClaimWork(asController(f.client.Subject), &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
			must(t, err)
			_, err = f.executions.RecordDispatch(asController(f.client.Subject), &controlpb.RecordDispatchRequest{SubmissionId: "restart-delivery-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "restart-delivery", NodeId: node, Input: claim.GetItem().GetInput()})
			must(t, err)
			_, err = f.store.Pool.Exec("CREATE TABLE simulator_delivery_hook(outcome text PRIMARY KEY)")
			must(t, err)
			fail := true
			f.store.OnDeliverySettled = func(ctx context.Context, tx *sql.Tx, _, _ string, settled core.Object) error {
				if _, err := tx.ExecContext(ctx, "INSERT INTO simulator_delivery_hook VALUES($1)", settled.S("outcome")); err != nil {
					return err
				}
				if fail {
					return errors.New("lose transaction after upload")
				}
				return nil
			}
			originalClient := f.controller.AgentRuns
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			targets := make(chan string, 1)
			uploaded := make(chan int, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, <-targets, r.Body)
				if err != nil {
					uploaded <- 0
					cancel()
					return
				}
				req.Header = r.Header.Clone()
				req.ContentLength = r.ContentLength
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					uploaded <- 0
					cancel()
					return
				}
				response.Body.Close()
				uploaded <- response.StatusCode
				cancel()
				w.WriteHeader(response.StatusCode)
			}))
			t.Cleanup(proxy.Close)
			f.controller.AgentRuns = &cancelAfterUploadClient{AgentRunServiceClient: originalClient, proxyURL: proxy.URL, targets: targets}
			_, err = f.controller.Step(ctx)
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatal("Controller cancellation was converted to a terminal delivery failure")
			}
			if status := <-uploaded; status != http.StatusOK {
				t.Fatal("fault injection did not commit the first object to real S3", status)
			}
			// Change the dirty checkout to make a regenerated snapshot observably different, without
			// depending on commit timestamps. Replay must deliver the already prepared payload.
			if changed {
				must(t, os.WriteFile(filepath.Join(checkout, "result.txt"), []byte("later checkout edit must not replace the frozen delivery\n"), 0o600))
			}
			preparedRef := runGit(t, "-C", checkout, "rev-parse", work.O("input").S("revisionRef"))
			rebootedNode, err := simulator.NewAgentNode(filepath.Join(f.root, "agent-node"))
			must(t, err)
			f.controller = &simulator.Controller{Client: f.client, SubstrateURL: f.external.URL, Executions: f.executions, AgentRuns: originalClient, AgentNode: rebootedNode, Git: f.git}
			must(t, f.controller.Acquire(t.Context()))
			refresh := &expiryFirstGrantClient{AgentRunServiceClient: originalClient, cfg: cfg}
			f.controller.AgentRuns = refresh
			_, err = f.controller.Step(t.Context())
			if err == nil || f.scalar("SELECT count(*) FROM revisions") != 0 {
				t.Fatal("failed settlement did not remain replayable")
			}
			if refresh.requests < 2 {
				t.Fatalf("expired S3 capability was not refreshed: requests=%d error=%v", refresh.requests, err)
			}
			// Replace both doubles after upload, then retry the same durable Node evidence.
			rebootedNode, err = simulator.NewAgentNode(filepath.Join(f.root, "agent-node"))
			must(t, err)
			f.controller = &simulator.Controller{Client: f.client, SubstrateURL: f.external.URL, Executions: f.executions, AgentRuns: controlpb.NewAgentRunServiceClient(f.controlConn), AgentNode: rebootedNode, Git: f.git}
			must(t, f.controller.Acquire(t.Context()))
			fail = false
			_, err = f.controller.Step(t.Context())
			must(t, err)
			if f.scalar("SELECT count(*) FROM revisions WHERE run_id=$1", run) != 1 || f.scalar("SELECT count(*) FROM simulator_delivery_hook") != 1 {
				t.Fatal("restart did not settle exactly once")
			}
			if runGit(t, "-C", checkout, "rev-parse", "HEAD") != branchBefore || runGit(t, "-C", checkout, "status", "--porcelain") != statusBefore {
				t.Fatal("delivery mutated branch or working directory")
			}
			if changed {
				contents, err := os.ReadFile(filepath.Join(checkout, "result.txt"))
				must(t, err)
				if string(contents) != "later checkout edit must not replace the frozen delivery\n" {
					t.Fatal("delivery replay changed the checkout contents")
				}
			}
			var finalCommit string
			must(t, f.store.Pool.QueryRow("SELECT final_commit FROM revisions WHERE run_id=$1", run).Scan(&finalCommit))
			if finalCommit != preparedRef {
				t.Fatal("partial-upload restart replaced the prepared snapshot")
			}
			var historyKey, historyDigest string
			var historySize int64
			must(t, f.store.Pool.QueryRow("SELECT history_key,history_size,history_sha256 FROM revisions WHERE run_id=$1", run).Scan(&historyKey, &historySize, &historyDigest))
			valid, err := objectstore.Verify(t.Context(), cfg, historyKey, historySize, historyDigest)
			must(t, err)
			if !valid || historyKey != work.O("input").S("historyKey") {
				t.Fatal("sealed history was not stored under fixed work key")
			}
			files, err := os.ReadDir(filepath.Join(f.root, "agent-node"))
			must(t, err)
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(f.root, "agent-node", file.Name()))
				must(t, err)
				if strings.Contains(string(data), "X-Amz-Signature") || strings.Contains(string(data), cfg.SecretAccessKey) {
					t.Fatal("Node journal persisted a grant or storage credential")
				}
			}
			_, err = f.store.DeleteRunWorkspace(t.Context(), run)
			must(t, err)
			f.drain()
			if f.scalar("SELECT count(*) FROM workspaces WHERE id=$1 AND deleted_at IS NOT NULL", wid) != 1 || f.scalar("SELECT count(*) FROM revisions WHERE run_id=$1", run) != 1 {
				t.Fatal("release discarded Revision or failed to delete runtime")
			}
		})
	}
}
