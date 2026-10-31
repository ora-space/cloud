package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// taskClient talks to the task routes and validates every response against the candidate task
// contract.
type taskClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	routes routers.Router
}

func (f *squadFixture) taskClient() *taskClient {
	f.t.Helper()
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "tasks.openapi.json"))
	if e != nil {
		f.t.Fatal(e)
	}
	if e = doc.Validate(f.ctx); e != nil {
		f.t.Fatal(e)
	}
	routes, e := legacy.NewRouter(doc)
	if e != nil {
		f.t.Fatal(e)
	}
	jar, _ := cookiejar.New(nil)
	return &taskClient{t: f.t, base: f.srv.URL, http: &http.Client{Jar: jar}, routes: routes}
}

func (c *taskClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, c.base+path, reader)
	if e != nil {
		c.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := c.http.Do(req)
	if e != nil {
		c.t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if strings.HasPrefix(path, "/api/v2/") {
		route, params, e := c.routes.FindRoute(req)
		if e != nil {
			c.t.Fatalf("%s %s is not in tasks.openapi.json: %v", method, path, e)
		}
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 res.StatusCode, Header: res.Header,
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		in.SetBodyBytes(raw)
		if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
			c.t.Fatalf("%s %s does not match tasks.openapi.json: %v", method, path, e)
		}
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (c *taskClient) login() {
	c.t.Helper()
	if status, body := c.do("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("squad-demo")}); status != 200 {
		c.t.Fatalf("login = %d %v", status, body)
	}
}

const taskProjectID = "00000000-0000-4000-8000-0000000000aa"

// project records the prepared project a task must point at.
func (f *squadFixture) project(t *testing.T) {
	t.Helper()
	owner := storetest.SignIn(t, f.store, "squad-demo", "演示用户", store.RoleOwner)
	u, e := f.store.SessionUser(f.ctx, owner)
	if e != nil {
		t.Fatal(e)
	}
	ts, e := f.store.Tenants(f.ctx, u.ID)
	if e != nil || len(ts) != 1 {
		t.Fatalf("tenants = %v %v", ts, e)
	}
	_, e = f.store.CreateProject(f.ctx, execution.Scope{TenantID: ts[0].ID, UserID: u.ID}, store.Project{
		ID: taskProjectID, Name: "Demo", RepositoryURL: "fixture://demo", Commit: "abc", MirrorPath: "/tmp/tasks"})
	if e != nil {
		t.Fatal(e)
	}
}

func TestTaskHTTPLifecycle(t *testing.T) {
	f := newSquadServer(t)
	c := f.taskClient()
	c.login()
	tid := f.tenant(t)
	f.project(t)
	base := "/api/v2/tenants/" + tid + "/tasks"

	if status, body := c.do("GET", base, nil); status != 200 || body["nextCursor"] != "" {
		t.Fatalf("empty list = %d %v", status, body)
	}
	status, created := c.do("POST", base, map[string]any{
		"title": "整理演示数据", "description": "把三条示例任务写好", "projectId": taskProjectID,
	})
	if status != 201 || created["status"] != store.TaskPending || created["version"] != float64(1) ||
		created["assignee"] != nil || created["createdByName"] != "演示用户" {
		t.Fatalf("create = %d %v", status, created)
	}
	id, _ := created["id"].(string)
	one := base + "/" + id

	// Creation rejects a caller-supplied version, an empty title and an unchecked assignee.
	if status, body := c.do("POST", base, map[string]any{"title": "带版本", "projectId": taskProjectID, "version": 1}); status != 400 || body["code"] != "invalid_version" {
		t.Fatalf("create with version = %d %v", status, body)
	}
	if status, body := c.do("POST", base, map[string]any{"title": " ", "projectId": taskProjectID}); status != 400 || body["code"] != "invalid_title" {
		t.Fatalf("blank title = %d %v", status, body)
	}
	if status, body := c.do("POST", base, map[string]any{"title": "错的人", "projectId": taskProjectID, "assignee": map[string]any{"type": "robot", "id": id}}); status != 400 || body["code"] != "invalid_assignee" {
		t.Fatalf("unknown assignee kind = %d %v", status, body)
	}
	if status, body := c.do("POST", base, map[string]any{"title": "错的项目", "projectId": "00000000-0000-4000-8000-0000000000bb"}); status != 400 || body["code"] != "invalid_project" {
		t.Fatalf("unknown project = %d %v", status, body)
	}
	if status, body := c.do("POST", base, map[string]any{"title": "多余字段", "projectId": taskProjectID, "status": "done"}); status != 400 || body["code"] != "invalid_json" {
		t.Fatalf("unknown field = %d %v", status, body)
	}

	// Every write needs the version the caller read.
	if status, body := c.do("PUT", one, map[string]any{"title": "改名", "projectId": taskProjectID}); status != 428 || body["code"] != "version_required" {
		t.Fatalf("edit without version = %d %v", status, body)
	}
	status, edited := c.do("PUT", one, map[string]any{"title": "整理演示数据（改）", "description": "两行", "projectId": taskProjectID, "version": 1})
	if status != 200 || edited["version"] != float64(2) || edited["title"] != "整理演示数据（改）" {
		t.Fatalf("edit = %d %v", status, edited)
	}
	if status, body := c.do("PUT", one, map[string]any{"title": "再改", "projectId": taskProjectID, "version": 1}); status != 409 || body["code"] != "version_conflict" {
		t.Fatalf("stale edit = %d %v", status, body)
	}

	// Status moves one step at a time; a skipped step is a client error, not a conflict.
	if status, body := c.do("POST", one+"/status", map[string]any{"status": store.TaskDone, "version": 2}); status != 400 || body["code"] != "invalid_transition" {
		t.Fatalf("skip review = %d %v", status, body)
	}
	status, running := c.do("POST", one+"/status", map[string]any{"status": store.TaskRunning, "version": 2})
	if status != 200 || running["status"] != store.TaskRunning || running["version"] != float64(3) {
		t.Fatalf("start = %d %v", status, running)
	}
	if status, body := c.do("POST", one+"/status", map[string]any{"status": store.TaskReview}); status != 428 || body["code"] != "version_required" {
		t.Fatalf("status without version = %d %v", status, body)
	}
	if status, body := c.do("POST", one+"/status", map[string]any{"status": store.TaskReview, "version": 2}); status != 409 || body["code"] != "version_conflict" {
		t.Fatalf("stale status = %d %v", status, body)
	}

	// The assignee is a typed reference: a missing field is malformed, null clears it.
	if status, body := c.do("PUT", one+"/assignment", map[string]any{"version": 3}); status != 400 || body["code"] != "invalid_assignee" {
		t.Fatalf("missing assignee = %d %v", status, body)
	}
	me := created["createdBy"]
	if status, body := c.do("PUT", one+"/assignment", map[string]any{"assignee": map[string]any{"type": "human", "id": me}, "version": 3}); status != 200 || body["assignee"] == nil {
		t.Fatalf("assign human = %d %v", status, body)
	}
	status, cleared := c.do("PUT", one+"/assignment", map[string]any{"assignee": nil, "version": 4})
	if status != 200 || cleared["assignee"] != nil || cleared["version"] != float64(5) {
		t.Fatalf("clear assignee = %d %v", status, cleared)
	}

	// Reads answer 404 for anything that cannot exist.
	if status, body := c.do("GET", one, nil); status != 200 || body["id"] != id {
		t.Fatalf("get = %d %v", status, body)
	}
	if status, _ := c.do("GET", base+"/not-a-uuid", nil); status != 404 {
		t.Fatalf("malformed id = %d", status)
	}
	if status, _ := c.do("GET", base+"/00000000-0000-4000-8000-0000000000ff", nil); status != 404 {
		t.Fatalf("unknown id = %d", status)
	}
	if status, body := c.do("GET", base, nil); status != 200 {
		t.Fatalf("list = %d %v", status, body)
	}
}
