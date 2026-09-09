package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

func TestCredentialVerificationAndDisabledAccounts(t *testing.T) {
	f := setup(t)
	gateway, err := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(t, err)
	valid := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "ora-simulator", Subject: "alice", Audience: jwt.ClaimStrings{"ora-cloud"}, IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}, Kind: "user", Source: "corp", Caller: "gateway-a"}
	cases := []string{"expired", "wrong-audience", "wrong-issuer", "caller-mismatch", "forged", "none", "future", "too-long", "missing-expiry", "source-missing"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			claims := valid
			key := any(f.client.Credentials.Private["user"])
			method := jwt.SigningMethod(jwt.SigningMethodEdDSA)
			switch name {
			case "expired":
				claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
				claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
			case "wrong-audience":
				claims.Audience = jwt.ClaimStrings{"ora-controller"}
			case "wrong-issuer":
				claims.Issuer = "untrusted"
			case "caller-mismatch":
				claims.Caller = "other-gateway"
			case "forged":
				_, private, e := ed25519.GenerateKey(rand.Reader)
				must(t, e)
				key = private
			case "none":
				method = jwt.SigningMethodNone
				key = jwt.UnsafeAllowNoneSignatureType
			case "future":
				claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Minute))
			case "too-long":
				claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
			case "missing-expiry":
				claims.ExpiresAt = nil
			case "source-missing":
				claims.Source = ""
			}
			token := jwt.NewWithClaims(method, claims)
			token.Header["kid"] = "user"
			raw, e := token.SignedString(key)
			must(t, e)
			req, e := http.NewRequest("GET", f.cloud.URL+"/api/v1/me", nil)
			must(t, e)
			req.Header.Set("Authorization", "Bearer "+gateway)
			req.Header.Set("X-Ora-User-Token", raw)
			res, e := f.client.HTTP.Do(req)
			must(t, e)
			defer res.Body.Close()
			if res.StatusCode != 401 {
				t.Fatalf("accepted %s credential: %d", name, res.StatusCode)
			}
		})
	}
	userCredential := f.user
	userCredential.Caller = "gateway-a"
	userToken, err := f.client.Credentials.Token("user", userCredential)
	must(t, err)
	for name, authorization := range map[string]string{"bare": gateway, "wrong-scheme": "Basic " + gateway, "extra-token": "Bearer " + gateway + " extra"} {
		t.Run("authorization-"+name, func(t *testing.T) {
			req, e := http.NewRequest("GET", f.cloud.URL+"/api/v1/me", nil)
			must(t, e)
			req.Header.Set("Authorization", authorization)
			req.Header.Set("X-Ora-User-Token", userToken)
			res, e := f.client.HTTP.Do(req)
			must(t, e)
			defer res.Body.Close()
			if res.StatusCode != 401 {
				t.Fatalf("accepted invalid Authorization form: %d", res.StatusCode)
			}
		})
	}
	// A real signed controller credential cannot use the public gateway surface.
	_, status, e := f.client.Call(context.Background(), "GET", "/api/v1/me", "controller", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "controller-a"}}, &f.user, "", nil)
	must(t, e)
	if status != 403 {
		t.Fatal("service role confused", status)
	}
	f.user.Subject = "disabled"
	user := f.call("GET", "/api/v1/me", nil, "", 200)
	_, e = f.store.Pool.Exec("UPDATE users SET status='disabled' WHERE id=$1", user.S("id"))
	must(t, e)
	f.call("GET", "/api/v1/me", nil, "", 403)
	f.user.Subject = "alice"
	f.user.Source = "another-account-namespace"
	other := f.call("GET", "/api/v1/me", nil, "", 200)
	if other.S("id") == f.uid {
		t.Fatal("source namespaces merged by subject/name")
	}
}

