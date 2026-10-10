package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func modelUser(t *testing.T, s *Store, subject string) (*Claims, string) {
	t.Helper()
	c := &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subject}, Source: "test-model", Caller: "gateway", DisplayName: "Model User"}
	o, _, err := s.Public(context.Background(), &PublicRequest{Method: "GET", Path: "/api/v1/me", Identity: c})
	if err != nil {
		t.Fatal(err)
	}
	return c, o.S("id")
}

func modelBody(protocol string) Object {
	return Object{"name": "Personal Provider", "protocol": protocol, "baseUrl": "https://models.example.invalid/v1", "authMode": "bearer", "models": []any{map[string]any{"id": "vendor/model-v1", "name": "Model V1", "contextWindow": int64(128000), "maxTokens": int64(8192)}}}
}

func modelRequest(t *testing.T, s *Store, c *Claims, method, path, id, key string, body Object) Object {
	t.Helper()
	o, _, err := s.Public(context.Background(), &PublicRequest{Method: method, Path: path, ModelConnectionID: id, Key: key, Body: body, Identity: c})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return o
}

func configuredModel(t *testing.T, s *Store, c *Claims) (Object, string) {
	t.Helper()
	connection := modelRequest(t, s, c, "POST", "/api/v1/me/model-connections", "", uuid.NewString(), modelBody("openai-completions")).O("resource")
	credentialID := uuid.NewString()
	ciphertext := make([]byte, 64)
	if _, err := rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	updated, err := s.PutModelCredential(context.Background(), c, connection.S("id"), connection.N("version"), "", credentialID, ciphertext, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	modelRequest(t, s, c, "PUT", "/api/v1/me/model-default", "", "", Object{"connectionId": updated.S("id"), "modelId": "vendor/model-v1", "version": int64(0)})
	return updated, credentialID
}

func expectModelFault(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || ErrorCode(err).Code != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}

func TestPersonalModelOwnershipDefaultsAndVersions(t *testing.T) {
	s := commandStore(t)
	owner, _ := modelUser(t, s, "owner")
	other, _ := modelUser(t, s, "other")
	for _, protocol := range []string{"openai-completions", "anthropic-messages"} {
		c := modelRequest(t, s, owner, "POST", "/api/v1/me/model-connections", "", protocol, modelBody(protocol)).O("resource")
		if c.B("credentialConfigured") || c["userId"] != nil || c["credentialId"] != nil {
			t.Fatalf("unsafe view: %v", c)
		}
		_, _, err := s.Public(context.Background(), &PublicRequest{Method: "GET", Path: "/api/v1/me/model-connections/" + c.S("id"), ModelConnectionID: c.S("id"), Identity: other})
		expectModelFault(t, err, "not_found")
		body := modelBody(protocol)
		body["version"] = int64(99)
		_, _, err = s.Public(context.Background(), &PublicRequest{Method: "PUT", Path: "/api/v1/me/model-connections/" + c.S("id"), ModelConnectionID: c.S("id"), Body: body, Identity: owner})
		expectModelFault(t, err, "version_conflict")
		modelRequest(t, s, owner, "PUT", "/api/v1/me/model-default", "", "", Object{"connectionId": c.S("id"), "modelId": "vendor/model-v1", "version": modelDefaultVersion(t, s, owner)})
	}
	list := modelRequest(t, s, owner, "GET", "/api/v1/me/model-connections", "", "", nil)
	if len(list["items"].([]Object)) != 2 {
		t.Fatalf("connections=%v", list)
	}
	if len(modelRequest(t, s, other, "GET", "/api/v1/me/model-connections", "", "", nil)["items"].([]Object)) != 0 {
		t.Fatal("cross-user list leak")
	}
}

func modelDefaultVersion(t *testing.T, s *Store, c *Claims) int64 {
	t.Helper()
	return modelRequest(t, s, c, "GET", "/api/v1/me/model-default", "", "", nil).N("version")
}

func TestPersonalModelCredentialIdempotencyAndDisabledUser(t *testing.T) {
	s := commandStore(t)
	owner, uid := modelUser(t, s, "owner")
	c := modelRequest(t, s, owner, "POST", "/api/v1/me/model-connections", "", "create-model", modelBody("anthropic-messages")).O("resource")
	ciphertext := make([]byte, 64)
	if _, err := rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	first, err := s.PutModelCredential(context.Background(), owner, c.S("id"), 1, "credential-write", id, ciphertext, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	retry, err := s.PutModelCredential(context.Background(), owner, c.S("id"), 1, "credential-write", id, ciphertext, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if jsonText(first) != jsonText(retry) {
		t.Fatal("credential retry changed response")
	}
	_, err = s.PutModelCredential(context.Background(), owner, c.S("id"), 1, "credential-write", uuid.NewString(), ciphertext, "test-key")
	expectModelFault(t, err, "idempotency_conflict")
	cleared, err := s.ClearModelCredential(context.Background(), owner, c.S("id"), 2, "credential-clear")
	if err != nil {
		t.Fatal(err)
	}
	if cleared.B("credentialConfigured") {
		t.Fatal("clear retained credential")
	}
	if _, err = s.Pool.Exec("UPDATE users SET status='disabled' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	_, err = s.ModelCredential(context.Background(), owner, c.S("id"))
	expectModelFault(t, err, "user_disabled")
}

func TestOpenCodeModelRunAtomicSnapshotAndThreadPermissions(t *testing.T) {
	s := commandStore(t)
	seed := seedDispatchScene(t, s.Pool, false)
	var uid string
	if err := s.Pool.QueryRow("SELECT owner_user_id FROM projects WHERE id=$1", seed.project).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	claims := &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "snapshot-owner"}, Source: "test-model", Caller: "gateway"}
	if _, err := s.Pool.Exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)", uid, claims.Source, claims.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec("UPDATE issue_runs SET status='failed' WHERE id=$1", seed.run); err != nil {
		t.Fatal(err)
	}
	input := Object{"agentPluginId": "official/ora-space.opencode", "agentPluginVersion": "0.6.4"}
	_, err := s.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("INSERT INTO issue_comments(id,tenant_id,issue_id,author_type,author_id,author_user_id,body,seq) VALUES($1,$2,$3,'user',$4,$4,'start',999)", uuid.NewString(), seed.tenant, seed.issue, uid)
		return enqueueRun(tx, seed.tenant, seed.issue, "agent", seed.agent, input, "", nil, "user", uid)
	})
	expectModelFault(t, err, "model_default_required")
	var count int
	if err = s.Pool.QueryRow("SELECT count(*) FROM issue_comments WHERE issue_id=$1 AND seq=999", seed.issue).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("missing model retained the comment")
	}
	c, credentialID := configuredModel(t, s, claims)
	run, err := s.transact(context.Background(), func(tx *transaction) Object {
		return enqueueRun(tx, seed.tenant, seed.issue, "agent", seed.agent, input, "", nil, "user", uid)
	})
	if err != nil {
		t.Fatal(err)
	}
	var binding Object
	_, err = s.transact(context.Background(), func(tx *transaction) Object {
		binding = tx.one("SELECT * FROM run_model_bindings WHERE run_id=$1", run.S("id"))
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.S("credentialId") != credentialID || binding.N("connectionVersion") != 2 || binding.O("model").S("id") != "vendor/model-v1" {
		t.Fatalf("snapshot=%v", binding)
	}
	update := modelBody("anthropic-messages")
	update["name"] = "Changed"
	update["version"] = c.N("version")
	modelRequest(t, s, claims, "PUT", "/api/v1/me/model-connections/"+c.S("id"), c.S("id"), "", update)
	_, err = s.transact(context.Background(), func(tx *transaction) Object {
		r := tx.one("SELECT * FROM issue_runs WHERE id=$1", run.S("id"))
		r["threadState"] = "active"
		p := threadModelPermissions(tx, r, uid)
		if !p.B("canAppend") || p.O("model").S("connectionName") != "Personal Provider" {
			t.Fatalf("owner perms=%v", p)
		}
		if threadModelPermissions(tx, r, uuid.NewString()).B("canAppend") {
			t.Fatal("non-owner may append")
		}
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Authorization must be enforced on the write endpoint as well as in its read capability.
	member, memberID := modelUser(t, s, "thread-member")
	admin, adminID := modelUser(t, s, "thread-admin")
	for _, pair := range []struct{ id, role string }{{memberID, "member"}, {adminID, "admin"}} {
		if _, err = s.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,$3,'active')", seed.tenant, pair.id, pair.role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Pool.Exec("UPDATE issue_runs SET phase='starting',status='dispatched',thread_state='pending' WHERE id=$1", run.S("id")); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []*Claims{member, admin} {
		_, _, err = s.Public(context.Background(), &PublicRequest{Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/runs/" + run.S("id") + "/thread/messages", TenantID: seed.tenant, IssueID: seed.issue, RunID: run.S("id"), Key: uuid.NewString(), Identity: caller, Body: Object{"content": []any{map[string]any{"type": "text", "text": "try a turn"}}}})
		expectModelFault(t, err, "model_run_initiator_required")
	}
	endRequest := &PublicRequest{Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/runs/" + run.S("id") + "/thread/end", TenantID: seed.tenant, IssueID: seed.issue, RunID: run.S("id"), Key: uuid.NewString(), Identity: member, Body: Object{}}
	_, _, err = s.Public(context.Background(), endRequest)
	expectModelFault(t, err, "model_run_end_forbidden")
	endRequest.Identity = admin
	endRequest.Key = uuid.NewString()
	_, status, err := s.Public(context.Background(), endRequest)
	if err != nil || status != 202 {
		t.Fatalf("admin end status=%d error=%v", status, err)
	}
}

func TestModelGrantsScopeRenewalFreezeAndRevocation(t *testing.T) {
	s := commandStore(t)
	scene := seedTakeoverScene(t, s)
	if _, err := s.Pool.Exec("UPDATE node_instances SET model_proxy=true WHERE node_id=$1", scene.node); err != nil {
		t.Fatal(err)
	}
	// A grant follows the same acknowledged runtime binding that production RuntimePermit
	// requires. Drive its real acknowledgement path rather than inventing a model-only fence.
	_, err := s.transact(context.Background(), func(tx *transaction) Object {
		c := runtimeControl(tx, scene.ws)
		n := currentNode(tx, scene.ws)
		return runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{"workspaceId": scene.ws, "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "nodeInstanceId": n.S("id"), "inputClosed": false, "unfinishedExecutionIds": []any{}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var uid string
	if err := s.Pool.QueryRow("SELECT owner_user_id FROM projects WHERE id=$1", scene.seed.seed.project).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	claims := &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "grant-owner"}, Source: "test-model", Caller: "gateway"}
	if _, err := s.Pool.Exec("INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)", uid, claims.Source, claims.Subject); err != nil {
		t.Fatal(err)
	}
	c, credentialID := configuredModel(t, s, claims)
	var bindingID string
	_, err = s.transact(context.Background(), func(tx *transaction) Object {
		r := tx.one("SELECT * FROM issue_runs WHERE id=$1", scene.run)
		input := r.O("input")
		input["agentPluginId"] = "official/ora-space.opencode"
		bindRunModel(tx, scene.run, uid, input)
		bindingID = input.S("modelBindingId")
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := ModelRuntimeScope{TenantID: scene.seed.seed.tenant, WorkspaceID: scene.ws, Generation: 1}
	digest := sha256.Sum256([]byte(uuid.NewString()))
	encoded := hex.EncodeToString(digest[:])
	g, err := s.CreateModelGrant(context.Background(), scope, bindingID, scene.execution, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if g.CredentialID != credentialID || g.Model.ID != "vendor/model-v1" || g.WorkspaceID != scene.ws {
		t.Fatalf("grant=%+v", g)
	}
	wrong := scope
	wrong.Generation = 2
	_, err = s.CreateModelGrant(context.Background(), wrong, bindingID, scene.execution, encoded)
	expectModelFault(t, err, "model_grant_revoked")
	renewed, err := s.RenewModelGrant(context.Background(), scope, g.ID, scene.execution)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.ID != g.ID || renewed.ExpiresAt.Before(g.ExpiresAt) {
		t.Fatal("renewal changed identity or shortened expiration")
	}
	resolved, err := s.ResolveModelGrant(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.CredentialID != g.CredentialID {
		t.Fatal("resolve changed frozen credential")
	}
	// The production schema forbids disabling the last active administrator. Keep a separate
	// active administrator so these tests exercise grant revocation without violating that guard.
	_, backupAdmin := modelUser(t, s, "grant-backup-admin")
	if _, err = s.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", scope.TenantID, backupAdmin); err != nil {
		t.Fatal(err)
	}
	for _, ending := range []struct{ name, query string }{
		{"thread ending", "UPDATE issue_runs SET thread_state='ending' WHERE id=$1"},
		{"cancellation pending", "UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1"},
	} {
		t.Run(ending.name, func(t *testing.T) {
			if _, err := s.Pool.Exec(ending.query, scene.run); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(uuid.NewString()))
			_, err := s.CreateModelGrant(context.Background(), scope, bindingID, scene.execution, hex.EncodeToString(hash[:]))
			expectModelFault(t, err, "model_session_ending")
			if ErrorCode(err).Status != 403 {
				t.Fatalf("ending denial status=%d", ErrorCode(err).Status)
			}
			_, err = s.RenewModelGrant(context.Background(), scope, g.ID, scene.execution)
			expectModelFault(t, err, "model_session_ending")
			_, err = s.ResolveModelGrant(context.Background(), encoded)
			expectModelFault(t, err, "model_session_ending")
			_, err = s.CreateModelGrant(context.Background(), wrong, bindingID, scene.execution, hex.EncodeToString(hash[:]))
			expectModelFault(t, err, "model_grant_revoked")
			_, err = s.RenewModelGrant(context.Background(), wrong, g.ID, scene.execution)
			expectModelFault(t, err, "model_grant_revoked")
			// A revoked authority never becomes a graceful-end hint just because ending is pending.
			for _, authority := range []struct{ query, restore, arg string }{
				{"UPDATE personal_model_connections SET enabled=false WHERE id=$1", "UPDATE personal_model_connections SET enabled=true WHERE id=$1", c.S("id")},
				{"UPDATE users SET status='disabled' WHERE id=$1", "UPDATE users SET status='active' WHERE id=$1", uid},
			} {
				if _, err = s.Pool.Exec(authority.query, authority.arg); err != nil {
					t.Fatal(err)
				}
				_, err = s.CreateModelGrant(context.Background(), scope, bindingID, scene.execution, hex.EncodeToString(hash[:]))
				expectModelFault(t, err, "model_grant_revoked")
				_, err = s.RenewModelGrant(context.Background(), scope, g.ID, scene.execution)
				expectModelFault(t, err, "model_grant_revoked")
				if _, err = s.Pool.Exec(authority.restore, authority.arg); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.Pool.Exec("UPDATE issue_runs SET thread_state='pending',cancel_requested_at=NULL WHERE id=$1", scene.run); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []struct{ name, deny, restore string }{
		{"account disabled", "UPDATE users SET status='disabled' WHERE id=$1", "UPDATE users SET status='active' WHERE id=$1"},
		{"membership disabled", "UPDATE tenant_memberships SET status='disabled' WHERE user_id=$1", "UPDATE tenant_memberships SET status='active' WHERE user_id=$1"},
	} {
		t.Run(state.name, func(t *testing.T) {
			if _, err := s.Pool.Exec(state.deny, uid); err != nil {
				t.Fatal(err)
			}
			_, err := s.ResolveModelGrant(context.Background(), encoded)
			expectModelFault(t, err, "model_grant_revoked")
			if _, err = s.Pool.Exec(state.restore, uid); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err = s.Pool.Exec("UPDATE node_instances SET model_proxy=false WHERE node_id=$1", scene.node); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResolveModelGrant(context.Background(), encoded)
	expectModelFault(t, err, "model_grant_revoked")
	if _, err = s.Pool.Exec("UPDATE node_instances SET model_proxy=true WHERE node_id=$1", scene.node); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClearModelCredential(context.Background(), claims, c.S("id"), c.N("version"), "grant-clear"); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResolveModelGrant(context.Background(), encoded)
	expectModelFault(t, err, "model_grant_revoked")
	_, err = s.RenewModelGrant(context.Background(), scope, g.ID, scene.execution)
	expectModelFault(t, err, "model_grant_revoked")
	if err = s.RevokeModelGrant(context.Background(), scope, g.ID, scene.execution); err != nil {
		t.Fatal(err)
	}
	// Rekeying enables future runs without resurrecting the cleared binding or its old secret.
	ciphertext := make([]byte, 64)
	if _, err = rand.Read(ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutModelCredential(context.Background(), claims, c.S("id"), 3, "", uuid.NewString(), ciphertext, "test-key"); err != nil {
		t.Fatal(err)
	}
	newDigest := sha256.Sum256([]byte(uuid.NewString()))
	_, err = s.CreateModelGrant(context.Background(), scope, bindingID, scene.execution, hex.EncodeToString(newDigest[:]))
	expectModelFault(t, err, "model_grant_revoked")
}

func TestPersonalModelConcurrentMetadataVersions(t *testing.T) {
	s := commandStore(t)
	owner, _ := modelUser(t, s, "concurrent-owner")
	c := modelRequest(t, s, owner, "POST", "/api/v1/me/model-connections", "", "concurrent-create", modelBody("openai-completions")).O("resource")
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, name := range []string{"First", "Second"} {
		workers.Add(1)
		go func(name string) {
			defer workers.Done()
			<-start
			body := modelBody("openai-completions")
			body["name"] = name
			body["version"] = int64(1)
			_, _, err := s.Public(context.Background(), &PublicRequest{Method: "PUT", Path: "/api/v1/me/model-connections/" + c.S("id"), ModelConnectionID: c.S("id"), Identity: owner, Body: body})
			results <- err
		}(name)
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if ErrorCode(err).Code == "version_conflict" {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("success=%d conflicts=%d", succeeded, conflicted)
	}
	final := modelRequest(t, s, owner, "GET", "/api/v1/me/model-connections/"+c.S("id"), c.S("id"), "", nil)
	if final.N("version") != 2 || final.S("name") != "First" && final.S("name") != "Second" {
		t.Fatalf("final metadata=%v", final)
	}
}
