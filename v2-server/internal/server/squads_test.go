package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

type squadFixture struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	db    *pgx.Conn
	srv   *httptest.Server
}

func newSquadServer(t *testing.T) *squadFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })
	var b [8]byte
	_, _ = rand.Read(b[:])
	schema := "squadhttp_" + hex.EncodeToString(b[:])
	if _, e := db.Exec(ctx, "CREATE SCHEMA "+schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	if _, e := db.Exec(ctx, "SET search_path TO "+schema+",public"); e != nil {
		t.Fatal(e)
	}
	config, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	s, e := store.OpenConfig(ctx, config)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	seedTestPluginCatalog(t, s)
	storetest.SignIn(t, s, "squad-demo", "演示用户", store.RoleOwner)
	srv := httptest.NewServer(server.New(s, server.Options{SessionTTL: time.Hour}))
	t.Cleanup(srv.Close)
	return &squadFixture{t: t, ctx: ctx, store: s, db: db, srv: srv}
}

type squadClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	routes routers.Router
}

func (f *squadFixture) client() *squadClient {
	f.t.Helper()
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "squads.openapi.json"))
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
	return &squadClient{t: f.t, base: f.srv.URL, http: &http.Client{Jar: jar}, routes: routes}
}

// do sends a request and validates responses of the candidate squad contract.
func (c *squadClient) do(method, path string, body any) (int, map[string]any) {
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
			c.t.Fatalf("%s %s is not in squads.openapi.json: %v", method, path, e)
		}
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 res.StatusCode, Header: res.Header,
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		in.SetBodyBytes(raw)
		if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
			c.t.Fatalf("%s %s does not match squads.openapi.json: %v", method, path, e)
		}
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (c *squadClient) login() {
	c.t.Helper()
	if status, body := c.do("POST", "/auth/login", map[string]string{"provider": "email", "email": storetest.Email("squad-demo")}); status != 200 {
		c.t.Fatalf("login = %d %v", status, body)
	}
}

