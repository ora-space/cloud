package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// A revoked reader receives only a contentless clearing snapshot and no subsequent private text.
func TestThreadPreviewSSEStopsAndClearsAfterMembershipRevocation(t *testing.T) {
	f := setup(t)
	_, err := f.store.Pool.Exec("DELETE FROM controller_leases")
	must(t, err)
	scene := seedAgentSessionScene(t, f.store)
	var tenant, issue string
	must(t, f.store.Pool.QueryRow("SELECT tenant_id,issue_id FROM issue_runs WHERE id=$1", scene.runID).Scan(&tenant, &issue))
	_, err = f.store.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", tenant, f.uid)
	must(t, err)
	snapshot := core.ThreadPreviewSnapshot{Available: true, ExecutionID: scene.executionID, Version: 1, Blocks: []core.ThreadPreviewBlock{{HistorySequence: 3, Record: json.RawMessage(`{"text":"authorized content"}`)}}}
	f.store.ThreadPreviews.Publish(scene.runID, snapshot)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", f.cloud.URL+"/api/v1/tenants/"+tenant+"/issues/"+issue+"/runs/"+scene.runID+"/thread/preview/events", nil)
	must(t, err)
	service, err := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(t, err)
	user := f.user
	user.Caller = "gateway-a"
	credential, err := f.client.Credentials.Token("user", user)
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+service)
	request.Header.Set("X-Ora-User-Token", credential)
	response, err := f.cloud.Client().Do(request)
	must(t, err)
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("SSE open: %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	must(t, err)
	if !strings.Contains(first, "authorized content") {
		t.Fatalf("missing initial preview: %s", first)
	}
	_, err = reader.ReadString('\n')
	must(t, err)
	_, err = f.store.Pool.Exec("UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2", tenant, f.uid)
	must(t, err)
	snapshot.Version = 2
	snapshot.Blocks[0].Record = json.RawMessage(`{"text":"must never reach revoked reader"}`)
	f.store.ThreadPreviews.Publish(scene.runID, snapshot)
	rest, err := io.ReadAll(reader)
	must(t, err)
	if strings.Contains(string(rest), "must never") || !strings.Contains(string(rest), `"available":false`) {
		t.Fatalf("revoked subscription was not cleared: %s", rest)
	}
}

// Optional preview publication uses the real authenticated RPC but cannot create an Ack basis.
func TestThreadPreviewRPCDoesNotCreateReceipt(t *testing.T) {
	h := newControlHarness(t)
	scene := seedAgentSessionScene(t, h.store)
	client := controlpb.NewAgentRunServiceClient(h.conn)
	request := &controlpb.PublishThreadPreviewRequest{Epoch: 1, OperationId: scene.runID, ExecutionId: scene.executionID, Version: 1, Blocks: []*controlpb.ThreadPreviewBlock{{HistorySequence: 4, TurnId: scene.initialTurnID, Record: threadLine("update", "first streamed text")}}}
	_, err := client.PublishThreadPreview(asController("ctrl-a"), request)
	must(t, err)
	if snapshot := h.store.ThreadPreviews.Snapshot(scene.runID); !snapshot.Available || len(snapshot.Blocks) != 1 {
		t.Fatalf("RPC failed to publish optional snapshot: %v", snapshot)
	}
	var receipts int
	must(t, h.store.Pool.QueryRow("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", scene.executionID).Scan(&receipts))
	if receipts != 0 {
		t.Fatal("preview created a durable acknowledgement receipt")
	}
	request.ExecutionId = "foreign-execution"
	if _, err := client.PublishThreadPreview(asController("ctrl-a"), request); err == nil {
		t.Fatal("RPC accepted a foreign execution preview")
	}
}

func TestMigration0047PreservesExistingThreadContent(t *testing.T) {
	pool, _ := testSchema(t, "test_preview46_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0047_thread_preview_positions.sql"))
	seed := seedOwnershipChain(t, pool)
	_, err := pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record) VALUES($1,1,'node','update','{"type":"update","original":true}')`, seed.runID)
	must(t, err)
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	var position *int64
	var raw string
	must(t, pool.QueryRow("SELECT history_sequence,record->>'original' FROM thread_entries WHERE run_id=$1", seed.runID).Scan(&position, &raw))
	if position != nil || raw != "true" {
		t.Fatal("preview migration reinterpreted an existing raw record")
	}
}
