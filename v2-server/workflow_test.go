package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/workflowhost"
)

// TestWorkflowDomainServesAnalyzeAndPublish drives the assembled server, so the routes answer with
// the real desktop-equivalent rules rather than 503 or a stub.
func TestWorkflowDomainServesAnalyzeAndPublish(t *testing.T) {
	s := storetest.Open(t)
	storetest.SignIn(t, s, "workflow-wiring", "Owner", store.RoleOwner)
	srv := httptest.NewServer(server.New(s, withWorkflowDomain(server.Options{SessionTTL: time.Hour}, nil)))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	call := func(method, path string, input any) (int, map[string]any) {
		t.Helper()
		var body io.Reader
		if input != nil {
			b, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out := map[string]any{}
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil && err != io.EOF {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
		return res.StatusCode, out
	}
	call("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("workflow-wiring")})
	_, tenants := call("GET", "/api/v1/me/tenants", nil)
	base := "/api/v2/tenants/" + tenants["items"].([]any)[0].(map[string]any)["id"].(string) + "/workflows"

	raw, err := os.ReadFile(filepath.Join("internal", "workflow", "testdata", "spare-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	spareChain := string(raw)

	status, analysis := call("POST", base+"/analyze", map[string]any{"graph": spareChain})
	want := map[string]any{"unusedNodeIds": []any{"other", "spare"}, "unrecognizedNodes": []any{}}
	if status != http.StatusOK || !reflect.DeepEqual(analysis, want) {
		t.Fatalf("analyze spare chain = %d %v, want %v", status, analysis, want)
	}
	status, failure := call("POST", base+"/analyze", map[string]any{"graph": "{}"})
	if status != http.StatusBadRequest || failure["code"] != "workflow_run_graph_parse" {
		t.Fatalf("analyze unparsable graph = %d %v", status, failure)
	}

	_, created := call("POST", base, map[string]any{"name": "备用链", "graph": spareChain})
	id := created["workflow"].(map[string]any)["id"].(string)
	// Spare nodes may stay unfinished: publish validates only the reachable projection.
	if status, published := call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"}); status != http.StatusOK {
		t.Fatalf("publish spare chain = %d %v", status, published)
	}
	_, before := call("GET", base+"/"+id+"/versions", nil)

	// Reconnecting the unfinished Agent makes its configuration part of the executable graph.
	reconnected := strings.Replace(spareChain, `{"source":"start","target":"out"}`, `{"source":"start","target":"spare"}`, 1)
	if status, saved := call("PUT", base+"/"+id+"/draft", map[string]any{"graph": reconnected}); status != http.StatusOK {
		t.Fatalf("save reconnected draft = %d %v", status, saved)
	}
	status, failure = call("POST", base+"/"+id+"/publish", map[string]any{"version": "v2"})
	if status != http.StatusBadRequest || failure["code"] != "workflow_run_graph_parse" {
		t.Fatalf("publish reconnected spare = %d %v", status, failure)
	}
	if _, after := call("GET", base+"/"+id+"/versions", nil); !reflect.DeepEqual(after, before) {
		t.Fatalf("refused publish changed versions: %v -> %v", before, after)
	}
}

// TestWorkflowDomainStreamsTheHostsRunChanges subscribes the run events route to the hub the host's
// engine publishes on, so a committed change reaches an open run page.
func TestWorkflowDomainStreamsTheHostsRunChanges(t *testing.T) {
	s := storetest.Open(t)
	stub := &executiontest.Stub{}
	host, err := workflowhost.NewHost(workflowhost.Config{Store: s, Execution: stub, Catalog: stub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	if o := withWorkflowDomain(server.Options{}, host); o.WorkflowRunEvents != server.WorkflowRunEvents(host.Hub()) {
		t.Fatalf("run events = %v, want the host's hub", o.WorkflowRunEvents)
	}
	if o := withWorkflowDomain(server.Options{}, nil); o.WorkflowRunEvents != nil {
		t.Fatalf("run events without a host = %v", o.WorkflowRunEvents)
	}
}
