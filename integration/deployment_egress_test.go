package integration

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/egress"
	"github.com/wanglongan587/cloud/internal/modelgateway"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// This resolver uses real DNS-over-TCP and probes the sole SQL connection during lookup.
// If admission holds a SQL transaction, the probe fails instead of hanging the suite.
func deploymentDNS(t *testing.T, f *fixture, private bool) (string, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	var fail atomic.Bool
	var calls atomic.Int32
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				calls.Add(1)
				var length [2]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					return
				}
				wire := make([]byte, int(binary.BigEndian.Uint16(length[:])))
				if _, err := io.ReadFull(conn, wire); err != nil {
					return
				}
				var q dnsmessage.Message
				if q.Unpack(wire) != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := f.store.Pool.PingContext(ctx); err != nil {
					t.Error("DNS called while SQL connection held")
				}
				a := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true}, Questions: q.Questions}
				question := q.Questions[0]
				if fail.Load() {
					a.RCode = dnsmessage.RCodeServerFailure
				} else if question.Type == dnsmessage.TypeA {
					ip := [4]byte{7, 1, 2, 3}
					if private {
						ip = [4]byte{10, 60, 0, 10}
					}
					a.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: question.Class}, Body: &dnsmessage.AResource{A: ip}}}
				}
				data, err := a.Pack()
				if err != nil {
					t.Error(err)
					return
				}
				binary.BigEndian.PutUint16(length[:], uint16(len(data)))
				_, _ = conn.Write(append(length[:], data...))
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); <-done; wg.Wait() })
	return l.Addr().String(), &fail, &calls
}

func deploymentPolicy(t *testing.T, address string) *egress.Policy {
	t.Helper()
	p, err := egress.Parse([]byte(fmt.Sprintf(`{"schemaVersion":1,"mode":"enterprise","resolver":{"transport":"tcp","address":%q},"targets":[{"host":"model.example.invalid","port":443},{"host":"assets.example.invalid","port":443}],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`, address)))
	must(t, err)
	return p
}

func TestDeploymentEgressSaveReplayAndPermissionReduction(t *testing.T) {
	f := setup(t)
	f.store.Pool.SetMaxOpenConns(1)
	address, fail, calls := deploymentDNS(t, f, false)
	policy := deploymentPolicy(t, address)
	f.store.ModelEgress = policy
	body := core.Object{"name": "Enterprise", "protocol": "openai-completions", "baseUrl": "https://model.example.invalid/v1", "authMode": "bearer", "models": []any{map[string]any{"id": "model", "name": "Model", "contextWindow": 128000, "maxTokens": 8192}}}
	created := f.call("POST", "/api/v1/me/model-connections", body, "enterprise-create", 201).O("resource")
	if calls.Load() != 2 {
		t.Fatal("save did not validate A and AAAA")
	}
	fail.Store(true)
	replayed := f.call("POST", "/api/v1/me/model-connections", body, "enterprise-create", 201).O("resource")
	if replayed.S("id") != created.S("id") || calls.Load() != 2 {
		t.Fatal("replay repeated network admission")
	}
	f.call("POST", "/api/v1/me/model-connections", body, "enterprise-create-fail", 400)
	if f.scalar("SELECT count(*) FROM personal_model_connections") != 1 {
		t.Fatal("failed DNS wrote a version")
	}
	before := calls.Load()
	body["version"] = created.N("version")
	body["enabled"] = false
	updated := f.call("PUT", "/api/v1/me/model-connections/"+created.S("id"), body, "", 200).O("resource")
	if updated.B("enabled") || calls.Load() != before {
		t.Fatal("pure disable used DNS")
	}
	body["version"] = updated.N("version")
	body["name"] = "Smuggled change"
	f.call("PUT", "/api/v1/me/model-connections/"+created.S("id"), body, "", 400)
	if f.scalar("SELECT version FROM personal_model_connections WHERE id=$1", created.S("id")) != int(updated.N("version")) {
		t.Fatal("changed configuration bypassed admission")
	}
	f.user.Subject = "other-user"
	before = calls.Load()
	f.call("PUT", "/api/v1/me/model-connections/"+created.S("id"), body, "", 404)
	if calls.Load() != before {
		t.Fatal("unauthorized caller caused DNS")
	}
	f.user.Subject = "alice"
	f.call("DELETE", "/api/v1/me/model-connections/"+created.S("id"), core.Object{"version": updated.N("version")}, "enterprise-delete", 200)
	if calls.Load() != before {
		t.Fatal("delete depended on DNS")
	}
	body["baseUrl"] = "https://unlisted.example.invalid/v1"
	f.call("POST", "/api/v1/me/model-connections", body, "unlisted", 400)
	if calls.Load() != before {
		t.Fatal("unlisted borrowed-public target queried DNS")
	}
}

