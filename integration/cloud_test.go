// Package integration exercises real HTTP, PostgreSQL constraints, durable effects, and Git.
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

type fixture struct {
	t                      *testing.T
	store                  *core.Store
	client                 *simulator.Client
	controller             *simulator.Controller
	substrate              *simulator.Substrate
	user                   core.Claims
	tid, uid, root, commit string
	cloud                  *httptest.Server
	external               *httptest.Server
	pgConfig               *pgx.ConnConfig
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required; PostgreSQL integration must not skip")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL (task test:integration requires it)")
	}
	config, e := pgx.ParseConfig(dsn)
	must(t, e)
	admin := stdlib.OpenDB(*config)
	must(t, admin.Ping())
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, e = admin.Exec("CREATE SCHEMA " + schema)
	must(t, e)
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config)
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	store, e := core.NewStore(db)
	must(t, e)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	credentials, e := simulator.NewCredentials()
	must(t, e)
	auth, e := core.NewAuthenticator("ora-cloud", credentials.Trust)
	must(t, e)
	gin.SetMode(gin.TestMode)
	log, _ := zap.NewDevelopment()
	cloud := httptest.NewServer(router.New(store, auth, log))
	t.Cleanup(cloud.Close)
	// Git for Windows still limits the linked-worktree GIT_DIR even with core.longpaths.
	root, e := os.MkdirTemp("", "ora-cloud-")
	must(t, e)
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	repo := filepath.Join(root, "source")
	must(t, os.MkdirAll(repo, 0o700))
	runGit(t, "init", "--initial-branch=main", repo)
	runGit(t, "-C", repo, "config", "user.name", "Integration")
	runGit(t, "-C", repo, "config", "user.email", "integration@example.invalid")
	must(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("durable workspace data\n"), 0o600))
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", "fixture")
	commit := runGit(t, "-C", repo, "rev-parse", "HEAD")
	substrate, e := simulator.NewSubstrate(filepath.Join(root, "substrate"), map[string]string{"https://example.invalid/repo.git": repo})
	must(t, e)
	external := httptest.NewServer(substrate)
	t.Cleanup(external.Close)
	client := &simulator.Client{URL: cloud.URL, Credentials: credentials, HTTP: &http.Client{Timeout: 10 * time.Second}, Subject: "controller-a"}
	f := &fixture{t: t, store: store, client: client, substrate: substrate, root: root, commit: commit, cloud: cloud, user: core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "alice"}, Source: "corp", DisplayName: "Alice"}}
	bootstrap, e := store.Bootstrap(context.Background(), "Test tenant", "corp", "alice", "Alice")
	must(t, e)
	f.tid, f.uid = bootstrap.S("tenantId"), bootstrap.S("userId")
	f.controller = &simulator.Controller{Client: client, SubstrateURL: external.URL}
	f.external, f.pgConfig = external, config
	validateHTTP(t, f)
	must(t, f.controller.Acquire(context.Background()))
	return f
}

