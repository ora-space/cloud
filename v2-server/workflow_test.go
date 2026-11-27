package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
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
	want := map[string]any{"unusedNodeIds": []any{"other", "spare"}, "unrecognizedNodes": []any{}, "unsupportedNodes": []any{},
		// The spare root Agent has desktop's default retry, which v2 applies to root Agents.
		"unappliedSettings": []any{}}
	if status != http.StatusOK || !reflect.DeepEqual(analysis, want) {
		t.Fatalf("analyze spare chain = %d %v, want %v", status, analysis, want)
	}
	// Roles, Skills, MCP and retry all apply now, so the one setting the analysis still lists is a
	// Loop member's interactive mode: v2 runs that member automatically where desktop parks it.
	// Prompt and tool stay unsupported, as on desktop.
	unapplied := `{"schemaVersion":2,"nodes":[{"id":"start","data":{"kind":"start"}},
		{"id":"writer","data":{"kind":"agent","agentConfig":{"prompt":"x","roleId":"reviewer",
			"mcps":[{"mcpId":"official/tavily","enabled":true}],
			"retry":{"enabled":false,"maxRetries":2,"initialDelaySeconds":10}}}},
		{"id":"loop","data":{"kind":"loop","loopConfig":{"maxIterations":3,"variables":[],
			"until":{"logic":"and","conditions":[]},"outputs":[]}}},
		{"id":"entry","parentId":"loop","data":{"kind":"start","containerId":"loop"}},
		{"id":"parked","parentId":"loop","data":{"kind":"agent","containerId":"loop",
			"agentConfig":{"prompt":"y","interactive":true}}},
		{"id":"tool","data":{"kind":"tool"}},{"id":"spare","data":{"kind":"prompt"}}],
		"edges":[{"source":"start","target":"writer"},{"source":"start","target":"loop"},
			{"source":"entry","target":"parked"},{"source":"writer","target":"tool"}]}`
	status, analysis = call("POST", base+"/analyze", map[string]any{"graph": unapplied})
	want = map[string]any{"unusedNodeIds": []any{"spare"}, "unrecognizedNodes": []any{},
		"unappliedSettings": []any{map[string]any{"nodeId": "parked", "settings": []any{"interactive"}}},
		"unsupportedNodes":  []any{map[string]any{"nodeId": "tool", "kind": "tool"}}}
	if status != http.StatusOK || !reflect.DeepEqual(analysis, want) {
		t.Fatalf("analyze unapplied and unsupported = %d %v, want %v", status, analysis, want)
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

// TestWorkflowRunStarterMapsPreflightRefusalsForDecomposition exercises the real workflowhost +
// taskdecomp adapter: a missing Skill or role localizes the decomposition failure and cancels the
// pending workflow run, and creating the missing Skill lets a subsequent split succeed.
func TestWorkflowRunStarterMapsPreflightRefusalsForDecomposition(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	token := storetest.SignIn(t, s, "decomp-starter", "Owner", store.RoleOwner)
	u, err := s.SessionUser(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := s.Tenants(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tid := tenants[0].ID
	scope := execution.Scope{TenantID: tid, UserID: u.ID}
	project, err := s.CreateProject(ctx, scope, store.Project{
		ID:            "22222222-2222-4222-8222-222222222222",
		Name:          "Demo",
		RepositoryURL: "https://example.invalid/demo",
		Commit:        "c",
		MirrorPath:    "/tmp/demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &executiontest.Stub{}
	wfHost, err := workflowhost.NewHost(workflowhost.Config{Store: s, Execution: stub, Catalog: stub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wfHost.Close)
	decompositions := taskdecomp.New(s, workflowRunStarter{wfHost}, nil)
	t.Cleanup(decompositions.Close)

	graph := `{"nodes":[
		{"id":"start","data":{"kind":"start","inputVariables":[{"name":"tasks","valueType":"array[object]","value":[{"title":"拆出的子任务","description":"说明"}]}]}},
		{"id":"cond","data":{"kind":"condition","cases":[{"id":"never","logic":"and","conditions":[{"variableSelector":["start","input"],"operator":"is","value":"__never__"}]}]}},
		{"id":"planner","data":{"kind":"agent","title":"拆分助手","agentConfig":{"prompt":"Split","skills":[{"skillId":"decomp-checklist","enabled":true}]}}},
		{"id":"out","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","tasks"]}]}}
	],"edges":[{"source":"start","target":"cond"},{"source":"cond","sourceHandle":"never","target":"planner"},{"source":"cond","target":"out"}]}`
	wf, _, err := s.CreateWorkflow(ctx, tid, "技能拆分", graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tid, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, tid, u.ID, store.TaskInput{Title: "大需求", ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := store.Access{TenantID: tid, UserID: u.ID, Role: store.RoleOwner}
	failed, err := decompositions.Start(ctx, access, task.ID, wf.ID, "missing-skill-1")
	if err != nil {
		t.Fatal(err)
	}
	wantFailure := "节点「拆分助手」需要的技能 decomp-checklist 不可用，请先在个人技能页导入同名技能或确认市场技能已下发到项目后再重新拆分。"
	if failed.Status != store.DecompositionFailed || failed.Failure != wantFailure {
		t.Fatalf("failed decomposition = %+v, want failure %q", failed, wantFailure)
	}
	refusedRun, err := s.WorkflowRun(ctx, tid, failed.WorkflowRunID)
	if err != nil || refusedRun.Status != store.WorkflowStatusCancelled {
		t.Fatalf("refused run = %+v (err=%v), want cancelled", refusedRun, err)
	}

	if _, err := s.CreateSkill(ctx, tid, u.ID, store.SkillInput{Name: "decomp-checklist", Description: "拆分清单", Content: "# 拆分清单"}); err != nil {
		t.Fatal(err)
	}
	retried, err := decompositions.Start(ctx, access, task.ID, wf.ID, "retry-after-skill")
	if err != nil || retried.Status != store.DecompositionRunning {
		t.Fatalf("retry decomposition = %+v (err=%v), want running", retried, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, err := decompositions.List(ctx, tid, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) > 0 && items[0].ID == retried.ID && items[0].Status != store.DecompositionRunning {
			if items[0].Status != store.DecompositionSucceeded || items[0].CreatedCount != 1 {
				t.Fatalf("retried decomposition = %+v, want succeeded with 1 subtask", items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retried decomposition never settled: %+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTaskWorkflowRunnerPassesHostRefusalsThrough wires the real workflowhost behind the task
// workflow host the way main does: a start the host refuses reaches the task workflow as the
// host's own error, so the recorded refusal is worded by the host (no decomposition translation,
// no duplicated sentinel) and the HTTP layer can still map it by the host's sentinel.
func TestTaskWorkflowRunnerPassesHostRefusalsThrough(t *testing.T) {
	ctx := context.Background()
	s, db := storetest.OpenWithConn(t)
	token := storetest.SignIn(t, s, "task-runner", "Owner", store.RoleOwner)
	u, err := s.SessionUser(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := s.Tenants(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tid := tenants[0].ID
	scope := execution.Scope{TenantID: tid, UserID: u.ID}
	project, err := s.CreateProject(ctx, scope, store.Project{
		ID:            "33333333-3333-4333-8333-333333333333",
		Name:          "Demo",
		RepositoryURL: "https://example.invalid/demo",
		Commit:        "c",
		MirrorPath:    "/tmp/demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &executiontest.Stub{}
	wfHost, err := workflowhost.NewHost(workflowhost.Config{Store: s, Execution: stub, Catalog: stub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wfHost.Close)
	taskWorkflows := taskworkflow.New(s, taskWorkflowRunner{wfHost})
	t.Cleanup(taskWorkflows.Close)

	graph := `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"reviewer","data":{"kind":"agent","title":"审查","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Review","skills":[{"skillId":"handle-checklist","enabled":true}]}}}],"edges":[{"source":"start","target":"reviewer"}]}`
	wf, _, err := s.CreateWorkflow(ctx, tid, "Needs skill", graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tid, wf.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, tid, u.ID, store.TaskInput{
		Title: "审查需求", ProjectID: project.ID, Assignee: &store.TaskAssigneeRef{Type: store.AssigneeHuman, ID: u.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	runID, err := taskWorkflows.Start(ctx, store.NewTaskWorkflow{
		TenantID: tid, TaskID: task.ID, UserID: u.ID, Key: "missing-skill", WorkflowID: wf.ID,
	}, nil)
	if err == nil {
		t.Fatal("a missing Skill started the handling run")
	}
	if !errors.Is(err, server.ErrWorkflowSkillNotFound) || errors.Is(err, taskdecomp.ErrWorkflowSkillNotFound) {
		t.Fatalf("start error = %v, want the host's ErrWorkflowSkillNotFound without the decomposition's translation", err)
	}
	var status, failure string
	if err := db.QueryRow(ctx, `SELECT status, failure FROM v2_task_workflow_runs WHERE tenant_id = $1 AND run_id = $2`, tid, runID).Scan(&status, &failure); err != nil {
		t.Fatal(err)
	}
	var missing *server.WorkflowSkillNotFound
	if !errors.As(err, &missing) {
		t.Fatalf("start error = %v, want *server.WorkflowSkillNotFound", err)
	}
	if status != "failed" || failure != missing.Error() {
		t.Fatalf("recorded refusal = %q %q, want failed %q", status, failure, missing.Error())
	}
	if strings.Contains(failure, "\n") {
		t.Fatalf("recorded refusal carries a joined decomposition sentinel: %q", failure)
	}
}

func buildWorkflowArchiveBytes(t *testing.T, manifest string, docs map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	mw, err := z.Create("orax.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	for name, body := range docs {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

// TestWorkflowPluginImportAllowsExecutorlessAgentAndRejectsInvalidGraph wires the real
// workflow domain validators into the HTTP plugin install route and verifies that:
//  1. Workflow packages with executor-less Agents (like ora-space.workflow-examples) install cleanly.
//  2. Manually publishing one of those unmodified templates still fails on missing executor.
//  3. Packages containing genuine non-executor graph defects still fail with plugin_workflow_invalid.
func TestWorkflowPluginImportAllowsExecutorlessAgentAndRejectsInvalidGraph(t *testing.T) {
	examplesBytes, examplesSHA := buildWorkflowArchiveBytes(t, `resolver = 1
title = "Workflow Examples"
identifier = "ora-space.workflow-examples"
namespace = "official"
kind = "workflow"
version = "0.1.0"
description = "Starter workflows"
`, map[string]string{
		"assets/workflows/hello-agent.json": `{"name":"Hello Agent","nodes":[{"id":"start","data":{"kind":"start"}},{"id":"agent","data":{"kind":"agent","agentConfig":{"prompt":"hello"}}},{"id":"out","data":{"kind":"output"}}],"edges":[{"source":"start","target":"agent"},{"source":"agent","target":"out"}]}`,
		"assets/workflows/named-slot.json":  `{"name":"Named Slot","nodes":[{"id":"start","data":{"kind":"start"}},{"id":"agent","data":{"kind":"agent","agentConfig":{"prompt":"slot","executorSlot":"coder"}}},{"id":"out","data":{"kind":"output"}}],"edges":[{"source":"start","target":"agent"},{"source":"agent","target":"out"}]}`,
	})
	brokenBytes, brokenSHA := buildWorkflowArchiveBytes(t, `resolver = 1
title = "Broken Workflow"
identifier = "ora-space.broken-workflow"
namespace = "official"
kind = "workflow"
version = "0.1.0"
description = "Invalid workflow"
`, map[string]string{
		"assets/workflows/broken.json": `{"name":"Broken","nodes":[{"id":"start","data":{"kind":"start"}},{"id":"agent","data":{"kind":"agent","agentConfig":{"promptInactivity":"forever"}}}],"edges":[{"source":"start","target":"agent"}]}`,
	})
	archives := map[string][]byte{
		"https://example.org/workflow-examples.orax": examplesBytes,
		"https://example.org/broken-workflow.orax":   brokenBytes,
	}

	s := storetest.Open(t)
	storetest.SignIn(t, s, "plugin-import-owner", "Owner", store.RoleOwner)
	opts := withWorkflowDomain(server.Options{
		SessionTTL: time.Hour,
		DownloadPluginPackage: func(_ context.Context, locator, _ string, _ pluginmarket.Retrieval, _ pluginmarket.Credentials) (string, error) {
			raw, ok := archives[locator]
			if !ok {
				return "", os.ErrNotExist
			}
			f, err := os.CreateTemp(t.TempDir(), "plugin-*.orax")
			if err != nil {
				return "", err
			}
			if _, err := f.Write(raw); err != nil {
				_ = f.Close()
				return "", err
			}
			if err := f.Close(); err != nil {
				return "", err
			}
			return f.Name(), nil
		},
	}, nil)
	srv := httptest.NewServer(server.New(s, opts))
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	call := func(method, path string, input any, headers map[string]string) (int, map[string]any) {
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
		for k, v := range headers {
			req.Header.Set(k, v)
		}
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

	status, _ := call("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("plugin-import-owner")}, nil)
	if status != http.StatusOK {
		t.Fatalf("login = %d", status)
	}
	_, tenants := call("GET", "/api/v1/me/tenants", nil, nil)
	tid := tenants["items"].([]any)[0].(map[string]any)["id"].(string)
	_, spaces := call("GET", "/api/v1/tenants/"+tid+"/spaces", nil, nil)
	sid := spaces["items"].([]any)[0].(map[string]any)["id"].(string)

	if err := s.ReplacePluginCatalog(context.Background(), []pluginmarket.Entry{
		{
			Namespace: "official", Identifier: "ora-space.workflow-examples", Title: "Workflow Examples",
			Kind: "workflow", Version: "0.1.0", Description: "Starter workflows",
			URL: "https://example.org/workflow-examples.orax", SHA256: examplesSHA,
			MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace",
		},
		{
			Namespace: "official", Identifier: "ora-space.broken-workflow", Title: "Broken Workflow",
			Kind: "workflow", Version: "0.1.0", Description: "Invalid workflow",
			URL: "https://example.org/broken-workflow.orax", SHA256: brokenSHA,
			MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace",
		},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	pluginsPath := "/api/v1/tenants/" + tid + "/spaces/" + sid + "/plugins"
	status, body := call("POST", pluginsPath, map[string]any{
		"identifier":    "official/ora-space.workflow-examples",
		"pluginVersion": "0.1.0",
		"version":       0,
	}, map[string]string{"Idempotency-Key": "install-examples"})
	if status != http.StatusOK {
		t.Fatalf("install executorless examples = %d %v", status, body)
	}
	resource := body["resource"].(map[string]any)
	if resource["observedState"] != "installed" {
		t.Fatalf("installed resource = %v", resource)
	}

	workflows, err := s.Workflows(context.Background(), tid)
	if err != nil || len(workflows) != 2 {
		t.Fatalf("imported workflows = %+v %v, want 2", workflows, err)
	}

	// Manually publishing an unmodified imported template must still enforce executor validation.
	status, pubErr := call("POST", "/api/v2/tenants/"+tid+"/workflows/"+workflows[0].ID+"/publish", map[string]any{"version": "v2"}, nil)
	if status != http.StatusBadRequest || pubErr["code"] != "workflow_run_graph_parse" {
		t.Fatalf("manual publish of executorless template = %d %v", status, pubErr)
	}
	params, _ := pubErr["params"].(map[string]any)
	if params["nodeId"] != "agent" || params["field"] != "data.agentConfig.executor" {
		t.Fatalf("publish refusal params = %v", params)
	}

	// Non-executor validation defects must still fail import and roll back cleanly.
	status, brokenBody := call("POST", pluginsPath, map[string]any{
		"identifier":    "official/ora-space.broken-workflow",
		"pluginVersion": "0.1.0",
		"version":       0,
	}, map[string]string{"Idempotency-Key": "install-broken"})
	if status != http.StatusBadRequest || brokenBody["code"] != "plugin_workflow_invalid" {
		t.Fatalf("install invalid workflow = %d %v, want 400 plugin_workflow_invalid", status, brokenBody)
	}
	afterWorkflows, err := s.Workflows(context.Background(), tid)
	if err != nil || len(afterWorkflows) != 2 {
		t.Fatalf("failed install altered workflows: %+v %v", afterWorkflows, err)
	}
	status, installedList := call("GET", pluginsPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list plugins = %d %v", status, installedList)
	}
	for _, raw := range installedList["items"].([]any) {
		if raw.(map[string]any)["identifier"] == "ora-space.broken-workflow" {
			t.Fatalf("failed install left plugin selection: %v", raw)
		}
	}
}