func TestDeploymentEgressAssetDNSRejectionLeavesNoDocuments(t *testing.T) {
	f := setup(t)
	key := make([]byte, 32)
	_, err := rand.Read(key)
	must(t, err)
	cipher, err := modelgateway.NewCipher(key, "synthetic-metrics-v1")
	clear(key)
	must(t, err)
	run, _, token := seedGatewayModelGrant(t, f, "openai-completions", cipher)
	_, err = f.store.Pool.Exec("UPDATE run_model_bindings SET base_url='https://model.example.invalid/v1' WHERE run_id=$1", run)
	must(t, err)
	f.store.Pool.SetMaxOpenConns(1)
	address, _, calls := deploymentDNS(t, f, true)
	policy := deploymentPolicy(t, address)
	client, err := modelgateway.NewUpstreamClient(modelgateway.UpstreamConfig{Policy: policy})
	must(t, err)
	t.Cleanup(client.CloseIdleConnections)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, client.Transport)
	repo := workflowMarketplace(t, f.root)
	pkg := workflowPackage(t, map[string]string{"release.json": executableDocument("Blocked asset", "")})
	publishWorkflowRelease(t, repo, "egress-kit", "1.0.0", "https://assets.example.invalid/package.orax?signature=a%2Fb", pkg)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/egress-kit", "")
	if err := f.importWorkflowAssets(t); err == nil {
		t.Fatal("DNS denial did not fail the package")
	}
	item := f.assetImport(t, sid, "official/egress-kit")
	if item.S("state") != "failed" || len(assetDocuments(item)) != 0 || f.scalar("SELECT count(*) FROM workflows") != 0 {
		t.Fatalf("DNS rejection leaked document results: %v", item)
	}
	if calls.Load() != 2 {
		t.Fatal("asset Fetch did not use deployment TCP resolver")
	}
	if strings.Contains(fmt.Sprint(item), "signature=") {
		t.Fatal("signed URL leaked into failure")
	}
	auth, err := core.NewAuthenticator("ora-cloud", f.client.Credentials.Trust)
	must(t, err)
	gateway, err := modelgateway.New(&modelgateway.Options{Store: f.store, Auth: auth, Cipher: cipher, PublicOrigin: "https://model.example.invalid", Upstream: client})
	must(t, err)
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(http.HandlerFunc(gateway.ModelHandler))
	t.Cleanup(server.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"synthetic/model"}`))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	must(t, err)
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK || calls.Load() != 4 {
		t.Fatalf("model did not use same DNS rejection: status=%d calls=%d", response.StatusCode, calls.Load())
	}
	if f.scalar("SELECT count(*) FROM run_model_bindings WHERE run_id=$1 AND revoked_at IS NOT NULL", run) != 0 {
		t.Fatal("network denial became permanent revocation")
	}
}

func TestDeploymentEgressRevocationSurvivesRemovedTarget(t *testing.T) {
	for _, action := range []string{"disable", "delete", "clear-key"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			key := make([]byte, 32)
			_, err := rand.Read(key)
			must(t, err)
			cipher, err := modelgateway.NewCipher(key, "synthetic-metrics-v1")
			clear(key)
			must(t, err)
			run, _, _ := seedGatewayModelGrant(t, f, "openai-completions", cipher)
			var connection string
			must(t, f.store.Pool.QueryRow("SELECT connection_id FROM run_model_bindings WHERE run_id=$1", run).Scan(&connection))
			body := f.call("GET", "/api/v1/me/model-connections/"+connection, nil, "", 200)
			// A restarted enterprise policy has no target permission. No DNS server is configured.
			denied, err := egress.Parse([]byte(`{"schemaVersion":1,"mode":"enterprise","targets":[],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`))
			must(t, err)
			f.store.ModelEgress = denied
			switch action {
			case "disable":
				body["enabled"] = false
				for _, field := range []string{"id", "credentialConfigured", "createdAt", "updatedAt"} {
					delete(body, field)
				}
				f.call("PUT", "/api/v1/me/model-connections/"+connection, body, "", 200)
			case "delete":
				f.call("DELETE", "/api/v1/me/model-connections/"+connection, core.Object{"version": body.N("version")}, "removed-delete", 200)
			case "clear-key":
				_, err = f.store.ClearModelCredential(t.Context(), &f.user, connection, body.N("version"), "removed-clear")
				must(t, err)
			}
			if f.scalar("SELECT count(*) FROM run_model_bindings WHERE run_id=$1 AND revoked_at IS NOT NULL", run) != 1 || f.scalar("SELECT count(*) FROM model_access_grants WHERE revoked_at IS NULL") != 0 {
				t.Fatal("removed target prevented permanent revocation")
			}
			if action == "disable" {
				allowed, err := egress.Parse([]byte(`{"schemaVersion":1,"mode":"enterprise","targets":[{"host":"synthetic-model.example.invalid","port":443,"staticAddresses":["7.1.2.3"]}],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`))
				must(t, err)
				f.store.ModelEgress = allowed
				body["version"] = body.N("version") + 1
				body["enabled"] = true
				f.call("PUT", "/api/v1/me/model-connections/"+connection, body, "", 200)
				if f.scalar("SELECT count(*) FROM run_model_bindings WHERE run_id=$1 AND revoked_at IS NOT NULL", run) != 1 {
					t.Fatal("re-enable revived revoked binding")
				}
			}
		})
	}
}
