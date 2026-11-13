package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestProjectRepositoryLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("v2 repository service requires Linux/WSL")
	}
	s := openStore(t)
	remote, commit, requests := projectGitRemote(t)
	root := t.TempDir()
	options := projectrepos.Options{Root: root, AllowLoopbackHTTP: true}
	projects, err := projectrepos.New(s, options)
	if err != nil {
		t.Fatal(err)
	}
	storetest.SignIn(t, s, "project-owner", "Owner", store.RoleOwner)
	var createdProjects []store.Project
	serverOptions := server.Options{SessionTTL: time.Hour, Projects: projects, ProjectCreated: func(p store.Project) { createdProjects = append(createdProjects, p) }}
	srv := httptest.NewServer(server.New(s, serverOptions))
	defer func() { srv.Close() }()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "projectrepos", "projects.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	routes, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, input any) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(input)
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(path, "/api/v2/") {
			route, params, err := routes.FindRoute(req)
			if err != nil {
				t.Fatal(err)
			}
			in := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}, Status: res.StatusCode, Header: res.Header, Options: &openapi3filter.Options{IncludeResponseStatus: true}}
			in.SetBodyBytes(raw)
			if err := openapi3filter.ValidateResponse(t.Context(), in); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
		out := map[string]any{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	const absentTenant = "11111111-1111-4111-8111-111111111111"
	if status, _ := call("GET", "/api/v2/tenants/"+absentTenant+"/projects", nil); status != 401 {
		t.Fatalf("anonymous status=%d", status)
	}
	call("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("project-owner")})
	_, who := call("GET", "/api/v1/me", nil)
	_, tenants := call("GET", "/api/v1/me/tenants", nil)
	tenant := tenants["items"].([]any)[0].(map[string]any)["id"].(string)
	path := "/api/v2/tenants/" + tenant + "/projects"
	input := projectrepos.Input{Name: "Demo", RepositoryURL: remote.URL + "/repo.git", DefaultBranch: "missing"}
	if status, out := call("POST", path, input); status != 400 || out["code"] != "repository_unavailable" {
		t.Fatalf("missing branch: %d %v", status, out)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed preparation left files: %v %v", entries, err)
	}
	if _, list := call("GET", path, nil); len(list["items"].([]any)) != 0 {
		t.Fatalf("failed preparation created project: %v", list)
	}

	// Poison inherited Git config; clone must use its own environment and ignore URL rewrites.
	poison := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(poison, []byte("[url \"file:///forbidden/\"]\n insteadOf = http://\n[credential]\n helper = !exit 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", poison)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.http.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "never")
	input.DefaultBranch = "main"
	status, created := call("POST", path, input)
	if status != 201 || created["commitId"] != commit || created["ownerUserId"] != who["id"] {
		t.Fatalf("create: %d %v", status, created)
	}
	id := created["id"].(string)
	scope := execution.Scope{TenantID: tenant, UserID: who["id"].(string)}
	stored, err := s.GetProject(t.Context(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MirrorPath != filepath.Join(root, id+".git") || stored.SnapshotRef != projectrepos.SnapshotRef(id, commit) {
		t.Fatalf("stored repository=%+v", stored)
	}
	// Only the stored project is announced (to prewarm its Node); failed creations are not.
	if len(createdProjects) != 1 || createdProjects[0] != stored {
		t.Fatalf("announced projects=%+v want %+v", createdProjects, stored)
	}
	if got := projectGit(t, stored.MirrorPath, "rev-parse", stored.SnapshotRef); got != commit {
		t.Fatalf("snapshot=%q want=%q", got, commit)
	}
	if remotes := projectGit(t, stored.MirrorPath, "remote"); remotes != "" {
		t.Fatalf("upstream remains configured: %q", remotes)
	}
	if status, got := call("GET", path+"/"+id, nil); status != 200 || got["commitId"] != commit {
		t.Fatalf("read: %d %v", status, got)
	}
	if status, got := call("GET", path+"?limit=0", nil); status != 400 {
		t.Fatalf("invalid page: %d %v", status, got)
	}
	if status, got := call("POST", path, map[string]any{"name": "x", "repositoryUrl": input.RepositoryURL, "defaultBranch": "main", "credentialRefId": absentTenant}); status != 400 {
		t.Fatalf("credential rejected: %d %v", status, got)
	}
	before := requests.Load()
	if status, _ := call("POST", "/api/v2/tenants/"+absentTenant+"/projects", input); status != 404 {
		t.Fatalf("foreign tenant status=%d", status)
	}
	if requests.Load() != before {
		t.Fatal("unauthorized create contacted repository")
	}

	// Restart the service after the source disappears: reads use the saved mirror and database.
	remote.Close()
	srv.Close()
	projects, err = projectrepos.New(s, options)
	if err != nil {
		t.Fatal(err)
	}
	serverOptions.Projects = projects
	srv = httptest.NewServer(server.New(s, serverOptions))
	call("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("project-owner")})
	if status, got := call("GET", path+"/"+id, nil); status != 200 || got["commitId"] != commit {
		t.Fatalf("restart read: %d %v", status, got)
	}
	if status, got := call("GET", path, nil); status != 200 || len(got["items"].([]any)) != 1 {
		t.Fatalf("restart list: %d %v", status, got)
	}
	if requests.Load() != before {
		t.Fatal("read/restart fetched upstream")
	}
	if status, _ := call("POST", path, input); status != 400 {
		t.Fatalf("unavailable source status=%d", status)
	}
	if _, got := call("GET", path, nil); len(got["items"].([]any)) != 1 {
		t.Fatal("failed clone persisted project")
	}

	// A second tenant member sees the recorded creator, not their own identity.
	token := storetest.SignIn(t, s, "project-reader", "Reader", store.RoleOwner)
	base, _ := http.NewRequest("GET", srv.URL, nil)
	jar.SetCookies(base.URL, []*http.Cookie{{Name: server.SessionCookie, Value: token}})
	if status, got := call("GET", path+"/"+id, nil); status != 200 || got["ownerUserId"] != who["id"] {
		t.Fatalf("shared creator: %d %v", status, got)
	}
}

func projectGitRemote(t *testing.T) (*httptest.Server, string, *atomic.Int64) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	projectGit(t, work, "init", "-b", "main")
	projectGit(t, work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "initial")
	commit := projectGit(t, work, "rev-parse", "HEAD")
	projectGit(t, root, "clone", "--bare", work, filepath.Join(root, "repo.git"))
	backend := filepath.Join(projectGit(t, root, "--exec-path"), "git-http-backend")
	cgiHandler := &cgi.Handler{Path: backend, Root: "/", Dir: root, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	requests := &atomic.Int64{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); cgiHandler.ServeHTTP(w, r) }))
	t.Cleanup(remote.Close)
	return remote, commit, requests
}

func TestTrustedDemoRepositorySeed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("v2 repository service requires Linux/WSL")
	}
	s := openStore(t)
	token := storetest.SignIn(t, s, "fixture-owner", "Owner", store.RoleOwner)
	user, err := s.SessionUser(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := s.Tenants(t.Context(), user.ID)
	if err != nil || len(tenants) != 1 {
		t.Fatalf("tenants=%v err=%v", tenants, err)
	}
	scope := execution.Scope{TenantID: tenants[0].ID, UserID: user.ID}
	root := t.TempDir()
	projects, err := projectrepos.New(s, projectrepos.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		project store.Project
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { p, err := projects.SeedDemo(t.Context(), scope); results <- result{p, err} }()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.project.ID != second.project.ID {
		t.Fatalf("seed results: %+v %+v", first, second)
	}
	p := first.project
	if p.RepositoryURL != "fixture://demo" || p.OwnerUserID != scope.UserID || p.SnapshotRef != projectrepos.SnapshotRef(p.ID, p.Commit) {
		t.Fatalf("seed=%+v", p)
	}
	if got := projectGit(t, p.MirrorPath, "show", p.Commit+":README.md"); !strings.Contains(got, "# Ora demo") {
		t.Fatalf("seed README=%q", got)
	}
	if _, err := projects.Create(t.Context(), scope, projectrepos.Input{Name: "untrusted", RepositoryURL: "fixture://demo", DefaultBranch: "main"}); err != execution.ErrInvalidRequest {
		t.Fatalf("public fixture request=%v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != p.ID+".git" {
		t.Fatalf("seed directory entries=%v err=%v", entries, err)
	}
	projects, err = projectrepos.New(s, projectrepos.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	again, err := projects.SeedDemo(t.Context(), scope)
	if err != nil || again.ID != p.ID || again.Commit != p.Commit {
		t.Fatalf("restarted seed=%+v err=%v", again, err)
	}
}

func projectGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