func TestConcurrentIdempotencyAndLastAdminProtection(t *testing.T) {
	f := setup(t)
	var wg sync.WaitGroup
	results := make(chan core.Object, 16)
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, status, e := f.client.Call(context.Background(), "POST", f.path("/projects"), "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, "same-key", core.Object{"name": "Concurrent", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"})
			if e != nil || status != 202 {
				errs <- fmt.Sprint(status, e, o)
				return
			}
			results <- o
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	pid := ""
	for o := range results {
		if pid == "" {
			pid = o.O("resource").S("id")
		}
		if pid != o.O("resource").S("id") {
			t.Fatal("duplicate project")
		}
	}
	if f.scalar("SELECT count(*) FROM projects") != 1 || f.scalar("SELECT count(*) FROM workspaces") != 1 || f.scalar("SELECT count(*) FROM operations") != 1 {
		t.Fatal("non-atomic create")
	}
	f.drain()
	f.user.Subject = "bob"
	bob := f.call("GET", "/api/v1/me", nil, "", 200)
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+bob.S("id")), core.Object{"role": "admin", "status": "active", "version": 0}, "", 200)
	subjects := []string{"alice", "bob"}
	ids := []string{f.uid, bob.S("id")}
	statuses := make(chan int, 2)
	for i := range subjects {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subjects[i]}, Source: "corp"}
			_, status, _ := f.client.Call(context.Background(), "PUT", f.path("/members/"+ids[i]), "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &u, "", core.Object{"role": "member", "status": "active", "version": 1})
			statuses <- status
		}(i)
	}
	wg.Wait()
	close(statuses)
	success, conflict := 0, 0
	for status := range statuses {
		if status == 200 {
			success++
		}
		if status == 409 {
			conflict++
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("last admin race success=%d conflict=%d", success, conflict)
	}
	if f.scalar("SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND role='admin' AND status='active'", f.tid) != 1 {
		t.Fatal("lost last administrator")
	}
}

func TestControllerTakeoverReconcilesAndFences(t *testing.T) {
	f := setup(t)
	created := f.create("create")
	pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
	f.substrate.SetFault("sandbox_ensure", "lose_response")
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("expected response loss")
	}
	oldOperation := f.call("GET", f.path("/operations/"+created.O("operation").S("id")), nil, "", 200)
	if oldOperation.S("state") != "retry_wait" || oldOperation.S("errorCode") != "substrate_timeout" {
		t.Fatal("lost response was not deferred", oldOperation)
	}
	oldEpoch := f.controller.Epoch
	// Test-only clock manipulation avoids sleeping for the production 30-second lease.
	_, e := f.store.Pool.Exec("UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second'")
	must(t, e)
	_, e = f.store.Pool.Exec("UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1", oldOperation.S("id"))
	must(t, e)
	replacementClient := *f.client
	replacementClient.Subject = "controller-b"
	replacement := &simulator.Controller{Client: &replacementClient, SubstrateURL: f.controller.SubstrateURL}
	must(t, replacement.Acquire(context.Background()))
	if replacement.Epoch != oldEpoch+1 {
		t.Fatal("takeover epoch not advanced")
	}
	f.internal("/internal/v1/operations/"+oldOperation.S("id")+"/advance", core.Object{"epoch": oldEpoch, "version": oldOperation.N("version")}, 409)
	f.substrate.SetFault("sandbox_ensure", "")
	must(t, replacement.Drain(context.Background()))
	if f.ws(wid).S("observedState") != "ready" {
		t.Fatal("takeover did not recover")
	}
	if f.scalar("SELECT count(*) FROM sandbox_instances WHERE workspace_id=$1", wid) != 1 {
		t.Fatal("takeover recreated existing sandbox")
	}
	if f.scalar("SELECT count(*) FROM external_effects WHERE project_id=$1 AND kind='sandbox_ensure'", pid) != 1 {
		t.Fatal("effect identity lost")
	}
	f.internal("/internal/v1/admissions", core.Object{"epoch": oldEpoch, "tenantId": f.tid, "workspaceId": wid, "action": "execute", "ticketId": uuid.NewString(), "kind": "task"}, 409)
	// The old holder cannot renew or release the new holder's lease.
	f.internal("/internal/v1/controller-lease/renew", core.Object{"epoch": oldEpoch}, 409)
	f.internal("/internal/v1/controller-lease/release", core.Object{"epoch": oldEpoch}, 409)
}