func TestMigrateUpgradesPreviousSchemaAndData(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required; PostgreSQL integration must not skip")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL (task test:integration requires it)")
	}
	config, err := pgx.ParseConfig(dsn)
	must(t, err)
	admin := stdlib.OpenDB(*config)
	must(t, admin.Ping())
	schema := "test_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	must(t, err)
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		must(t, pool.Close())
		_, dropErr := admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		must(t, dropErr)
		must(t, admin.Close())
	})

	_, err = pool.Exec("CREATE TABLE schema_migrations(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
	must(t, err)
	for _, version := range []string{"0001_core.sql", "0002_aggregate_guards.sql", "0003_resource_versions.sql"} {
		migration, readErr := os.ReadFile(filepath.Join("..", "internal", "core", "migrations", version))
		must(t, readErr)
		_, err = pool.Exec(string(migration))
		must(t, err)
		sum := sha256.Sum256(migration)
		_, err = pool.Exec("INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)", version, hex.EncodeToString(sum[:]))
		must(t, err)
	}

	ids := make([]string, 9)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	tx, err := pool.Begin()
	must(t, err)
	seed := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO users(id,display_name,status) VALUES($1,'Upgrade user','active')", []any{ids[0]}},
		{"INSERT INTO tenants(id,name,status) VALUES($1,'Upgrade tenant','active')", []any{ids[1]}},
		{"INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", []any{ids[1], ids[0]}},
		{"INSERT INTO projects(id,tenant_id,owner_user_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,'Upgrade project','https://example.invalid/upgrade.git','main','active')", []any{ids[2], ids[1], ids[0]}},
		{"INSERT INTO project_storage(project_id,substrate_storage_id,observed_state) VALUES($1,'upgrade-storage','ready')", []any{ids[2]}},
		{"INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,runtime_generation) VALUES($1,$2,$3,$4,'main','running','ready',1)", []any{ids[3], ids[1], ids[0], ids[2]}},
		{"INSERT INTO sandbox_instances(id,workspace_id,generation,substrate_sandbox_id,observed_state) VALUES($1,$2,1,'upgrade-sandbox','running')", []any{ids[4], ids[3]}},
		{"INSERT INTO node_instances(id,sandbox_instance_id,workspace_id,service_subject,connection_state,protocol_version,initialized) VALUES($1,$2,$3,'upgrade-node','connected',1,true)", []any{ids[5], ids[4], ids[3]}},
		{"INSERT INTO execution_tickets(id,workspace_id,node_instance_id,actor_user_id,admission_epoch,kind,state) VALUES($1,$2,$3,$4,1,'interaction','active')", []any{ids[6], ids[3], ids[5], ids[0]}},
		{"INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,'start','succeeded','done','{}','upgrade-operation','upgrade-hash')", []any{ids[7], ids[1], ids[0], ids[2], ids[3]}},
		{"INSERT INTO external_effects(id,operation_id,project_id,kind,state,reconciled_epoch) VALUES($1,$2,$3,'storage_ensure','succeeded',1)", []any{ids[8], ids[7], ids[2]}},
	}
	for _, statement := range seed {
		_, err = tx.Exec(statement.query, statement.args...)
		must(t, err)
	}
	must(t, tx.Commit())

	store := &core.Store{Pool: pool}
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))
	var request []byte
	must(t, pool.QueryRow("SELECT request FROM external_effects WHERE id=$1", ids[8]).Scan(&request))
	var intent core.Object
	must(t, json.Unmarshal(request, &intent))
	if intent.S("kind") != "storage_ensure" || intent.S("projectId") != ids[2] {
		t.Fatalf("external effect intent was not backfilled: %v", intent)
	}
	var tenantID string
	must(t, pool.QueryRow("SELECT tenant_id::text FROM execution_tickets WHERE id=$1", ids[6]).Scan(&tenantID))
	if tenantID != ids[1] {
		t.Fatalf("ticket tenant was not backfilled: want %s got %s", ids[1], tenantID)
	}
}

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	b, e := exec.Command("git", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}

func (f *fixture) call(method, path string, body core.Object, key string, want int) core.Object {
	f.t.Helper()
	o, status, e := f.client.Call(context.Background(), method, path, "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, key, body)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("%s %s: want %d got %d %v", method, path, want, status, o)
	}
	return o
}
func (f *fixture) path(s string) string { return "/api/v1/tenants/" + f.tid + s }
func (f *fixture) create(key string) core.Object {
	return f.call("POST", f.path("/projects"), core.Object{"name": "Project", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"}, key, 202)
}
func (f *fixture) drain() { f.t.Helper(); must(f.t, f.controller.Drain(context.Background())) }
func (f *fixture) ws(wid string) core.Object {
	return f.call("GET", f.path("/workspaces/"+wid), nil, "", 200)
}

func (f *fixture) scalar(q string, args ...any) int {
	f.t.Helper()
	var n int
	must(f.t, f.store.Pool.QueryRow(q, args...).Scan(&n))
	return n
}

func (f *fixture) node(wid string) core.Claims {
	f.t.Helper()
	var nid, sid string
	var generation int64
	must(f.t, f.store.Pool.QueryRow("SELECT n.id,s.id,s.generation FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND s.terminated_at IS NULL AND n.ended_at IS NULL", wid).Scan(&nid, &sid, &generation))
	return core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: nid}, WorkspaceID: wid, SandboxID: sid, Generation: generation}
}

