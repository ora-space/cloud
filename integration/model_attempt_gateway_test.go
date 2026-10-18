package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/modelgateway"
)

// This fixture stages an active synthetic Agent session and its model binding. It exercises real
// grant authorization, HTTP/TLS, attempt persistence and authenticated public statistics, not a real
// provider or Node model invocation. Secrets exist only in memory and an isolated encrypted schema.
func seedGatewayModelGrant(t *testing.T, f *fixture, protocol string, cipher *modelgateway.Cipher) (run, base, token string) {
	t.Helper()
	session := seedAgentSessionScene(t, f.store)
	var tenant, issue, workspace string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id,issue_id,workspace_id FROM issue_runs WHERE id=$1`, session.runID).Scan(&tenant, &issue, &workspace))
	_, err := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenant, f.uid)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE node_instances SET model_proxy=true WHERE node_id=$1`, session.nodeID)
	must(t, err)
	var epoch, version int64
	must(t, f.store.Pool.QueryRow(`SELECT control_epoch,version FROM runtime_controls WHERE workspace_id=$1`, workspace).Scan(&epoch, &version))
	_, err = f.store.Control(t.Context(), &core.ControlRequest{
		Action: "runtime_ack", Service: &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}},
		Body: core.Object{"workspaceId": workspace, "controlEpoch": epoch, "controlVersion": version, "nodeInstanceId": session.nodeID, "inputClosed": false, "unfinishedExecutionIds": []any{}, "epoch": 1},
	})
	must(t, err)
	model := core.Object{"id": "synthetic/model", "name": "Synthetic model", "contextWindow": 128000, "maxTokens": 8192}
	connection := f.call("POST", "/api/v1/me/model-connections", core.Object{
		"name": "Synthetic metrics", "protocol": protocol, "baseUrl": "https://synthetic-model.example.invalid/v1", "authMode": "bearer", "models": []any{model},
	}, uuid.NewString(), 201).O("resource")
	credential, binding := uuid.NewString(), uuid.NewString()
	secret := make([]byte, 32)
	_, err = rand.Read(secret)
	must(t, err)
	sealed, err := cipher.Seal(credential, []byte(hex.EncodeToString(secret)))
	clear(secret)
	must(t, err)
	updated, err := f.store.PutModelCredential(t.Context(), &f.user, connection.S("id"), connection.N("version"), "", credential, sealed, "synthetic-metrics-v1")
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO run_model_bindings(id,run_id,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
	 VALUES($1,$2,$3,$4,$5,$6,'Synthetic metrics',$7,'https://synthetic-model.example.invalid/v1','bearer',$8)`, binding, session.runID, f.uid, connection.S("id"), updated.N("version"), credential, protocol, mustJSON(t, model))
	must(t, err)
	token = uuid.NewString() + uuid.NewString()
	digest := sha256.Sum256([]byte(token))
	_, err = f.store.CreateModelGrant(t.Context(), core.ModelRuntimeScope{TenantID: tenant, WorkspaceID: workspace, Generation: 1}, binding, session.executionID, hex.EncodeToString(digest[:]))
	must(t, err)
	base = "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + session.runID + "/"
	return session.runID, base, token
}

func TestModelGatewayPersistsActualAttemptsThroughStatisticsHTTP(t *testing.T) {
	for _, protocol := range []string{"openai-completions", "anthropic-messages"} {
		t.Run(protocol, func(t *testing.T) {
			f := setup(t)
			key := make([]byte, 32)
			_, err := rand.Read(key)
			must(t, err)
			cipher, err := modelgateway.NewCipher(key, "synthetic-metrics-v1")
			clear(key)
			must(t, err)
			complete, partial, missing := syntheticMetricFrames(protocol)
			var requests atomic.Int64
			var run string
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// One available SQL connection proves registration released its transaction before I/O.
				var started int
				queryErr := f.store.Pool.QueryRowContext(r.Context(), `SELECT count(*) FROM model_request_attempts WHERE run_id=$1 AND outcome='started'`, run).Scan(&started)
				if queryErr != nil || started != 1 {
					t.Error("upstream did not observe exactly one committed dispatch intent")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				index := requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				switch index {
				case 1:
					_, _ = io.WriteString(w, complete)
				case 2:
					_, _ = io.WriteString(w, missing)
				case 3:
					_, _ = io.WriteString(w, partial)
				case 4:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic private provider diagnostic"}}`)
				case 5:
					_, _ = io.WriteString(w, partial)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					t.Error("gateway retried the upstream")
				}
			}))
			t.Cleanup(upstream.Close)
			var base, token string
			run, base, token = seedGatewayModelGrant(t, f, protocol, cipher)
			f.store.Pool.SetMaxOpenConns(1)
			auth, err := core.NewAuthenticator("ora-cloud", f.client.Credentials.Trust)
			must(t, err)
			target, err := url.Parse(upstream.URL)
			must(t, err)
			clientTransport := fixtureModelTransport{target: target, transport: upstream.Client().Transport}
			gateway, err := modelgateway.New(&modelgateway.Options{Store: f.store, Auth: auth, Cipher: cipher, PublicOrigin: "https://models.example.invalid", Upstream: &http.Client{Transport: clientTransport}})
			must(t, err)
			t.Cleanup(gateway.Close)
			finalized := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gateway.ModelHandler(w, r)
				finalized <- struct{}{}
			}))
			t.Cleanup(server.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			path := "/runtime/openai/v1/chat/completions"
			if protocol == "anthropic-messages" {
				path = "/runtime/anthropic/v1/messages"
			}
			for index, expected := range []string{complete, missing, partial, ""} {
				r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, strings.NewReader(`{"model":"synthetic/model","stream":true,"messages":[]}`))
				must(t, err)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				response, err := client.Do(r)
				must(t, err)
				data, err := io.ReadAll(response.Body)
				must(t, response.Body.Close())
				must(t, err)
				select {
				case <-finalized:
				case <-time.After(5 * time.Second):
					t.Fatal("completed request retained its finalization")
				}
				if index < 3 && (response.StatusCode != 200 || string(data) != expected) {
					t.Fatalf("successful response bytes changed at attempt %d", index)
				}
				if index == 3 && (response.StatusCode != 502 || strings.Contains(string(data), "private provider")) {
					t.Fatal("upstream error was not safely preserved")
				}
			}
			cancelled, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, err := http.NewRequestWithContext(cancelled, http.MethodPost, server.URL+path, strings.NewReader(`{"model":"synthetic/model","stream":true,"messages":[]}`))
			must(t, err)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			response, err := client.Do(r)
			must(t, err)
			_, err = io.ReadFull(response.Body, make([]byte, len(partial)))
			must(t, err)
			cancel()
			must(t, response.Body.Close())
			select {
			case <-finalized:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled request retained its finalization")
			}
			statistics := f.call("GET", base+"statistics", nil, "", 200)
			counts := statistics.O("counts")
			if requests.Load() != 5 || counts.N("total") != 5 || counts.N("succeeded") != 2 || counts.N("failed") != 1 || counts.N("incomplete") != 1 || counts.N("cancelled") != 1 || counts.N("started") != 0 {
				t.Fatalf("actual attempt counts=%v requests=%d", counts, requests.Load())
			}
			for _, field := range []string{"inputTokens", "outputTokens"} {
				counter := statistics.O("usage").O(field)
				want := int64(22)
				if field == "outputTokens" {
					want = 7
				}
				if counter.N("value") != want || counter.N("knownCount") != 3 || counter.N("totalCount") != 5 || counter.S("availability") != "partial" {
					t.Fatalf("%s lost snapshot/coverage semantics: %v", field, counter)
				}
			}
			for field, want := range map[string]int64{"cacheReadTokens": 8, "cacheWriteTokens": 2, "reasoningTokens": 1} {
				counter := statistics.O("usage").O(field)
				if counter.N("value") != want || counter.N("knownCount") != 1 {
					t.Fatalf("%s did not survive real persistence: %v", field, counter)
				}
			}
			calls := threadItems(t, f.call("GET", base+"model-calls", nil, "", 200))
			if len(calls) != 5 || calls[0].O("usage").S("availability") != "complete" || calls[1].O("usage").S("availability") != "unavailable" || calls[2].B("terminalObserved") || calls[4].S("outcome") != "cancelled" || calls[4].O("usage").S("availability") != "partial" {
				t.Fatalf("public attempt evidence=%v", calls)
			}
		})
	}
}

// Only the network destination is replaced, keeping configured public URLs and real TLS/HTTP.
// This is an injected test transport, not a production exemption for private provider addresses.
type fixtureModelTransport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (f fixtureModelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	request.URL.Scheme, request.URL.Host = f.target.Scheme, f.target.Host
	return f.transport.RoundTrip(request)
}

func (f fixtureModelTransport) CloseIdleConnections() {
	if closer, ok := f.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func syntheticMetricFrames(protocol string) (complete, partial, missing string) {
	if protocol == "anthropic-messages" {
		start := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1,\"cache_read_input_tokens\":8,\"cache_creation_input_tokens\":2}}}\n\n"
		finish := "data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":3,\"output_tokens_details\":{\"thinking_tokens\":1}}}\n\n"
		missing = "data: {\"type\":\"message_stop\"}\n\n"
		return start + finish + finish + missing, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n", missing
	}
	// OpenRouter intentionally repeats a finished, content-free choice in its final usage frame.
	finish := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\",\"role\":\"assistant\"},\"finish_reason\":\"stop\",\"native_finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":8,\"cache_write_tokens\":2},\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n"
	missing = "data: [DONE]\n\n"
	return finish + finish + missing, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n", missing
}