func TestTerminationUnknownRetryAndVersionedReplay(t *testing.T) {
	f := setup(t)
	created := f.create("create")
	f.drain()
	wid := created.O("workspace").S("id")
	oldVersion := f.ws(wid).N("version")
	stopped := f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": oldVersion}, "stop", 202)
	oid := stopped.O("operation").S("id")
	f.substrate.SetFault("sandbox_terminate", "unconfirmed")
	if e := f.controller.Drain(context.Background()); e == nil {
		t.Fatal("termination must block")
	}
	if f.scalar("SELECT count(*) FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", wid) != 1 {
		t.Fatal("unconfirmed sandbox forgotten")
	}
	f.call("POST", f.path("/workspaces/"+wid+"/start"), core.Object{"version": f.ws(wid).N("version")}, "premature-start", 409)
	deferred := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
	if deferred.S("state") != "blocked" || deferred.S("errorCode") != "termination_unconfirmed" {
		t.Fatal("unconfirmed termination was not blocked", deferred)
	}
	replay := f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": oldVersion}, "stop", 202)
	if replay.O("operation").S("id") != oid {
		t.Fatal("stale-version replay not original operation")
	}
	f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": deferred.N("version")}, "retry", 202)
	f.substrate.SetFault("sandbox_terminate", "")
	f.controller.Operation = nil
	f.drain()
	if f.ws(wid).S("observedState") != "stopped" {
		t.Fatal("retry did not stop")
	}
}

func TestProjectCreationDeletionSerializationAndStrictInputs(t *testing.T) {
	f := setup(t)
	created := f.create("create")
	f.drain()
	pid := created.O("resource").S("id")
	p := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	f.call("PATCH", f.path("/projects/"+pid), core.Object{"version": p.N("version"), "repositoryUrl": "https://example.invalid/other"}, "", 400)
	f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "evil", "baseRef": "main", "relativePath": "../../etc"}, "unsafe-path", 400)
	f.call("POST", f.path("/projects"), core.Object{"name": "secret", "repositoryUrl": "https://user:password@example.invalid/repo"}, "url-password", 400)
	var wg sync.WaitGroup
	var createStatus, deleteStatus int
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, createStatus, _ = f.client.Call(context.Background(), "POST", f.path("/projects/"+pid+"/workspaces"), "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, "race-create", core.Object{"title": "Concurrent", "baseRef": "main"})
	}()
	go func() {
		defer wg.Done()
		_, deleteStatus, _ = f.client.Call(context.Background(), "DELETE", f.path("/projects/"+pid), "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, "race-delete", core.Object{"version": p.N("version")})
	}()
	wg.Wait()
	if (createStatus != 202 || deleteStatus != 409) && (createStatus != 409 || deleteStatus != 202) {
		t.Fatalf("create=%d delete=%d", createStatus, deleteStatus)
	}
	f.drain()
	if deleteStatus == 202 && f.scalar("SELECT count(*) FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL", pid) != 0 {
		t.Fatal("workspace escaped cascade")
	}
}

func TestWrongWorkspaceNodeCannotRefuseAnotherStop(t *testing.T) {
	f := setup(t)
	p := f.create("create")
	f.drain()
	pid, main := p.O("resource").S("id"), p.O("workspace").S("id")
	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Side", "baseRef": "main"}, "side", 202)
	f.drain()
	wid := isolated.O("resource").S("id")
	n := f.node(wid)
	var nv int64
	must(t, f.store.Pool.QueryRow("SELECT version FROM node_instances WHERE id=$1", n.Subject).Scan(&nv))
	out, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/status", "node", n, nil, "", core.Object{"version": nv, "connectionState": "disconnected", "initialized": true})
	must(t, e)
	if status != 200 {
		t.Fatal(out)
	}
	stop := f.call("POST", f.path("/workspaces/"+main+"/stop"), core.Object{"version": f.ws(main).N("version")}, "stop-main", 202)
	_, status, e = f.client.Call(context.Background(), "POST", "/internal/v1/nodes/idle", "node", n, nil, "", core.Object{"version": out.N("version"), "operationId": stop.O("operation").S("id"), "admissionEpoch": f.ws(wid).N("admissionEpoch"), "idle": false})
	must(t, e)
	if status != 409 {
		t.Fatal("wrong workspace affected operation", status)
	}
	op := f.call("GET", f.path("/operations/"+stop.O("operation").S("id")), nil, "", 200)
	if op.S("state") == "failed" {
		t.Fatal("unrelated node canceled stop")
	}
}