func (f *squadFixture) tenant(t *testing.T) string {
	t.Helper()
	token := storetest.SignIn(t, f.store, "squad-demo", "演示用户", store.RoleOwner)
	u, e := f.store.SessionUser(f.ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	ts, e := f.store.Tenants(f.ctx, u.ID)
	if e != nil || len(ts) != 1 {
		t.Fatalf("tenants = %v %v", ts, e)
	}
	return ts[0].ID
}

func TestSquadHTTPLifecycle(t *testing.T) {
	f := newSquadServer(t)
	c := f.client()
	c.login()
	tid := f.tenant(t)
	base := "/api/v2/tenants/" + tid + "/squads"

	if status, body := c.do("GET", base, nil); status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatalf("empty list = %d %v", status, body)
	}
	status, created := c.do("POST", base, map[string]any{"name": "产品小队", "description": "一起做产品", "charter": "上午评审"})
	if status != 201 || created["version"] != float64(1) || created["humanCount"] != float64(1) || created["maintainerCount"] != float64(1) {
		t.Fatalf("create = %d %v", status, created)
	}
	sid := created["id"].(string)
	members := created["members"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["maintainer"] != true || members[0].(map[string]any)["type"] != "human" {
		t.Fatalf("creator = %v", members)
	}

	// Version is required and stale versions conflict without changing the row.
	update := map[string]any{"name": "改名", "description": "", "charter": "", "leaderAgentId": nil}
	if status, body := c.do("PUT", base+"/"+sid, update); status != 428 || body["code"] != "version_required" {
		t.Fatalf("update without version = %d %v", status, body)
	}
	update["version"] = 2
	if status, body := c.do("PUT", base+"/"+sid, update); status != 409 || body["code"] != "version_conflict" {
		t.Fatalf("stale update = %d %v", status, body)
	}
	update["version"] = 1
	status, updated := c.do("PUT", base+"/"+sid, update)
	if status != 200 || updated["name"] != "改名" || updated["version"] != float64(2) {
		t.Fatalf("update = %d %v", status, updated)
	}

	// The creator cannot be added twice; adding an existing member succeeds and bumps the version.
	if status, body := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "human", "userId": members[0].(map[string]any)["userId"], "role": "", "maintainer": false, "version": 2,
	}); status != 409 || body["code"] != "member_exists" {
		t.Fatalf("duplicate add = %d %v", status, body)
	}
	token := storetest.SignIn(t, f.store, "second", "第二人", store.RoleOwner)
	second, e := f.store.SessionUser(f.ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE memberships SET role = 'member' WHERE tenant_id = $1 AND user_id = $2`, tid, second.ID); e != nil {
		t.Fatal(e)
	}
	status, withHuman := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "human", "userId": second.ID, "role": "评审", "maintainer": false, "version": 2,
	})
	if status != 201 || withHuman["humanCount"] != float64(2) || withHuman["version"] != float64(3) {
		t.Fatalf("add human = %d %v", status, withHuman)
	}

	// Agent members require an active Space Agent in the same workspace; retired ones are rejected.
	var agentID string
	if e := f.db.QueryRow(f.ctx, `INSERT INTO v2_space_agents (tenant_id, name) VALUES ($1, '研究员') RETURNING id`, tid).Scan(&agentID); e != nil {
		t.Fatal(e)
	}
	status, withAgent := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "agent", "spaceAgentId": agentID, "role": "调研", "maintainer": false, "version": 3,
	})
	if status != 201 || withAgent["agentCount"] != float64(1) {
		t.Fatalf("add agent = %d %v", status, withAgent)
	}
	var retired string
	if e := f.db.QueryRow(f.ctx, `INSERT INTO v2_space_agents (tenant_id, name, status) VALUES ($1, '退役', 'retired') RETURNING id`, tid).Scan(&retired); e != nil {
		t.Fatal(e)
	}
	if status, body := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "agent", "spaceAgentId": retired, "role": "", "maintainer": false, "version": 4,
	}); status != 400 || body["code"] != "invalid_space_agent" {
		t.Fatalf("retired agent = %d %v", status, body)
	}
	if status, body := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "agent", "spaceAgentId": agentID, "role": "", "maintainer": true, "version": 4,
	}); status != 400 || body["code"] != "invalid_member" {
		t.Fatalf("agent maintainer = %d %v", status, body)
	}

	// A workspace member who is not a maintainer reads but cannot mutate.
	other := f.client()
	req, _ := http.NewRequest("GET", other.base, nil)
	other.http.Jar.SetCookies(req.URL, []*http.Cookie{{Name: server.SessionCookie, Value: token}})
	if status, body := other.do("GET", base, nil); status != 200 || len(body["items"].([]any)) != 1 {
		t.Fatalf("member read = %d %v", status, body)
	}
	if status, body := other.do("PUT", base+"/"+sid, map[string]any{"name": "越权", "description": "", "charter": "", "leaderAgentId": nil, "version": 4}); status != 403 || body["code"] != "forbidden" {
		t.Fatalf("non-maintainer write = %d %v", status, body)
	}

	// Removing a member needs the current version; the removed member leaves the live counts.
	var memberID string
	for _, raw := range withAgent["members"].([]any) {
		m := raw.(map[string]any)
		if m["userId"] == second.ID && m["endedAt"] == nil {
			memberID = m["id"].(string)
		}
	}
	if memberID == "" {
		t.Fatalf("second membership missing: %v", withAgent["members"])
	}
	if status, body := c.do("DELETE", base+"/"+sid+"/members/"+memberID, nil); status != 428 || body["code"] != "version_required" {
		t.Fatalf("delete without version = %d %v", status, body)
	}
	status, removed := c.do("DELETE", base+"/"+sid+"/members/"+memberID+"?version=4", nil)
	if status != 200 || removed["humanCount"] != float64(1) {
		t.Fatalf("remove = %d %v", status, removed)
	}

	// Archiving ends live grants, keeps the history and rejects later joins.
	status, archived := c.do("POST", base+"/"+sid+"/archive", map[string]any{"version": 5})
	if status != 200 || archived["archivedAt"] == nil || archived["humanCount"] != float64(0) {
		t.Fatalf("archive = %d %v", status, archived)
	}
	if status, body := c.do("GET", base+"/"+sid, nil); status != 200 || len(body["members"].([]any)) < 3 {
		t.Fatalf("archived detail = %d %v", status, body)
	}
	if status, body := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "human", "userId": second.ID, "role": "", "maintainer": false, "version": 6,
	}); status != 409 || body["code"] != "squad_archived" {
		t.Fatalf("add after archive = %d %v", status, body)
	}
	if status, body := c.do("GET", base, nil); status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatalf("archived listed = %d %v", status, body)
	}

	// Unauthenticated and cross-tenant access stay indistinguishable from absent resources.
	anon := f.client()
	if status, _ := anon.do("GET", base, nil); status != 401 {
		t.Fatalf("anonymous list = %d", status)
	}
	if status, _ := c.do("GET", "/api/v2/tenants/00000000-0000-0000-0000-000000000001/squads", nil); status != 404 {
		t.Fatalf("cross-tenant list = %d", status)
	}
	if status, _ := c.do("GET", base+"/00000000-0000-0000-0000-000000000001", nil); status != 404 {
		t.Fatalf("missing squad = %d", status)
	}
}

func TestSquadHTTPSelfExit(t *testing.T) {
	f := newSquadServer(t)
	c := f.client()
	c.login()
	tid := f.tenant(t)
	base := "/api/v2/tenants/" + tid + "/squads"

	status, created := c.do("POST", base, map[string]any{"name": "退出小队", "description": "", "charter": ""})
	if status != 201 {
		t.Fatalf("create = %d %v", status, created)
	}
	sid := created["id"].(string)

	// A second workspace member joins as an ordinary (non-maintainer) human.
	token := storetest.SignIn(t, f.store, "second", "第二人", store.RoleOwner)
	second, e := f.store.SessionUser(f.ctx, token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE memberships SET role = 'member' WHERE tenant_id = $1 AND user_id = $2`, tid, second.ID); e != nil {
		t.Fatal(e)
	}
	status, withMember := c.do("POST", base+"/"+sid+"/members", map[string]any{
		"type": "human", "userId": second.ID, "role": "评审", "maintainer": false, "version": 1,
	})
	if status != 201 || withMember["humanCount"] != float64(2) {
		t.Fatalf("add member = %d %v", status, withMember)
	}
	var memberID string
	for _, raw := range withMember["members"].([]any) {
		m := raw.(map[string]any)
		if m["userId"] == second.ID && m["endedAt"] == nil {
			memberID = m["id"].(string)
		}
	}
	if memberID == "" {
		t.Fatalf("second membership missing: %v", withMember["members"])
	}

	// The member uses their own session and exits their own record without being a maintainer.
	other := f.client()
	req, _ := http.NewRequest("GET", other.base, nil)
	other.http.Jar.SetCookies(req.URL, []*http.Cookie{{Name: server.SessionCookie, Value: token}})
	status, removed := other.do("DELETE", base+"/"+sid+"/members/"+memberID+"?version=2", nil)
	if status != 200 || removed["humanCount"] != float64(1) {
		t.Fatalf("self-exit = %d %v", status, removed)
	}
}
