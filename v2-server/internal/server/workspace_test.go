package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

var workspaceContract = sync.OnceValues(func() (routers.Router, error) {
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "workspace.openapi.json"))
	if e != nil {
		return nil, e
	}
	if e := doc.Validate(context.Background()); e != nil {
		return nil, e
	}
	return legacy.NewRouter(doc)
})

// workspaceGet calls a workspace route and checks the response against workspace.openapi.json.
func workspaceGet(t *testing.T, client *http.Client, base, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	routes, e := workspaceContract()
	if e != nil {
		t.Fatal(e)
	}
	route, params, e := routes.FindRoute(req)
	if e != nil {
		t.Fatalf("GET %s is not in workspace.openapi.json: %v", path, e)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 res.StatusCode, Header: res.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(raw)
	if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
		t.Fatalf("GET %s does not match workspace.openapi.json: %v\n%s", path, e, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// agentCheckout is a cloned project the Agent then edited: one modified and one new file.
func agentCheckout(t *testing.T) (root, base string) {
	t.Helper()
	root = t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		cmd.Dir = root
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v: %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		if e := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); e != nil {
			t.Fatal(e)
		}
	}
	git("init", "-q", ".")
	write("README.md", "hello\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base = git("rev-parse", "HEAD")
	write("README.md", "hello\nagent\n")
	write("src/app.go", "package src\n")
	return root, base
}

func TestChatWorkspaceReadsTheConversationCheckout(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	storetest.SignIn(t, s, "viewer", "Viewer", store.RoleOwner)
	srv := httptest.NewServer(server.New(s, server.Options{SessionTTL: time.Hour}))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login, _ := json.Marshal(map[string]string{"provider": "email", "email": storetest.Email("viewer")})
	res, e := client.Post(srv.URL+"/auth/login", "application/json", strings.NewReader(string(login)))
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	user, tenant := storetest.Login(t, s, "viewer")
	scope := execution.Scope{TenantID: tenant, UserID: user}
	c, e := s.CreateChatConversation(ctx, store.NewChatConversation{TenantID: tenant, UserID: user, CreateKey: "k1", ProjectID: "demo", ExecutorID: "fixture", Title: "看文件"})
	if e != nil {
		t.Fatal(e)
	}
	base := "/api/v2/tenants/" + tenant + "/chat/conversations/" + c.ID + "/workspace"
	get := func(path string) (int, map[string]any) { return workspaceGet(t, client, srv.URL, base+path) }

	if status, body := get("/changes"); status != 404 || body["code"] != "workspace_not_ready" {
		t.Fatalf("before the first checkout = %d %v", status, body)
	}
	root, commit := agentCheckout(t)
	turn := execution.Turn{Ref: execution.Ref{SessionID: "s1", TurnID: "t1"}, Caller: execution.Caller{Kind: execution.CallerChat, ID: c.ID, AttemptID: "k1"}, Prompt: execution.TextPrompt("改一下")}
	if e := s.CreateExecutionSession(ctx, scope, execution.Session{ID: "s1", WorkspaceID: "w1", Model: execution.Model{ID: "fixture"}}, turn); e != nil {
		t.Fatal(e)
	}
	if e := s.RecordChatTurn(ctx, c.ID, "s1", "k1", "t1", time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := s.RecordWorkspace(ctx, "w1", store.WorkspaceLocation{Path: root, BaseCommit: commit}); e != nil {
		t.Fatal(e)
	}

	status, changes := get("/changes")
	files, _ := changes["files"].([]any)
	if status != 200 || changes["baseCommit"] != commit || len(files) != 2 ||
		files[0].(map[string]any)["path"] != "README.md" || files[1].(map[string]any)["status"] != "added" {
		t.Fatalf("changes = %d %v", status, changes)
	}
	if status, diff := get("/diff?path=README.md"); status != 200 || !strings.Contains(diff["patch"].(string), "+agent") {
		t.Fatalf("diff = %d %v", status, diff)
	}
	if status, dir := get("/entries"); status != 200 || len(dir["entries"].([]any)) != 2 || dir["entries"].([]any)[0].(map[string]any)["path"] != "src" {
		t.Fatalf("root entries = %d %v", status, dir)
	}
	if status, file := get("/file?path=" + url.QueryEscape("src/app.go")); status != 200 || file["content"] != "package src\n" {
		t.Fatalf("file = %d %v", status, file)
	}
	for path, want := range map[string]string{
		"/file?path=" + url.QueryEscape("/etc/passwd"): "invalid_path",
		"/entries?path=.git":                           "invalid_path",
		"/file?path=missing.txt":                       "path_not_found",
		"/file?path=src":                               "not_a_file",
	} {
		if status, body := get(path); status < 400 || body["code"] != want {
			t.Errorf("%s = %d %v, want %s", path, status, body, want)
		}
	}
	other := "/api/v2/tenants/" + tenant + "/chat/conversations/00000000-0000-0000-0000-000000000000/workspace/changes"
	if status, body := workspaceGet(t, client, srv.URL, other); status != 404 || body["code"] != "not_found" {
		t.Fatalf("unknown conversation = %d %v", status, body)
	}
	if e := os.RemoveAll(root); e != nil {
		t.Fatal(e)
	}
	if status, body := get("/entries"); status != 410 || body["code"] != "workspace_removed" {
		t.Fatalf("after cleanup = %d %v", status, body)
	}
}

func TestWorkflowRunWorkspaceBeforeAnyAgentNode(t *testing.T) {
	call, s, signed := workflowServerWithClient(t, nil)
	tid := workflowTenant(t, call)
	_, me := call("GET", "/api/v1/me", nil)
	project := workflowRunProject(t, s, me["id"].(string), tid)
	base := "/api/v2/tenants/" + tid + "/workflows"
	_, created := call("POST", base, map[string]any{"name": "看文件", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)
	call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	_, run := call("POST", base+"/"+id+"/runs", map[string]any{"projectId": project.ID})
	runID := run["run"].(map[string]any)["id"].(string)

	runs := "/api/v2/tenants/" + tid + "/workflow-runs/"
	if status, body := workspaceGet(t, signed.client, signed.base, runs+runID+"/workspace/changes"); status != 404 || body["code"] != "workspace_not_ready" {
		t.Fatalf("pending run = %d %v", status, body)
	}
	if status, body := workspaceGet(t, signed.client, signed.base, runs+"00000000-0000-0000-0000-000000000000/workspace/entries"); status != 404 || body["code"] != "workflow_run_not_found" {
		t.Fatalf("unknown run = %d %v", status, body)
	}
}

func TestTaskRunWorkspaceReadsTheRunCheckout(t *testing.T) {
	ctx := context.Background()
	f := newSquadServer(t)
	f.taskRunServer(t)
	tid := f.tenant(t)
	f.project(t)
	c := f.taskClient()
	c.login()
	_, targets := c.do("GET", "/api/v1/tenants/"+tid+"/collaboration/targets", nil)
	var agentID string
	for _, item := range targets["items"].([]any) {
		if target := item.(map[string]any); target["description"] == "official/ora-space.opencode" {
			agentID = target["id"].(string)
		}
	}
	base := "/api/v2/tenants/" + tid + "/tasks"
	newTask := func(title string) string {
		status, created := c.do("POST", base, map[string]any{"title": title, "projectId": taskProjectID, "assignee": map[string]any{"type": "agent", "id": agentID}})
		if status != 201 {
			t.Fatalf("create = %d %v", status, created)
		}
		return created["id"].(string)
	}
	task, other := newTask("修复登录页"), newTask("别的任务")
	status, run := c.doHeaders("POST", base+"/"+task+"/runs", map[string]any{}, map[string]string{"Idempotency-Key": "start-1"})
	if status != 201 {
		t.Fatalf("start = %d %v", status, run)
	}
	runID := run["id"].(string)
	get := func(path string) (int, map[string]any) {
		return workspaceGet(t, c.http, c.base, base+"/"+task+"/runs/"+runID+"/workspace"+path)
	}

	if status, body := get("/changes"); status != 404 || body["code"] != "workspace_not_ready" {
		t.Fatalf("before the checkout = %d %v", status, body)
	}
	_, me := c.do("GET", "/api/v1/me", nil)
	scope := execution.Scope{TenantID: tid, UserID: me["id"].(string)}
	root, commit := agentCheckout(t)
	turn := execution.Turn{Ref: execution.Ref{SessionID: "session-1", TurnID: "turn-1"}, Caller: execution.Caller{Kind: execution.CallerTask, ID: runID, AttemptID: "start-1"}, Prompt: execution.TextPrompt("修复"), StartedAt: time.Now()}
	if e := f.store.CreateExecutionSession(ctx, scope, execution.Session{ID: "session-1", WorkspaceID: "w1", Model: execution.Model{ID: "fixture"}}, turn); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RecordWorkspace(ctx, "w1", store.WorkspaceLocation{Path: root, BaseCommit: commit}); e != nil {
		t.Fatal(e)
	}

	status, changes := get("/changes")
	if files, _ := changes["files"].([]any); status != 200 || changes["baseCommit"] != commit || len(files) != 2 {
		t.Fatalf("changes = %d %v", status, changes)
	}
	if status, diff := get("/diff?path=README.md"); status != 200 || !strings.Contains(diff["patch"].(string), "+agent") {
		t.Fatalf("diff = %d %v", status, diff)
	}
	// A run belongs to its task: it cannot be read through another one.
	wrong := base + "/" + other + "/runs/" + runID + "/workspace/changes"
	if status, body := workspaceGet(t, c.http, c.base, wrong); status != 404 || body["code"] != "not_found" {
		t.Fatalf("run of another task = %d %v", status, body)
	}
}
