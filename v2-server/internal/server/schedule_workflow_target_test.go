package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// storeRunHost starts and cancels runs in the store as the execution bridge would, executing
// nothing; refuse makes it decline to start, as the bridge does for a graph it cannot run.
type storeRunHost struct {
	db     *store.Store
	refuse bool
	scopes []server.WorkflowRunScope
}

func (h *storeRunHost) StartRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	h.scopes = append(h.scopes, scope)
	if h.refuse {
		return store.WorkflowRun{}, server.ErrWorkflowRunNotStartable
	}
	return h.db.StartWorkflowRun(ctx, scope.TenantID, runID)
}

func (h *storeRunHost) CancelRun(ctx context.Context, scope server.WorkflowRunScope, runID string) (store.WorkflowRun, error) {
	return h.db.CancelWorkflowRun(ctx, scope.TenantID, runID)
}

func (h *storeRunHost) UpdateRunInput(context.Context, server.WorkflowRunScope, string, *string, map[string]json.RawMessage) (store.WorkflowRun, error) {
	return store.WorkflowRun{}, errors.New("not used")
}

func (h *storeRunHost) CompleteNode(context.Context, server.WorkflowRunScope, string, string, bool, string) (store.WorkflowRun, error) {
	return store.WorkflowRun{}, errors.New("not used")
}

type workflowTargetFixture struct {
	db       *store.Store
	token    string
	scope    execution.Scope
	project  store.Project
	workflow store.Workflow
	draft    store.WorkflowSnapshot
	v1       store.WorkflowSnapshot
	catalog  *executiontest.Stub
	host     *storeRunHost
}

func newWorkflowTargetFixture(t *testing.T) *workflowTargetFixture {
	t.Helper()
	db := openStore(t)
	ctx := context.Background()
	f := &workflowTargetFixture{db: db, token: storetest.SignIn(t, db, "scheduled-workflows", "Owner", store.RoleOwner)}
	u, err := db.SessionUser(ctx, f.token)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := db.Tenants(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.scope = execution.Scope{TenantID: ts[0].ID, UserID: u.ID}
	if f.project, err = db.CreateProject(ctx, f.scope, store.Project{ID: "33333333-3333-4333-8333-333333333333", Name: "Demo", RepositoryURL: "https://example.invalid/demo", Commit: "c", MirrorPath: "/private/demo"}); err != nil {
		t.Fatal(err)
	}
	if f.workflow, f.draft, err = db.CreateWorkflow(ctx, f.scope.TenantID, "周报流程", `{"nodes":[],"edges":[]}`); err != nil {
		t.Fatal(err)
	}
	if f.v1, err = db.PublishWorkflow(ctx, f.scope.TenantID, f.workflow.ID, "v1", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	model := execution.Model{ID: "fixture", Name: "Fixture"}
	f.catalog = &executiontest.Stub{
		ListModelsFunc: func(context.Context, execution.Scope) ([]execution.Model, error) {
			return []execution.Model{model}, nil
		},
		ListExecutorsFunc: func(context.Context, execution.Scope) ([]execution.Executor, error) { return nil, nil },
	}
	f.host = &storeRunHost{db: db}
	return f
}

func (f *workflowTargetFixture) options(withRuns bool) server.Options {
	o := server.Options{Execution: f.catalog, Catalog: f.catalog}
	if withRuns {
		o.WorkflowRuns = f.host
	}
	return o
}

func (f *workflowTargetFixture) target(over map[string]any) map[string]any {
	target := map[string]any{"kind": "workflow", "workflowId": f.workflow.ID, "snapshotId": f.v1.ID, "input": "整理本周的变更"}
	for k, v := range over {
		target[k] = v
	}
	return target
}

func (f *workflowTargetFixture) config(projectID string, target map[string]any) map[string]any {
	return map[string]any{"name": "每周整理", "projectId": projectID, "target": target, "rule": map[string]any{"kind": "hours", "time": "09:00", "days": []int{}, "hours": 1, "timezone": "UTC"}}
}

func (f *workflowTargetFixture) client(t *testing.T, o server.Options) func(method, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "schedules.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(f.db, o)
	return func(method, path string, input any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(input)
		req := httptest.NewRequest(method, "/api/v2/tenants/"+f.scope.TenantID+path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: server.SessionCookie, Value: f.token})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		route, params, err := routes.FindRoute(req)
		if err != nil {
			t.Fatal(err)
		}
		v := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: rec.Code, Header: rec.Header(), Options: &openapi3filter.Options{IncludeResponseStatus: true}}
		v.SetBodyBytes(rec.Body.Bytes())
		if err = openapi3filter.ValidateResponse(context.Background(), v); err != nil {
			t.Fatalf("contract: %v %s", err, rec.Body)
		}
		return rec
	}
}

