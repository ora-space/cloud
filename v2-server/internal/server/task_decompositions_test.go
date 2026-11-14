package server_test

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

// finishingRuns is a workflow host whose runs end as soon as they start, with a fixed output.
type finishingRuns struct {
	store  *store.Store
	output string
}

func (r finishingRuns) Start(ctx context.Context, tenant, _, runID string) error {
	if _, e := r.store.StartWorkflowRun(ctx, tenant, runID); e != nil {
		return e
	}
	_, e := r.store.FinishWorkflowRun(ctx, tenant, runID, store.WorkflowStatusSucceeded, &r.output, nil)
	return e
}

func (finishingRuns) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}

var decompositionCatalogContract = sync.OnceValues(func() (routers.Router, error) {
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "decomposition-workflows.openapi.json"))
	if e != nil {
		return nil, e
	}
	if e := doc.Validate(context.Background()); e != nil {
		return nil, e
	}
	return legacy.NewRouter(doc)
})

// decompositionCatalog calls the catalog route, checks the response against
// decomposition-workflows.openapi.json, and returns each workflow's verdict by name.
func decompositionCatalog(t *testing.T, c *taskClient, tid string) map[string]decompositionVerdict {
	t.Helper()
	req, e := http.NewRequest("GET", c.base+"/api/v2/tenants/"+tid+"/decomposition-workflows", nil)
	if e != nil {
		t.Fatal(e)
	}
	res, e := c.http.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	routes, e := decompositionCatalogContract()
	if e != nil {
		t.Fatal(e)
	}
	route, params, e := routes.FindRoute(req)
	if e != nil {
		t.Fatalf("catalog route is not in decomposition-workflows.openapi.json: %v", e)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 res.StatusCode, Header: res.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	in.SetBodyBytes(raw)
	if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
		t.Fatalf("catalog does not match decomposition-workflows.openapi.json: %v\n%s", e, raw)
	}
	var body struct {
		Items []struct {
			Name             string  `json:"name"`
			PublishedVersion *string `json:"publishedVersion"`
			CanDecompose     bool    `json:"canDecompose"`
			Reason           string  `json:"decompositionBlockedReason"`
		} `json:"items"`
	}
	if e := json.Unmarshal(raw, &body); e != nil || res.StatusCode != 200 {
		t.Fatalf("catalog = %d %s (%v)", res.StatusCode, raw, e)
	}
	out := map[string]decompositionVerdict{}
	for _, item := range body.Items {
		out[item.Name] = decompositionVerdict{published: item.PublishedVersion != nil, can: item.CanDecompose, reason: item.Reason}
	}
	return out
}

type decompositionVerdict struct {
	published bool
	can       bool
	reason    string
}

func TestDecompositionWorkflowCatalogHTTP(t *testing.T) {
	f := newSquadServer(t)
	c := f.taskClient()
	c.login()
	tid := f.tenant(t)
	tasksGraph := `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"out","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","input"]}]}}],"edges":[{"source":"start","target":"out"}]}`
	summaryGraph := `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"out","data":{"kind":"output","outputs":[{"name":"summary","variableSelector":["start","input"]}]}}],"edges":[{"source":"start","target":"out"}]}`
	// Each published workflow's draft is then changed to the opposite graph without republishing:
	// the verdict must follow the published snapshot a run would freeze, not the draft.
	ids := map[string]string{}
	for name, graphs := range map[string][2]string{"可拆分": {tasksGraph, summaryGraph}, "不可拆分": {summaryGraph, tasksGraph}} {
		w, _, e := f.store.CreateWorkflow(f.ctx, tid, name, graphs[0])
		if e != nil {
			t.Fatal(e)
		}
		if _, e := f.store.PublishWorkflow(f.ctx, tid, w.ID, "v1", nil); e != nil {
			t.Fatal(e)
		}
		if _, e := f.store.UpdateWorkflowDraft(f.ctx, tid, w.ID, graphs[1]); e != nil {
			t.Fatal(e)
		}
		ids[name] = w.ID
	}
	if _, _, e := f.store.CreateWorkflow(f.ctx, tid, "未发布", tasksGraph); e != nil {
		t.Fatal(e)
	}
	want := map[string]decompositionVerdict{
		"可拆分":  {published: true, can: true},
		"不可拆分": {published: true, reason: taskdecomp.OutputRequirement},
		"未发布":  {reason: "工作流尚未发布。"},
	}
	for round := range 2 { // the second round is served from the verdict cache
		if got := decompositionCatalog(t, c, tid); !maps.Equal(got, want) {
			t.Fatalf("round %d: catalog = %+v, want %+v", round, got, want)
		}
	}

	// Publishing the changed drafts swaps both verdicts: a new published snapshot is judged afresh.
	for _, id := range ids {
		if _, e := f.store.PublishWorkflow(f.ctx, tid, id, "v2", nil); e != nil {
			t.Fatal(e)
		}
	}
	want["可拆分"], want["不可拆分"] = want["不可拆分"], want["可拆分"]
	if got := decompositionCatalog(t, c, tid); !maps.Equal(got, want) {
		t.Fatalf("after republish: catalog = %+v, want %+v", got, want)
	}
}

