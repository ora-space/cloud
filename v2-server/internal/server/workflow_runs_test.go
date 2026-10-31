package server_test

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func workflowRunProject(t *testing.T, s *store.Store, user, tenant string) store.Project {
	t.Helper()
	project, err := s.CreateProject(context.Background(), execution.Scope{TenantID: tenant, UserID: user}, store.Project{
		ID:            "project-runs",
		Name:          "演示项目",
		RepositoryURL: "https://example.invalid/demo.git",
		Branch:        "main",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
		MirrorPath:    "/tmp/project-runs",
		SnapshotRef:   "refs/ora/snapshots/project-runs",
	})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestWorkflowRunsOverHTTP(t *testing.T) {
	call, s := workflowServerWithStore(t, nil)
	ctx := context.Background()
	tid := workflowTenant(t, call)
	_, me := call("GET", "/api/v1/me", nil)
	user := me["id"].(string)
	project := workflowRunProject(t, s, user, tid)
	base := "/api/v2/tenants/" + tid + "/workflows"

	_, created := call("POST", base, map[string]any{"name": "运行契约", "graph": nil})
	workflow := created["workflow"].(map[string]any)
	draft := created["draft"].(map[string]any)
	id := workflow["id"].(string)
	_, published := call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	snapshotID := published["snapshot"].(map[string]any)["id"].(string)

	status, failure := call("POST", base+"/"+id+"/runs", map[string]any{"projectId": ""})
	if status != 400 || failure["code"] != "invalid_configuration" {
		t.Fatalf("missing project = %d %v", status, failure)
	}
	status, failure = call("POST", base+"/"+id+"/runs", map[string]any{"projectId": project.ID, "snapshotId": draft["id"]})
	if status != 400 || failure["code"] != "workflow_run_cannot_use_draft_snapshot" {
		t.Fatalf("draft run = %d %v", status, failure)
	}
	status, failure = call("POST", base+"/"+id+"/runs", map[string]any{"projectId": "missing"})
	if status != 404 || failure["code"] != "project_not_found" {
		t.Fatalf("unknown project = %d %v", status, failure)
	}
	status, failure = call("POST", "/api/v2/tenants/"+tid+"/workflows/00000000-0000-0000-0000-000000000000/runs", map[string]any{"projectId": project.ID})
	if status != 404 || failure["code"] != "workflow_not_found" {
		t.Fatalf("unknown workflow = %d %v", status, failure)
	}

	status, createdRun := call("POST", base+"/"+id+"/runs", map[string]any{"projectId": project.ID, "input": "开始吧"})
	if status != 201 {
		t.Fatalf("create run = %d %v", status, createdRun)
	}
	run := createdRun["run"].(map[string]any)
	runID := run["id"].(string)
	if run["status"] != "pending" || run["snapshotId"] != snapshotID || run["projectId"] != project.ID {
		t.Fatalf("run = %v", run)
	}
	if run["name"] != "运行契约" || run["input"] != "开始吧" || run["startedAt"] != nil {
		t.Fatalf("frozen run fields = %v", run)
	}
	if run["state"] != `{"current_nodes": []}` {
		t.Fatalf("initial state = %v", run["state"])
	}

	_, listed := call("GET", base+"/"+id+"/runs", nil)
	runs := listed["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("workflow runs = %v", listed)
	}
	summary := runs[0].(map[string]any)
	if summary["version"] != "v1" || summary["id"] != runID || summary["projectId"] != project.ID {
		t.Fatalf("summary = %v", summary)
	}
	_, byProject := call("GET", "/api/v2/tenants/"+tid+"/workflow-runs?projectId="+project.ID, nil)
	if len(byProject["runs"].([]any)) != 1 {
		t.Fatalf("project runs = %v", byProject)
	}

	// A live run freezes v1: neither that version nor the definition may disappear under it.
	if status, _ := call("POST", base+"/"+id+"/publish", map[string]any{"version": "v2"}); status != 200 {
		t.Fatalf("publish v2 = %d", status)
	}
	status, failure = call("DELETE", base+"/"+id+"/versions/v1", nil)
	if status != 400 || failure["code"] != "workflow_snapshot_in_use" {
		t.Fatalf("delete referenced version = %d %v", status, failure)
	}
	status, failure = call("DELETE", base+"/"+id, nil)
	if status != 400 || failure["code"] != "workflow_active_runs" {
		t.Fatalf("delete referenced workflow = %d %v", status, failure)
	}

	_, detail := call("GET", "/api/v2/tenants/"+tid+"/workflow-runs/"+runID, nil)
	if len(detail["nodes"].([]any)) != 0 || len(detail["scopes"].([]any)) != 0 {
		t.Fatalf("empty detail = %v", detail)
	}

	// The execution bridge owns starting and dispatching; parking is what turns the run into the
	// derived awaitingInput the page renders.
	if _, err := s.StartWorkflowRun(ctx, tid, runID); err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateWorkflowNodeRun(ctx, tid, runID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParkWorkflowNodeRun(ctx, tid, runID, node.ID); err != nil {
		t.Fatal(err)
	}
	_, detail = call("GET", "/api/v2/tenants/"+tid+"/workflow-runs/"+runID, nil)
	run = detail["run"].(map[string]any)
	if run["status"] != "awaitingInput" || run["startedAt"] == nil {
		t.Fatalf("derived status = %v", run)
	}
	nodes := detail["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %v", detail["nodes"])
	}
	nodeView := nodes[0].(map[string]any)
	if nodeView["status"] != "pending" || nodeView["scopeId"] != "root:"+runID {
		t.Fatalf("node view = %v", nodeView)
	}
	_, nodeList := call("GET", "/api/v2/tenants/"+tid+"/workflow-runs/"+runID+"/nodes", nil)
	if len(nodeList["nodes"].([]any)) != 1 {
		t.Fatalf("node list = %v", nodeList)
	}

	status, failure = call("GET", "/api/v2/tenants/"+tid+"/workflow-runs/00000000-0000-0000-0000-000000000000", nil)
	if status != 404 || failure["code"] != "workflow_run_not_found" {
		t.Fatalf("unknown run = %d %v", status, failure)
	}
	status, failure = call("GET", "/api/v2/tenants/"+tid+"/workflow-runs/not-a-uuid", nil)
	if status != 404 || failure["code"] != "workflow_run_not_found" {
		t.Fatalf("malformed run id = %d %v", status, failure)
	}
}
