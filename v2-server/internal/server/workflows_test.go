package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// workflowCall drives the definition routes while validating every response against the candidate
// contract, so the spec and the handlers cannot drift apart silently.
type workflowCall func(method, path string, input any) (int, map[string]any)

func workflowServer(t *testing.T, mutate func(*server.Options)) workflowCall {
	call, _ := workflowServerWithStore(t, mutate)
	return call
}

func workflowServerWithStore(t *testing.T, mutate func(*server.Options)) (workflowCall, *store.Store) {
	t.Helper()
	s := openStore(t)
	storetest.SignIn(t, s, "workflow-owner", "Owner", store.RoleOwner)
	now := time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)
	opts := server.Options{
		SessionTTL: time.Hour,
		Now:        func() time.Time { return now },
		AnalyzeWorkflow: func(string) (server.WorkflowAnalysis, error) {
			return server.WorkflowAnalysis{UnusedNodeIDs: []string{"spare"}}, nil
		},
		ValidateWorkflow: func(graph string) error {
			if strings.Contains(graph, "unrunnable") {
				return errUnrunnable
			}
			return nil
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv := httptest.NewServer(server.New(s, opts))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "workflows.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
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
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if strings.HasPrefix(path, "/api/v2") {
			route, params, err := routes.FindRoute(req)
			if err != nil {
				t.Fatal(err)
			}
			v := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
				Status:                 res.StatusCode,
				Header:                 res.Header,
				Options:                &openapi3filter.Options{IncludeResponseStatus: true},
			}
			v.SetBodyBytes(raw)
			if err := openapi3filter.ValidateResponse(context.Background(), v); err != nil {
				t.Fatal(err)
			}
		}
		out := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode %s %s: %v (%s)", method, path, err, raw)
			}
		}
		return res.StatusCode, out
	}
	call("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("workflow-owner")})
	return call, s
}

var errUnrunnable = &unrunnableGraphError{"document cannot execute"}

type unrunnableGraphError struct{ reason string }

func (e *unrunnableGraphError) Error() string { return e.reason }

func workflowTenant(t *testing.T, call workflowCall) string {
	t.Helper()
	_, tenants := call("GET", "/api/v1/me/tenants", nil)
	return tenants["items"].([]any)[0].(map[string]any)["id"].(string)
}