func TestTaskDecompositionHTTP(t *testing.T) {
	f := newSquadServer(t)
	decompositions := taskdecomp.New(f.store, finishingRuns{store: f.store,
		output: `{"tasks":[{"title":"登录页","description":"邮箱登录"},{"title":"注册页","description":"校验邮箱"}]}`}, nil)
	t.Cleanup(decompositions.Close)
	f.srv.Close()
	f.srv = httptest.NewServer(server.New(f.store, server.Options{SessionTTL: time.Hour, TaskDecompositions: decompositions}))
	t.Cleanup(f.srv.Close)
	c := f.taskClient()
	c.login()
	tid := f.tenant(t)
	f.project(t)
	w, _, e := f.store.CreateWorkflow(f.ctx, tid, "需求拆分", `{"nodes":[{"id":"start","data":{"kind":"start"}},{"id":"out","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","input"]}]}}],"edges":[{"source":"start","target":"out"}]}`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.PublishWorkflow(f.ctx, tid, w.ID, "v1", nil); e != nil {
		t.Fatal(e)
	}
	base := "/api/v2/tenants/" + tid + "/tasks"
	status, parent := c.do("POST", base, map[string]any{"title": "账号体系", "description": "登录和注册", "projectId": taskProjectID})
	if status != 201 || parent["parentId"] != nil {
		t.Fatalf("create = %d %v", status, parent)
	}
	split := base + "/" + parent["id"].(string) + "/decompositions"
	key := map[string]string{"Idempotency-Key": "split-1"}

	if status, body := c.do("POST", split, map[string]any{"workflowId": w.ID}); status != 400 || body["code"] != "idempotency_key_required" {
		t.Fatalf("no key = %d %v", status, body)
	}
	if status, body := c.doHeaders("POST", split, map[string]any{"workflowId": "00000000-0000-4000-8000-000000000999"}, key); status != 400 || body["code"] != "invalid_workflow" {
		t.Fatalf("unknown workflow = %d %v", status, body)
	}
	incompatible, _, e := f.store.CreateWorkflow(f.ctx, tid, "缺少 tasks", `{"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]}`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.PublishWorkflow(f.ctx, tid, incompatible.ID, "v1", nil); e != nil {
		t.Fatal(e)
	}
	status, rejected := c.doHeaders("POST", split, map[string]any{"workflowId": incompatible.ID}, key)
	params, _ := rejected["params"].(map[string]any)
	message, _ := params["message"].(string)
	if status != 400 || rejected["code"] != "decomposition_output_required" ||
		!strings.Contains(message, "tasks") || !strings.Contains(message, "{title, description}") {
		t.Fatalf("incompatible workflow = %d %v", status, rejected)
	}
	status, started := c.doHeaders("POST", split, map[string]any{"workflowId": w.ID}, key)
	if status != 201 || started["workflowName"] != "需求拆分" || started["workflowRunId"] == "" {
		t.Fatalf("start = %d %v", status, started)
	}

	deadline := time.Now().Add(5 * time.Second)
	var done map[string]any
	for {
		status, list := c.do("GET", split, nil)
		items, _ := list["items"].([]any)
		if status != 200 || len(items) != 1 {
			t.Fatalf("list = %d %v", status, list)
		}
		if done = items[0].(map[string]any); done["status"] != store.DecompositionRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never settled: %v", done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if done["status"] != store.DecompositionSucceeded || done["createdCount"] != float64(2) || done["failure"] != "" {
		t.Fatalf("settled = %v", done)
	}

	_, list := c.do("GET", base, nil)
	var child string
	subtasks := 0
	for _, item := range list["items"].([]any) {
		task := item.(map[string]any)
		if task["parentId"] == parent["id"] {
			subtasks++
			child = task["id"].(string)
		}
	}
	if subtasks != 2 {
		t.Fatalf("tasks = %v", list)
	}
	if status, body := c.doHeaders("POST", base+"/"+child+"/decompositions", map[string]any{"workflowId": w.ID}, key); status != 409 || body["code"] != "subtask_not_decomposable" {
		t.Fatalf("split a subtask = %d %v", status, body)
	}
}