func TestScheduleWorkflowTargetIsChosenFromPublishedWorkflows(t *testing.T) {
	f := newWorkflowTargetFixture(t)
	call := f.client(t, f.options(true))

	// The resources offer the published version, with the snapshot a schedule freezes.
	w := call("GET", "/schedule-resources", nil)
	var resources struct {
		Workflows []map[string]string `json:"workflows"`
		Available map[string]bool     `json:"available"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resources) != nil || !resources.Available["workflows"] || len(resources.Workflows) != 1 {
		t.Fatalf("resources: %d %s", w.Code, w.Body)
	}
	if got := resources.Workflows[0]; got["id"] != f.workflow.ID || got["name"] != "周报流程" || got["version"] != "v1" || got["snapshotId"] != f.v1.ID {
		t.Fatalf("workflow resource = %v", got)
	}

	w = call("POST", "/schedule-configs", f.config(f.project.ID, f.target(nil)))
	if w.Code != 201 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	var saved store.ScheduleConfig
	_ = json.Unmarshal(w.Body.Bytes(), &saved)
	var stored map[string]any
	_ = json.Unmarshal(saved.Target, &stored)
	if stored["snapshotId"] != f.v1.ID || stored["input"] != "整理本周的变更" || saved.ProjectID != f.project.ID {
		t.Fatalf("saved = %+v target %v", saved, stored)
	}

	for name, tc := range map[string]struct {
		project string
		target  map[string]any
		status  int
		code    string
	}{
		"draft snapshot":   {f.project.ID, f.target(map[string]any{"snapshotId": f.draft.ID}), 404, "not_found"},
		"unknown workflow": {f.project.ID, f.target(map[string]any{"workflowId": "44444444-4444-4444-8444-444444444444"}), 404, "not_found"},
		"unknown project":  {"55555555-5555-4555-8555-555555555555", f.target(nil), 404, "not_found"},
		"no project":       {"", f.target(nil), 400, "project_required"},
		"legacy reference": {f.project.ID, map[string]any{"kind": "workflow", "providerReference": "legacy-id"}, 400, "invalid_target"},
		"input too long":   {f.project.ID, f.target(map[string]any{"input": strings.Repeat("a", 32001)}), 400, "invalid_target"},
	} {
		w := call("POST", "/schedule-configs", f.config(tc.project, tc.target))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}

	// A process that cannot run workflows neither offers nor accepts them.
	without := f.client(t, f.options(false))
	w = without("GET", "/schedule-resources", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"workflows":false`) || !strings.Contains(w.Body.String(), `"workflows":[]`) {
		t.Fatalf("resources without runs: %d %s", w.Code, w.Body)
	}
	if w = without("POST", "/schedule-configs", f.config(f.project.ID, f.target(nil))); w.Code != 503 || !strings.Contains(w.Body.String(), "schedule_target_unavailable") {
		t.Fatalf("save without runs: %d %s", w.Code, w.Body)
	}
}