func (f *fixture) finishTicket(ticketID string, node core.Claims) core.Object {
	f.t.Helper()
	var ticketVersion int64
	must(f.t, f.store.Pool.QueryRow("SELECT version FROM execution_tickets WHERE id=$1", ticketID).Scan(&ticketVersion))
	out, status, err := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/tickets/"+ticketID+"/finish", "node", node, nil, "", core.Object{"version": ticketVersion})
	must(f.t, err)
	if status != 200 {
		f.t.Fatalf("finish ticket: want 200 got %d: %v", status, out)
	}
	return out
}

func (f *fixture) internal(path string, body core.Object, want int) core.Object {
	f.t.Helper()
	o, status, e := f.client.Call(context.Background(), "POST", path, "controller", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: f.client.Subject}}, &f.user, "", body)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("%s want %d got %d: %v", path, want, status, o)
	}
	return o
}

func TestHTTPProjectLifecycleAndDurableRecovery(t *testing.T) {
	f := setup(t)
	created := f.create("create")
	pid, wid, oid := created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
	duplicate := f.create("create")
	if duplicate.O("resource").S("id") != pid {
		t.Fatal("idempotency created another project")
	}
	f.call("POST", f.path("/projects"), core.Object{"name": "Different", "repositoryUrl": "https://example.invalid/repo.git"}, "create", 409)
	f.substrate.SetFault("worktree_ensure", "lose_response")
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("expected lost external response")
	}
	if f.scalar("SELECT count(*) FROM external_effects WHERE operation_id=$1", oid) != 2 {
		t.Fatal("write-ahead effects missing")
	}
	f.substrate.SetFault("worktree_ensure", "")
	deferred := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if deferred.S("state") != "retry_wait" || deferred.S("errorCode") != "substrate_timeout" {
		t.Fatal("response loss was not deferred", deferred)
	}
	f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": deferred.N("version")}, "worktree-retry", 202)
	f.drain()
	ready := f.ws(wid)
	if ready.S("observedState") != "ready" {
		t.Fatal(ready)
	}
	checkout := filepath.Join(f.substrate.Root, "projects", pid, "workspaces", wid, "checkout")
	if runGit(t, "-C", checkout, "rev-parse", "HEAD") != f.commit {
		t.Fatal("real commit not resolved")
	}
	if !strings.Contains(runGit(t, "--git-dir", filepath.Join(f.substrate.Root, "projects", pid, "repository.git"), "worktree", "list", "--porcelain"), wid) {
		t.Fatal("main is not linked worktree")
	}
	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Task", "baseRef": "main"}, "isolated", 202)
	iwid := isolated.O("resource").S("id")
	f.drain()
	if f.scalar("SELECT count(*) FROM tasks WHERE workspace_id=$1", iwid) != 1 {
		t.Fatal("task display identity missing")
	}
	f.call("DELETE", f.path("/workspaces/"+wid), core.Object{"version": f.ws(wid).N("version")}, "main-delete", 409)
	data := filepath.Join(f.substrate.Root, "projects", pid, "workspaces", iwid, "runtime", "state.txt")
	must(t, os.WriteFile(data, []byte("persistent"), 0o600))
	oldNode := f.node(iwid)
	stop := f.call("POST", f.path("/workspaces/"+iwid+"/stop"), core.Object{"version": f.ws(iwid).N("version")}, "stop", 202)
	f.drain()
	if f.ws(iwid).S("observedState") != "stopped" {
		t.Fatal("not stopped")
	}
	if _, e := os.Stat(data); e != nil {
		t.Fatal("stop deleted persistent data")
	}
	// Same key is checked before the now-stale version.
	retryStop := f.call("POST", f.path("/workspaces/"+iwid+"/stop"), core.Object{"version": isolated.O("resource").N("version") + 3}, "different-stop", 409)
	_ = retryStop
	if stop.O("operation").S("id") == "" {
		t.Fatal(stop)
	}
	f.call("POST", f.path("/workspaces/"+iwid+"/start"), core.Object{"version": f.ws(iwid).N("version")}, "restart", 202)
	f.drain()
	if f.ws(iwid).N("runtimeGeneration") != 2 {
		t.Fatal("generation not advanced")
	}
	if _, e := os.Stat(data); e != nil {
		t.Fatal("replacement lost data")
	}
	_, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/status", "node", oldNode, nil, "", core.Object{"version": 1, "connectionState": "connected", "initialized": true})
	must(t, e)
	if status != 409 {
		t.Fatal("late old Node accepted", status)
	}
	del := f.call("DELETE", f.path("/workspaces/"+iwid), core.Object{"version": f.ws(iwid).N("version")}, "delete-isolated", 202)
	f.substrate.SetFault("worktree_delete", "fail")
	if e = f.controller.Drain(context.Background()); e == nil {
		t.Fatal("expected cleanup failure")
	}
	op := f.call("GET", f.path("/operations/"+del.O("operation").S("id")), nil, "", 200)
	if op.S("state") != "retry_wait" || op.S("errorCode") != "git_cleanup_failed" {
		t.Fatal("cleanup failure was not deferred", op)
	}
	if _, e = os.Stat(data); e != nil {
		t.Fatal("failed cleanup lost tracking/data")
	}
	f.substrate.SetFault("worktree_delete", "")
	f.call("POST", f.path("/operations/"+op.S("id")+"/retry"), core.Object{"version": op.N("version")}, "isolated-cleanup-retry", 202)
	f.drain()
	f.call("GET", f.path("/workspaces/"+iwid), nil, "", 404)
	p := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": p.N("version")}, "delete-project", 202)
	f.drain()
	f.call("GET", f.path("/projects/"+pid), nil, "", 404)
	if _, e = os.Stat(filepath.Join(f.substrate.Root, "projects", pid)); !os.IsNotExist(e) {
		t.Fatal("project storage survived delete", e)
	}
}

