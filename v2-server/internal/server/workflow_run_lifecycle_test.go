package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

type fakeRunHost struct {
	started, cancelled, updated, completed int
	lastScope                              server.WorkflowRunScope
	lastInput                              *string
	lastVariables                          map[string]json.RawMessage
	lastNodeID                             string
	lastForce                              bool
	lastRequester                          string
	err                                    error
}

func (f *fakeRunHost) StartRun(_ context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	f.started++
	f.lastScope = scope
	return f.run(runID, store.WorkflowStatusRunning)
}

func (f *fakeRunHost) CancelRun(_ context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	f.cancelled++
	f.lastScope = scope
	return f.run(runID, store.WorkflowStatusCancelled)
}

func (f *fakeRunHost) UpdateRunInput(_ context.Context, scope server.WorkflowRunScope, runID string, input *string, variables map[string]json.RawMessage) (store.WorkflowRun, error) {
	f.updated++
	f.lastScope = scope
	f.lastInput = input
	f.lastVariables = variables
	return f.run(runID, store.WorkflowStatusPending)
}

func (f *fakeRunHost) CompleteNode(_ context.Context, scope server.WorkflowRunScope, runID, nodeID string, force bool, requester string) (store.WorkflowRun, error) {
	f.completed++
	f.lastScope = scope
	f.lastNodeID = nodeID
	f.lastForce = force
	f.lastRequester = requester
	return f.run(runID, store.WorkflowStatusRunning)
}

func (f *fakeRunHost) run(runID, status string) (store.WorkflowRun, error) {
	if f.err != nil {
		return store.WorkflowRun{}, f.err
	}
	now := time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)
	if runID == "" {
		runID = "11111111-1111-4111-8111-111111111111"
	}
	return store.WorkflowRun{
		ID: runID, TenantID: "tenant", WorkflowID: "22222222-2222-4222-8222-222222222222",
		SnapshotID: "33333333-3333-4333-8333-333333333333", ProjectID: "project", Name: "运行",
		Status: status, State: []byte(`{"current_nodes":[]}`), CreatedAt: now, UpdatedAt: now,
	}, nil
}

func lifecycleServer(t *testing.T, host server.WorkflowRunHost) (workflowCall, *fakeRunHost) {
	t.Helper()
	fake, _ := host.(*fakeRunHost)
	call := workflowServer(t, func(o *server.Options) { o.WorkflowRuns = host })
	return call, fake
}

func TestWorkflowRunLifecycleRoutesDelegateToBridge(t *testing.T) {
	host := &fakeRunHost{}
	call, _ := lifecycleServer(t, host)
	tid := workflowTenant(t, call)
	_, me := call("GET", "/api/v1/me", nil)
	runID := "11111111-1111-4111-8111-111111111111"
	base := "/api/v2/tenants/" + tid + "/workflow-runs/" + runID

	status, started := call("POST", base+"/start", nil)
	if status != 200 || started["run"].(map[string]any)["status"] != "running" {
		t.Fatalf("start = %d %v", status, started)
	}
	if host.started != 1 || host.lastScope.TenantID != tid || host.lastScope.UserID != me["id"] {
		t.Fatalf("start bridge call = %+v scope=%+v", host, host.lastScope)
	}

	if status, _ := call("POST", base+"/cancel", nil); status != 200 {
		t.Fatalf("cancel = %d", status)
	}
	if host.cancelled != 1 || host.lastScope.TenantID != tid {
		t.Fatalf("cancel bridge call = %+v", host)
	}

	input := "补充输入"
	status, updated := call("PATCH", base+"/input", map[string]any{"input": input, "variables": map[string]any{"count": 3}})
	if status != 200 || updated["run"] == nil {
		t.Fatalf("input = %d %v", status, updated)
	}
	if host.updated != 1 || host.lastInput == nil || *host.lastInput != input || string(host.lastVariables["count"]) != "3" {
		t.Fatalf("input bridge call = %+v input=%v variables=%v", host, host.lastInput, host.lastVariables)
	}

	status, completed := call("POST", base+"/nodes/agent-1/complete", map[string]any{"force": true, "requester": "human"})
	if status != 200 || completed["run"] == nil {
		t.Fatalf("complete = %d %v", status, completed)
	}
	if host.completed != 1 || host.lastNodeID != "agent-1" || !host.lastForce || host.lastRequester != "human" {
		t.Fatalf("complete bridge call = %+v", host)
	}

	// A malformed id never reaches the bridge.
	if status, _ := call("POST", "/api/v2/tenants/"+tid+"/workflow-runs/not-a-uuid/start", nil); status != 404 {
		t.Fatalf("malformed id = %d", status)
	}
	// Only the human requester is accepted at this phase.
	status, failure := call("POST", base+"/nodes/agent-1/complete", map[string]any{"force": false, "requester": "agent"})
	if status != 400 || failure["code"] != "invalid_json" {
		t.Fatalf("unknown requester = %d %v", status, failure)
	}
}