func TestWorkflowAuthoringOverHTTP(t *testing.T) {
	call := workflowServer(t, nil)
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflows"

	status, created := call("POST", base, map[string]any{"name": "  评审流程  ", "graph": nil})
	if status != 201 {
		t.Fatalf("create = %d %v", status, created)
	}
	workflow := created["workflow"].(map[string]any)
	draft := created["draft"].(map[string]any)
	if workflow["name"] != "评审流程" {
		t.Fatalf("name = %v", workflow["name"])
	}
	if workflow["publishedSnapshotId"] != nil {
		t.Fatalf("new workflow is published: %v", workflow["publishedSnapshotId"])
	}
	if draft["version"] != "draft" || draft["updatedAt"] == nil || draft["graph"] == "" {
		t.Fatalf("draft = %v", draft)
	}
	id := workflow["id"].(string)

	status, failure := call("POST", base, map[string]any{"name": "评审流程", "graph": nil})
	if status != 409 || failure["code"] != "workflow_name_conflict" {
		t.Fatalf("duplicate name = %d %v", status, failure)
	}
	status, failure = call("POST", base, map[string]any{"name": "   ", "graph": nil})
	if status != 400 || failure["code"] != "workflow_name_blank" {
		t.Fatalf("blank name = %d %v", status, failure)
	}
	status, failure = call("POST", base, map[string]any{"name": "坏图", "graph": "unrunnable"})
	if status != 201 {
		t.Fatalf("create keeps unvalidated graphs = %d %v", status, failure)
	}

	_, listed := call("GET", base, nil)
	workflows := listed["workflows"].([]any)
	if len(workflows) != 2 || workflows[0].(map[string]any)["name"] != "坏图" {
		t.Fatalf("list order = %v", workflows)
	}
	if workflows[0].(map[string]any)["publishedVersion"] != nil {
		t.Fatalf("unpublished summary has a version: %v", workflows[0])
	}

	status, _ = call("PUT", base+"/"+id, map[string]any{"name": "坏图"})
	if status != 409 {
		t.Fatalf("rename onto an existing name = %d", status)
	}

	status, saved := call("PUT", base+"/"+id+"/draft", map[string]any{"graph": `{"nodes":[{"id":"start","type":"workflow"}],"edges":[]}`})
	if status != 200 {
		t.Fatalf("save draft = %d %v", status, saved)
	}
	status, failure = call("PUT", base+"/"+id+"/draft", map[string]any{})
	if status != 400 || failure["code"] != "invalid_json" {
		t.Fatalf("draft without graph = %d %v", status, failure)
	}
	if saved["snapshot"].(map[string]any)["graph"] != `{"nodes":[{"id":"start","type":"workflow"}],"edges":[]}` {
		t.Fatalf("saved draft = %v", saved)
	}
	_, detail := call("GET", base+"/"+id, nil)
	if detail["published"] != nil {
		t.Fatalf("detail published before publish = %v", detail["published"])
	}
	if detail["workflow"].(map[string]any)["updatedAt"] != workflow["updatedAt"] {
		// A draft save must not move the definition's timestamp; only publish does.
		t.Fatalf("draft save moved the definition: %v -> %v", workflow["updatedAt"], detail["workflow"])
	}

	// Publish freezes the saved draft under the automatic version derived from the injected clock.
	status, published := call("POST", base+"/"+id+"/publish", map[string]any{"version": nil})
	if status != 200 {
		t.Fatalf("publish = %d %v", status, published)
	}
	snapshot := published["snapshot"].(map[string]any)
	if snapshot["version"] != "v1793439000000" || snapshot["updatedAt"] != nil {
		t.Fatalf("published snapshot = %v", snapshot)
	}
	status, failure = call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1793439000000"})
	if status != 409 || failure["code"] != "workflow_version_already_exists" {
		t.Fatalf("duplicate explicit version = %d %v", status, failure)
	}
	status, failure = call("POST", base+"/"+id+"/publish", map[string]any{"version": "draft"})
	if status != 400 || failure["code"] != "workflow_version_reserved" {
		t.Fatalf("reserved version = %d %v", status, failure)
	}
	status, failure = call("POST", base+"/"+id+"/publish", map[string]any{"version": "a/b"})
	if status != 400 || failure["code"] != "workflow_version_invalid" {
		t.Fatalf("invalid version = %d %v", status, failure)
	}
	// An automatic publish whose clock-derived version is taken retries with a suffix instead of
	// failing, as desktop's contract does.
	status, retried := call("POST", base+"/"+id+"/publish", map[string]any{"version": nil})
	if status != 200 || retried["snapshot"].(map[string]any)["version"] != "v1793439000000-1" {
		t.Fatalf("automatic retry = %d %v", status, retried)
	}

	_, versions := call("GET", base+"/"+id+"/versions", nil)
	listedVersions := versions["versions"].([]any)
	if len(listedVersions) != 2 {
		t.Fatalf("versions = %v", versions)
	}
	versionID := ""
	for _, entry := range listedVersions {
		item := entry.(map[string]any)
		if item["version"] == "v1793439000000" {
			versionID = item["id"].(string)
		}
	}
	if versionID == "" {
		t.Fatalf("first version missing: %v", versions)
	}
	status, byVersion := call("GET", base+"/"+id+"/versions/v1793439000000", nil)
	if status != 200 || byVersion["snapshot"].(map[string]any)["id"] != versionID {
		t.Fatalf("get version = %d %v", status, byVersion)
	}
	status, bySnapshot := call("GET", base+"/"+id+"/snapshots/"+versionID, nil)
	if status != 200 || bySnapshot["snapshot"].(map[string]any)["version"] != "v1793439000000" {
		t.Fatalf("get snapshot = %d %v", status, bySnapshot)
	}

	status, rolled := call("POST", base+"/"+id+"/rollback", map[string]any{"snapshotId": versionID})
	if status != 200 || rolled["snapshot"].(map[string]any)["version"] != "draft" {
		t.Fatalf("rollback = %d %v", status, rolled)
	}
	status, activated := call("POST", base+"/"+id+"/activate", map[string]any{"snapshotId": versionID})
	if status != 200 || activated["snapshot"].(map[string]any)["version"] != "draft" {
		t.Fatalf("activate = %d %v", status, activated)
	}
	// Activating the older version moves the pointer, so the newer one becomes deletable while the
	// active and draft ones stay protected.
	status, deletedVersion := call("DELETE", base+"/"+id+"/versions/v1793439000000-1", nil)
	if status != 200 || deletedVersion["snapshotId"] == nil {
		t.Fatalf("delete inactive version = %d %v", status, deletedVersion)
	}
	status, failure = call("DELETE", base+"/"+id+"/versions/v1793439000000", nil)
	if status != 400 || failure["code"] != "workflow_cannot_delete_active_version" {
		t.Fatalf("delete active version = %d %v", status, failure)
	}
	status, failure = call("DELETE", base+"/"+id+"/versions/draft", nil)
	if status != 400 || failure["code"] != "workflow_cannot_delete_draft" {
		t.Fatalf("delete draft = %d %v", status, failure)
	}
	status, failure = call("DELETE", base+"/"+id+"/versions/missing", nil)
	if status != 404 || failure["code"] != "workflow_snapshot_not_found" {
		t.Fatalf("delete missing version = %d %v", status, failure)
	}
	status, deleted := call("DELETE", base+"/"+id, nil)
	if status != 200 || deleted["workflowId"] != id {
		t.Fatalf("delete workflow = %d %v", status, deleted)
	}
	status, failure = call("GET", base+"/"+id, nil)
	if status != 404 || failure["code"] != "workflow_not_found" {
		t.Fatalf("get deleted workflow = %d %v", status, failure)
	}
}