func TestScheduledWorkflowRunsThroughTheRunEntry(t *testing.T) {
	f := newWorkflowTargetFixture(t)
	ctx := context.Background()
	input, _ := json.Marshal(f.target(nil))
	saved, err := f.db.CreateScheduleConfig(ctx, f.scope.TenantID, f.scope.UserID, store.ScheduleInput{Name: "每周整理", ProjectID: f.project.ID, Target: input, Rule: json.RawMessage(`{"kind":"hours","hours":1,"timezone":"UTC"}`)})
	if err != nil {
		t.Fatal(err)
	}
	now := saved.CreatedAt.Add(time.Minute)
	c, err := server.NewScheduleCoordinator(f.db, f.options(true), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, f.scope); err != nil {
		t.Fatal(err)
	}
	occ, err := c.Manual(ctx, f.scope, saved.ID, "manual-1")
	if err != nil {
		t.Fatal(err)
	}
	if occ.Status != "running" {
		t.Fatalf("occurrence = %+v", occ)
	}
	// The run is the occurrence's own: frozen snapshot, project and input, started in the owner's scope.
	run, err := f.db.ScheduledWorkflowRun(ctx, f.scope.TenantID, occ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.WorkflowStatusRunning || run.SnapshotID != f.v1.ID || run.ProjectID != f.project.ID || run.Input == nil || *run.Input != "整理本周的变更" || run.Name != "每周整理" {
		t.Fatalf("run = %+v", run)
	}
	if occ.ExecutionRef != `{"workflowRunId":"`+run.ID+`"}` || len(f.host.scopes) != 1 || f.host.scopes[0] != (server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: f.scope.UserID}) {
		t.Fatalf("ref = %s scopes = %+v", occ.ExecutionRef, f.host.scopes)
	}
	// The same request key finds the same occurrence and run.
	again, err := c.Manual(ctx, f.scope, saved.ID, "manual-1")
	if err != nil || again.ID != occ.ID || len(f.host.scopes) != 1 {
		t.Fatalf("retry = %+v %v starts=%d", again, err, len(f.host.scopes))
	}
	// A publish after the schedule was saved does not change what it runs.
	if _, err := f.db.UpdateWorkflowDraft(ctx, f.scope.TenantID, f.workflow.ID, `{"nodes":[],"edges":[],"note":"v2"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.PublishWorkflow(ctx, f.scope.TenantID, f.workflow.ID, "v2", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}

	output := `{"summary":"完成"}`
	if _, err := f.db.FinishWorkflowRun(ctx, f.scope.TenantID, run.ID, store.WorkflowStatusSucceeded, &output, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(ctx, f.scope); err != nil {
		t.Fatal(err)
	}
	settled, err := f.db.GetOccurrence(ctx, f.scope.TenantID, f.scope.UserID, occ.ID)
	if err != nil || settled.Status != "succeeded" || !settled.Released {
		t.Fatalf("settled = %+v %v", settled, err)
	}

	// Its result is the run, read through the run routes, not a turn.
	srv := server.New(f.db, f.options(true))
	req := httptest.NewRequest("GET", "/api/v2/tenants/"+f.scope.TenantID+"/schedule-configs/"+saved.ID+"/runs/"+occ.ID+"/result", nil)
	req.AddCookie(&http.Cookie{Name: server.SessionCookie, Value: f.token})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "execution_result_not_found") {
		t.Fatalf("result route: %d %s", rec.Code, rec.Body)
	}
}

func TestScheduledWorkflowTheBridgeRefusesLeavesNoRunBehind(t *testing.T) {
	f := newWorkflowTargetFixture(t)
	f.host.refuse = true
	ctx := context.Background()
	input, _ := json.Marshal(f.target(nil))
	saved, err := f.db.CreateScheduleConfig(ctx, f.scope.TenantID, f.scope.UserID, store.ScheduleInput{Name: "拒绝", ProjectID: f.project.ID, Target: input, Rule: json.RawMessage(`{"kind":"hours","hours":1,"timezone":"UTC"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := server.NewScheduleCoordinator(f.db, f.options(true), func() time.Time { return saved.CreatedAt.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Recover(ctx, f.scope); err != nil {
		t.Fatal(err)
	}
	occ, err := c.Manual(ctx, f.scope, saved.ID, "refused")
	if !errors.Is(err, server.ErrWorkflowRunNotStartable) {
		t.Fatalf("manual = %+v %v", occ, err)
	}
	occ, err = f.db.GetOccurrence(ctx, f.scope.TenantID, f.scope.UserID, occ.ID)
	if err != nil || occ.Status != "skipped" || occ.Reason != "workflow_run_not_startable" || !occ.Released {
		t.Fatalf("occurrence = %+v %v", occ, err)
	}
	run, err := f.db.ScheduledWorkflowRun(ctx, f.scope.TenantID, occ.ID)
	if err != nil || run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("refused run = %+v %v", run, err)
	}
}