func TestWorkflowRunLifecycleRefusals(t *testing.T) {
	runBase := func(tid string) string {
		return "/api/v2/tenants/" + tid + "/workflow-runs/11111111-1111-4111-8111-111111111111"
	}
	for _, tc := range []struct {
		name   string
		err    error
		code   string
		want   int
		method string
		suffix string
		body   any
	}{
		{"not startable", server.ErrWorkflowRunNotStartable, "workflow_run_start_failed", 400, "POST", "/start", nil},
		{"not editable", server.ErrWorkflowRunNotEditable, "workflow_run_not_editable", 409, "PATCH", "/input", map[string]any{"input": "x"}},
		{"node not parked", server.ErrWorkflowNodeNotAwaitingInput, "workflow_node_not_awaiting_input", 400, "POST", "/nodes/agent-1/complete", map[string]any{"force": false}},
		{"node missing", server.ErrWorkflowNodeNotFound, "workflow_node_not_found", 404, "POST", "/nodes/agent-1/complete", map[string]any{"force": false}},
		{"run missing", store.ErrWorkflowRunNotFound, "workflow_run_not_found", 404, "POST", "/start", nil},
		{"stop unconfirmed", server.ErrWorkflowRunStopUnconfirmed, "workflow_run_stop_unconfirmed", 503, "POST", "/cancel", nil},
		{"input invalid", &server.WorkflowRunInputInvalid{Variable: "count", Reason: "must be a number"}, "workflow_run_input_invalid", 400, "PATCH", "/input", map[string]any{"input": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &fakeRunHost{err: tc.err}
			call, _ := lifecycleServer(t, host)
			tid := workflowTenant(t, call)
			status, failure := call(tc.method, runBase(tid)+tc.suffix, tc.body)
			if status != tc.want || failure["code"] != tc.code {
				t.Fatalf("refusal = %d %v", status, failure)
			}
			if tc.code == "workflow_run_input_invalid" {
				params := failure["params"].(map[string]any)
				if params["variable"] != "count" || params["reason"] != "must be a number" {
					t.Fatalf("params = %v", params)
				}
			}
		})
	}
}

func TestWorkflowRunLifecycleNeedsBridge(t *testing.T) {
	call := workflowServer(t, nil)
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflow-runs/11111111-1111-4111-8111-111111111111"
	for _, route := range []struct{ method, path string }{
		{"POST", base + "/start"},
		{"POST", base + "/cancel"},
		{"PATCH", base + "/input"},
		{"POST", base + "/nodes/agent-1/complete"},
	} {
		status, failure := call(route.method, route.path, map[string]any{"input": nil, "force": false})
		if status != http.StatusServiceUnavailable || failure["code"] != "workflow_run_host_unavailable" {
			t.Fatalf("%s %s = %d %v", route.method, route.path, status, failure)
		}
	}
}