func TestWorkflowAnalysisAndPublishValidation(t *testing.T) {
	call := workflowServer(t, nil)
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflows"

	status, analysis := call("POST", base+"/analyze", map[string]any{"graph": "{}"})
	if status != 200 || len(analysis["unusedNodeIds"].([]any)) != 1 {
		t.Fatalf("analyze = %d %v", status, analysis)
	}

	_, created := call("POST", base, map[string]any{"name": "不可执行", "graph": "unrunnable"})
	id := created["workflow"].(map[string]any)["id"].(string)
	status, failure := call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	if status != 400 || failure["code"] != "workflow_run_graph_parse" {
		t.Fatalf("publish invalid graph = %d %v", status, failure)
	}
}

func TestWorkflowWithoutDomainStillReadsAssets(t *testing.T) {
	call := workflowServer(t, func(o *server.Options) {
		o.AnalyzeWorkflow = nil
		o.ValidateWorkflow = nil
	})
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflows"
	_, created := call("POST", base, map[string]any{"name": "只有定义", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)

	status, failure := call("POST", base+"/analyze", map[string]any{"graph": "{}"})
	if status != 503 || failure["code"] != "workflow_domain_unavailable" {
		t.Fatalf("analyze without domain = %d %v", status, failure)
	}
	status, failure = call("POST", base+"/"+id+"/publish", map[string]any{"version": "v1"})
	if status != 503 || failure["code"] != "workflow_domain_unavailable" {
		t.Fatalf("publish without domain = %d %v", status, failure)
	}
	if _, listed := call("GET", base, nil); len(listed["workflows"].([]any)) != 1 {
		t.Fatalf("list without domain = %v", listed)
	}
	if _, saved := call("PUT", base+"/"+id+"/draft", map[string]any{"graph": "{}"}); saved["snapshot"] == nil {
		t.Fatalf("draft save without domain = %v", saved)
	}
}

func TestWorkflowRoutesRequireMembership(t *testing.T) {
	call := workflowServer(t, nil)
	base := "/api/v2/tenants/00000000-0000-0000-0000-000000000000/workflows"
	status, failure := call("GET", base, nil)
	if status != 404 || failure["code"] != "not_found" {
		t.Fatalf("unknown tenant = %d %v", status, failure)
	}
	status, failure = call("GET", "/api/v2/tenants/not-a-uuid/workflows", nil)
	if status != 404 || failure["code"] != "not_found" {
		t.Fatalf("malformed tenant = %d %v", status, failure)
	}
}

// TestWorkflowPublishRetriesUnderOneClockReading pins the automatic version to the clock value the
// request started with: a collision retry must not read a later instant and freeze under a
// different name than the one it was reserving.
func TestWorkflowPublishRetriesUnderOneClockReading(t *testing.T) {
	base := time.Date(2026, 10, 31, 9, 30, 0, 0, time.UTC)
	second := base.Add(time.Second)
	calls := 0
	call := workflowServer(t, func(o *server.Options) {
		o.Now = func() time.Time {
			calls++
			if calls == 1 {
				return base
			}
			return second.Add(time.Duration(calls-2) * time.Second)
		}
	})
	tid := workflowTenant(t, call)
	basePath := "/api/v2/tenants/" + tid + "/workflows"
	_, created := call("POST", basePath, map[string]any{"name": "时钟", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)

	// Publish#1 consumes the first clock reading; the explicit version occupies the second reading,
	// which the automatic publish below will collide with.
	if status, _ := call("POST", basePath+"/"+id+"/publish", map[string]any{"version": "v" + itoa(second.UnixMilli())}); status != 200 {
		t.Fatalf("prepare colliding version = %d", status)
	}
	status, published := call("POST", basePath+"/"+id+"/publish", map[string]any{"version": nil})
	if status != 200 {
		t.Fatalf("automatic publish = %d %v", status, published)
	}
	if got, want := published["snapshot"].(map[string]any)["version"], "v"+itoa(second.UnixMilli())+"-1"; got != want {
		t.Fatalf("retry version = %v, want %v (clock re-read during retry)", got, want)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

// TestWorkflowRoutePatternsStayDistinct guards the mux against a literal route such as
// /workflows/analyze being swallowed by /workflows/{wid}.
func TestWorkflowRoutePatternsStayDistinct(t *testing.T) {
	calls := atomic.Int32{}
	call := workflowServer(t, func(o *server.Options) {
		o.AnalyzeWorkflow = func(string) (server.WorkflowAnalysis, error) {
			calls.Add(1)
			return server.WorkflowAnalysis{}, nil
		}
	})
	tid := workflowTenant(t, call)
	status, _ := call("POST", "/api/v2/tenants/"+tid+"/workflows/analyze", map[string]any{"graph": "{}"})
	if status != 200 || calls.Load() != 1 {
		t.Fatalf("analyze route = %d, analyzer calls = %d", status, calls.Load())
	}
}

func TestWorkflowVersionEdgeCases(t *testing.T) {
	call := workflowServer(t, nil)
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflows"
	_, created := call("POST", base, map[string]any{"name": "版本边界", "graph": nil})
	id := created["workflow"].(map[string]any)["id"].(string)

	// A user version is any single addressable path segment, including spaces and non-ASCII text.
	unicode := "版本 1"
	status, published := call("POST", base+"/"+id+"/publish", map[string]any{"version": unicode})
	if status != 200 || published["snapshot"].(map[string]any)["version"] != unicode {
		t.Fatalf("unicode version = %d %v", status, published)
	}
	status, byVersion := call("GET", base+"/"+id+"/versions/"+url.PathEscape(unicode), nil)
	if status != 200 || byVersion["snapshot"].(map[string]any)["version"] != unicode {
		t.Fatalf("read unicode version = %d %v", status, byVersion)
	}

	// The bound is 128 bytes, and dot segments or path separators are not addressable versions.
	if status, _ := call("POST", base+"/"+id+"/publish", map[string]any{"version": strings.Repeat("a", 128)}); status != 200 {
		t.Fatalf("128-byte version = %d", status)
	}
	for _, bad := range []string{".", "..", strings.Repeat("a", 129), `a\b`, "a\u0001b"} {
		status, failure := call("POST", base+"/"+id+"/publish", map[string]any{"version": bad})
		if status != 400 || failure["code"] != "workflow_version_invalid" {
			t.Fatalf("invalid version %q = %d %v", bad, status, failure)
		}
	}
}

func TestWorkflowSnapshotLookupIsScopedToItsWorkflow(t *testing.T) {
	call := workflowServer(t, nil)
	tid := workflowTenant(t, call)
	base := "/api/v2/tenants/" + tid + "/workflows"
	_, first := call("POST", base, map[string]any{"name": "第一个", "graph": nil})
	firstID := first["workflow"].(map[string]any)["id"].(string)
	_, published := call("POST", base+"/"+firstID+"/publish", map[string]any{"version": "v1"})
	snapshotID := published["snapshot"].(map[string]any)["id"].(string)
	_, second := call("POST", base, map[string]any{"name": "第二个", "graph": nil})
	secondID := second["workflow"].(map[string]any)["id"].(string)

	if status, _ := call("GET", base+"/"+firstID+"/snapshots/"+snapshotID, nil); status != 200 {
		t.Fatalf("own snapshot = %d", status)
	}
	status, failure := call("GET", base+"/"+secondID+"/snapshots/"+snapshotID, nil)
	if status != 404 || failure["code"] != "workflow_snapshot_not_found" {
		t.Fatalf("foreign snapshot = %d %v", status, failure)
	}
}