func TestIdentityConcurrencyMembershipAndIsolation(t *testing.T) {
	f := setup(t)
	f.user.Subject = "new-user"
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	errs := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, status, e := f.client.Call(context.Background(), "GET", "/api/v1/me", "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, "", nil)
			if e != nil || status != 200 {
				errs <- fmt.Sprint(status, e)
				return
			}
			ids <- o.S("id")
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	id := ""
	for got := range ids {
		if id == "" {
			id = got
		}
		if id != got {
			t.Fatal("duplicate identity users")
		}
	}
	if f.scalar("SELECT count(*) FROM users") != 2 {
		t.Fatal("orphan users from login race")
	}
	f.call("POST", f.path("/projects"), core.Object{"name": "Denied", "repositoryUrl": "https://example.invalid/repo.git"}, "no-member", 403)
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+id), core.Object{"role": "member", "status": "active", "version": 0}, "", 200)
	created := f.create("owner-project")
	f.drain()
	pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
	f.user.Subject = "new-user"
	f.call("GET", f.path("/projects/"+pid), nil, "", 404)
	f.call("GET", f.path("/workspaces/"+wid), nil, "", 404)
	f.call("GET", f.path("/operations/"+created.O("operation").S("id")), nil, "", 404)
	list := f.call("GET", f.path("/projects"), nil, "", 200)
	if len(list["items"].([]any)) != 0 {
		t.Fatal("list owner filter missing")
	}
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+id), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	f.user.Subject = "new-user"
	statusView := f.call("GET", f.path("/resource-status"), nil, "", 200)
	encoded, _ := json.Marshal(statusView)
	if bytes.Contains(encoded, []byte("repository")) || bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("result")) {
		t.Fatal("admin view leaks", string(encoded))
	}
	adminStop := f.call("POST", f.path("/workspaces/"+wid+"/administrative-stop"), core.Object{"version": f.scalar("SELECT version FROM workspaces WHERE id=$1", wid)}, "admin-stop", 202)
	f.drain()
	adminOp := f.call("GET", f.path("/operations/"+adminStop.O("operation").S("id")), nil, "", 200)
	for _, o := range []core.Object{adminStop.O("operation"), adminOp} {
		if _, exists := o["request"]; exists {
			t.Fatal("admin operation leaks request")
		}
		if _, exists := o["result"]; exists {
			t.Fatal("admin operation leaks result")
		}
	}
	other, e := f.store.Bootstrap(context.Background(), "Other", "corp", "other", "Other")
	must(t, e)
	original := f.tid
	f.tid = other.S("tenantId")
	f.call("GET", f.path("/projects/"+pid), nil, "", 403)
	f.tid = original
}