func TestPostgresAggregateConstraints(t *testing.T) {
	f := setup(t)
	p := f.create("create")
	pid, wid := p.O("resource").S("id"), p.O("workspace").S("id")
	other, e := f.store.Bootstrap(context.Background(), "Other", "corp", "other", "Other")
	must(t, e)
	checks := []struct {
		name, q string
		args    []any
	}{
		{"project must have main", "INSERT INTO projects(id,tenant_id,owner_user_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,'missing main','https://x','main','active')", []any{uuid.NewString(), f.tid, f.uid}},
		{"main cannot vanish", "UPDATE workspaces SET deleted_at=now() WHERE id=$1", []any{wid}},
		{"main cannot change aggregate", "UPDATE workspaces SET project_id=$1 WHERE id=$2", []any{uuid.NewString(), wid}},
		{"cross-tenant owner", "INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,'isolated','running','provisioning')", []any{uuid.NewString(), other.S("tenantId"), other.S("userId"), pid}},
		{"task requires isolated", "INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'wrong main task')", []any{uuid.NewString(), wid}},
		{"last admin user cannot disable", "UPDATE users SET status='disabled' WHERE id=$1", []any{f.uid}},
		{"last admin member cannot disable", "UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2", []any{f.tid, f.uid}},
		{"cross-tenant operation", "INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,'start','failed','sandbox','{}','x','x')", []any{uuid.NewString(), other.S("tenantId"), other.S("userId"), pid, wid}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if _, e := f.store.Pool.Exec(check.q, check.args...); e == nil {
				t.Fatal("constraint accepted invalid aggregate")
			}
		})
	}
	f.drain()
	if _, e = f.store.Pool.Exec("INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,2,'allocating')", uuid.NewString(), wid); e == nil {
		t.Fatal("multiple live sandboxes accepted")
	}
	// Direct SQL is used here only to verify constraints, never by simulated Controller.
	f.user.Subject = "bob"
	bob := f.call("GET", "/api/v1/me", nil, "", 200)
	f.user.Subject = "alice"
	f.call("PUT", f.path("/members/"+bob.S("id")), core.Object{"role": "member", "status": "active", "version": 0}, "", 200)
	ref, e := f.store.ConfigureCredential(context.Background(), f.tid, bob.S("id"), "secret://bob/git")
	must(t, e)
	f.call("POST", f.path("/projects"), core.Object{"name": "wrong credential", "repositoryUrl": "https://example.invalid/repo.git", "credentialRefId": ref.S("id")}, "credential-owner", 404)
}

func TestListPaginationAndErrorShape(t *testing.T) {
	f := setup(t)
	for i := 0; i < 3; i++ {
		f.create(fmt.Sprintf("p%d", i))
	}
	first := f.call("GET", f.path("/projects?limit=2"), nil, "", 200)
	items := first["items"].([]any)
	if len(items) != 2 || first.S("nextCursor") == "" {
		t.Fatal(first)
	}
	next := f.call("GET", f.path("/projects?limit=2&after="+first.S("nextCursor")), nil, "", 200)
	if len(next["items"].([]any)) != 1 || next.S("nextCursor") != "" {
		t.Fatal(next)
	}
	if core.Object(items[0].(map[string]any)).S("id") >= core.Object(items[1].(map[string]any)).S("id") {
		t.Fatal("unstable ordering")
	}
	err := f.call("GET", f.path("/projects?limit=200"), nil, "", 400)
	b, _ := json.Marshal(err)
	if err.S("code") == "" || err.S("requestId") == "" || err["params"] == nil || strings.Contains(string(b), "SQL") {
		t.Fatal("invalid error envelope", err)
	}
}