func TestStopAdmissionRaceAndIdleEvidence(t *testing.T) {
	f := setup(t)
	created := f.create("project")
	f.drain()
	wid := created.O("workspace").S("id")
	node := f.node(wid)
	ticketID := uuid.NewString()
	body := core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "kind": "interaction", "ticketId": ticketID, "epoch": f.controller.Epoch}
	f.internal("/internal/v1/access", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "epoch": f.controller.Epoch}, 200)
	ticket := f.internal("/internal/v1/admissions", body, 200)
	f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": f.ws(wid).N("version")}, "busy-stop", 409)
	if !f.ws(wid).B("admissionOpen") {
		t.Fatal("rejected stop closed admission")
	}
	_, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/tickets/"+ticketID+"/finish", "node", node, nil, "", core.Object{"version": ticket.N("version") + 1})
	must(t, e)
	if status != 409 {
		t.Fatal("stale ticket version accepted", status)
	}
	finished := f.finishTicket(ticketID, node)
	replay, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/tickets/"+ticketID+"/finish", "node", node, nil, "", core.Object{"version": ticket.N("version")})
	must(t, e)
	if status != 200 || replay.N("version") != finished.N("version") {
		t.Fatal("finished ticket replay was not idempotent", status, replay)
	}
	version := f.ws(wid).N("version")
	var stopStatus, admitStatus int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, stopStatus, _ = f.client.Call(context.Background(), "POST", f.path("/workspaces/"+wid+"/stop"), "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, "race-stop", core.Object{"version": version})
	}()
	body["ticketId"] = uuid.NewString()
	go func() {
		defer wg.Done()
		_, admitStatus, _ = f.client.Call(context.Background(), "POST", "/internal/v1/admissions", "controller", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: f.client.Subject}}, &f.user, "", body)
	}()
	wg.Wait()
	if !((stopStatus == 202 && admitStatus == 409) || (stopStatus == 409 && admitStatus == 200)) {
		t.Fatalf("unsafe race outcomes stop=%d admission=%d", stopStatus, admitStatus)
	}
	if admitStatus == 200 {
		f.finishTicket(body.S("ticketId"), node)
		f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": f.ws(wid).N("version")}, "final-stop", 202)
	}
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200).O("operation")
	f.internal("/internal/v1/operations/"+claimed.S("id")+"/advance", core.Object{"epoch": f.controller.Epoch, "version": claimed.N("version")}, 409)
	var nv int64
	must(t, f.store.Pool.QueryRow("SELECT version FROM node_instances WHERE id=$1", node.Subject).Scan(&nv))
	_, status, e = f.client.Call(context.Background(), "POST", "/internal/v1/nodes/idle", "node", node, nil, "", core.Object{"version": nv, "admissionEpoch": f.ws(wid).N("admissionEpoch"), "operationId": claimed.S("id"), "idle": false})
	must(t, e)
	if status != 200 {
		t.Fatal(status)
	}
	if !f.ws(wid).B("admissionOpen") {
		t.Fatal("node refusal failed to restore admission")
	}
}
