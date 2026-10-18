package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

type historyScene struct {
	store       *Store
	request     PublicRequest
	user        string
	revision    string
	key         string
	data        []byte
	digest      string
	storageRead atomic.Int64
}

func historyExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.Pool.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("stage isolated archive read fixture: %v", err)
	}
}

// The delivery rows are staged preconditions, not evidence of Node/S3 delivery. These tests prove
// the public archive reader against real PostgreSQL and HTTP, using only synthetic JSONL records.
func seedHistoryScene(t *testing.T) *historyScene {
	t.Helper()
	store := dispatcherDB(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	user, work, revision, execution := newID(), newID(), newID(), "history-delivery"
	data := []byte("{\"seq\":0,\"at\":\"2026-10-15T10:00:00Z\",\"type\":\"meta\",\"schemaVersion\":2,\"synthetic\":true}\n{\"seq\":1,\"type\":\"update\",\"number\":9007199254740993}\n")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key := "revisions/" + scene.seed.tenant + "/" + scene.seed.run + "/" + work + "/session.jsonl"
	historyExec(t, store, `INSERT INTO users(id,display_name,status) VALUES($1,'history member','active')`, user)
	historyExec(t, store, `INSERT INTO user_identities(user_id,source,subject) VALUES($1,'dev','history-member')`, user)
	historyExec(t, store, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')`, scene.seed.tenant, user)
	historyExec(t, store, `UPDATE issue_runs SET status='completed',phase='done',thread_state='ended' WHERE id=$1`, scene.seed.run)
	historyExec(t, store, `INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) VALUES($1,$2,'deliver_revision','{}','{}',$3)`, work, scene.seed.run, execution)
	historyExec(t, store, `INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,result,dispatched_epoch)
		VALUES($1,'deliver_revision',$2,$3,$4,'synthetic-node','synthetic-op','{}','{"outcome":"revision_unchanged"}',1)`, execution, scene.seed.run, work, scene.ws)
	historyExec(t, store, `INSERT INTO revision_verifications(execution_id,outcome) VALUES($1,'unchanged')`, execution)
	historyExec(t, store, `INSERT INTO revisions(id,execution_id,tenant_id,run_id,workspace_id,project_id,repository_url,base_commit,final_commit,revision_ref,history_key,history_size,history_sha256)
		VALUES($1,$2,$3,$4,$5,$6,'https://example.invalid/synthetic.git',$7,$7,$8,$9,$10,$11)`, revision, execution, scene.seed.tenant, scene.seed.run, scene.ws, scene.seed.project, baseCommit, "refs/ora/revisions/"+scene.seed.run, key, len(data), digest)
	return &historyScene{
		store: store, user: user, revision: revision, key: key, data: data, digest: digest,
		request: PublicRequest{
			Method: "GET", TenantID: scene.seed.tenant, IssueID: scene.seed.issue, RunID: scene.seed.run,
			Path:     "/api/v1/tenants/" + scene.seed.tenant + "/issues/" + scene.seed.issue + "/runs/" + scene.seed.run + "/history",
			Identity: &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "history-member"}, Source: "dev", DisplayName: "History member"},
		},
	}
}

func (scene *historyScene) storage(t *testing.T, beforeResponse func(), body []byte) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scene.storageRead.Add(1)
		if r.Method != "GET" || r.URL.Path != "/revisions/"+scene.key || r.URL.Query().Get("X-Amz-Signature") == "" {
			t.Error("archive request did not use the registered key and signed GET")
		}
		if beforeResponse != nil {
			beforeResponse()
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	scene.store.ObjectStore = &objectstore.Config{Endpoint: server.URL, PublicEndpoint: "http://must-not-be-used.invalid", Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: "synthetic-access", SecretAccessKey: "synthetic-secret"}
}

func TestSessionHistoryReadsRegisteredArchiveWithoutWritesOrLongTransaction(t *testing.T) {
	scene := seedHistoryScene(t)
	scene.store.Pool.SetMaxOpenConns(1)
	historyExec(t, scene.store, "SET default_transaction_read_only=on")
	scene.storage(t, func() {
		// One connection plus a real query proves the first transaction has released the connection
		// while HTTP is in flight; read-only transactions also reject any hidden identity mutation.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var value int
		if err := scene.store.Pool.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil || value != 1 {
			t.Errorf("storage I/O held a database transaction: %v", err)
		}
	}, scene.data)
	got, status, err := scene.store.SessionHistory(t.Context(), &scene.request)
	wantRecords, parseErr := sessionHistoryRecords(t.Context(), scene.data)
	want := Object{"source": "archive", "records": wantRecords, "sizeBytes": int64(len(scene.data)), "sha256": scene.digest}
	if err != nil || parseErr != nil || status != 200 || !reflect.DeepEqual(got, want) || scene.storageRead.Load() != 1 {
		t.Fatalf("archive read status=%d error=%v parse=%v reads=%d", status, err, parseErr, scene.storageRead.Load())
	}
	encoded, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(encoded), "9007199254740993") || strings.Contains(string(encoded), scene.key) || strings.Contains(string(encoded), "X-Amz") {
		t.Fatal("public payload lost numeric precision or exposed private storage details")
	}
}

func TestSessionHistoryRejectsUnauthorizedScopeAndUnavailableBeforeStorage(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *historyScene)
		status int
		code   string
	}{
		{"missing identity", func(_ *testing.T, s *historyScene) { s.request.Identity = nil }, 401, "invalid_identity"},
		{"unknown identity", func(_ *testing.T, s *historyScene) { s.request.Identity.Subject = "unregistered-nonmember" }, 403, "membership_required"},
		{"disabled member", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE tenant_memberships SET status='disabled' WHERE user_id=$1", s.user)
		}, 403, "membership_required"},
		{"disabled user", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE users SET status='disabled' WHERE id=$1", s.user)
		}, 403, "user_disabled"},
		{"wrong tenant", func(_ *testing.T, s *historyScene) { s.request.TenantID = newID() }, 403, "membership_required"},
		{"wrong issue", func(_ *testing.T, s *historyScene) { s.request.IssueID = newID() }, 404, "not_found"},
		{"wrong run", func(_ *testing.T, s *historyScene) { s.request.RunID = newID() }, 404, "not_found"},
		{"deleted issue", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE issues SET deleted_at=now() WHERE id=$1", s.request.IssueID)
		}, 404, "not_found"},
		{"deleted run", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE issue_runs SET deleted_at=now() WHERE id=$1", s.request.RunID)
		}, 404, "not_found"},
		{"not archived", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "DELETE FROM revisions WHERE id=$1", s.revision)
		}, 404, "history_unavailable"},
		{"expired archive", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE revisions SET expires_at=now()-interval '1 second' WHERE id=$1", s.revision)
		}, 404, "history_unavailable"},
		{"oversized archive", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE revisions SET history_size=$2 WHERE id=$1", s.revision, SessionHistoryByteLimit+1)
		}, 413, "history_too_large"},
		{"cross tenant key", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE revisions SET history_key=$2 WHERE id=$1", s.revision, strings.Replace(s.key, s.request.TenantID, newID(), 1))
		}, 502, "history_invalid"},
		{"cross run key", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE revisions SET history_key=$2 WHERE id=$1", s.revision, strings.Replace(s.key, s.request.RunID, newID(), 1))
		}, 502, "history_invalid"},
		{"URL as key", func(t *testing.T, s *historyScene) {
			historyExec(t, s.store, "UPDATE revisions SET history_key='https://example.invalid/private' WHERE id=$1", s.revision)
		}, 502, "history_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scene := seedHistoryScene(t)
			scene.storage(t, nil, scene.data)
			test.change(t, scene)
			var identitiesBefore int
			if err := scene.store.Pool.QueryRowContext(t.Context(), "SELECT count(*) FROM user_identities").Scan(&identitiesBefore); err != nil {
				t.Fatal(err)
			}
			got, _, err := scene.store.SessionHistory(t.Context(), &scene.request)
			historyFault(t, err, test.status, test.code)
			var identitiesAfter int
			if err := scene.store.Pool.QueryRowContext(t.Context(), "SELECT count(*) FROM user_identities").Scan(&identitiesAfter); err != nil {
				t.Fatal(err)
			}
			if got != nil || scene.storageRead.Load() != 0 || identitiesBefore != identitiesAfter {
				t.Fatal("denied read fetched an object, exposed data, or provisioned an identity")
			}
		})
	}
}

func TestSessionHistoryRechecksMembershipAndRegistrationAfterStorage(t *testing.T) {
	for _, test := range []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{"membership revoked", "UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND role='member'", 403, "membership_required"},
		{"archive replaced", "UPDATE revisions SET history_sha256=repeat('0',64) WHERE tenant_id=$1", 404, "history_unavailable"},
		{"run deleted", "UPDATE issue_runs SET deleted_at=now() WHERE tenant_id=$1", 404, "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scene := seedHistoryScene(t)
			scene.storage(t, func() {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				_, err := scene.store.transact(ctx, func(tx *transaction) Object {
					tx.exec(test.query, scene.request.TenantID)
					return nil
				})
				if err != nil {
					t.Errorf("concurrent change failed or was blocked by archive transaction: %v", err)
				}
			}, scene.data)
			got, _, err := scene.store.SessionHistory(t.Context(), &scene.request)
			historyFault(t, err, test.status, test.code)
			if got != nil || scene.storageRead.Load() != 1 {
				t.Fatal("stale authorization or registration released archive data")
			}
		})
	}
}

func TestSessionHistoryRejectsCorruptArchiveAndCancelledContext(t *testing.T) {
	scene := seedHistoryScene(t)
	corrupt := append([]byte(nil), scene.data...)
	corrupt[0] = '['
	scene.storage(t, nil, corrupt)
	_, _, err := scene.store.SessionHistory(t.Context(), &scene.request)
	historyFault(t, err, 502, "history_invalid")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = scene.store.SessionHistory(ctx, &scene.request)
	if !errors.Is(err, context.Canceled) || scene.storageRead.Load() != 1 {
		t.Fatalf("cancelled request contacted storage or lost cancellation: %v", err)
	}
}

func TestSessionReadIdentityResolvesHuaweiAliasesWithoutWriting(t *testing.T) {
	for _, source := range []string{"huawei-corp", "huawei-global"} {
		t.Run(source, func(t *testing.T) {
			scene := seedHistoryScene(t)
			subject := "verified-corporate-uuid"
			if source == "huawei-global" {
				subject = "12345"
			}
			historyExec(t, scene.store, "UPDATE user_identities SET source=$2,subject=$3 WHERE user_id=$1", scene.user, source, subject)
			scene.request.Identity.Source = "huawei-corp"
			scene.request.Identity.Subject = "verified-corporate-uuid"
			scene.request.Identity.GlobalUserID = "0012345"
			scene.store.Pool.SetMaxOpenConns(1)
			historyExec(t, scene.store, "SET default_transaction_read_only=on")
			scene.storage(t, nil, scene.data)
			_, status, err := scene.store.SessionHistory(t.Context(), &scene.request)
			if err != nil || status != 200 {
				t.Fatalf("existing Huawei identity was not resolved without binding an alias: %v", err)
			}
			var aliases int
			if err := scene.store.Pool.QueryRowContext(t.Context(), "SELECT count(*) FROM user_identities WHERE user_id=$1", scene.user).Scan(&aliases); err != nil || aliases != 1 {
				t.Fatalf("archive read changed aliases: count=%d error=%v", aliases, err)
			}
		})
	}
}

func TestSessionReadIdentityRejectsHuaweiAliasConflict(t *testing.T) {
	scene := seedHistoryScene(t)
	other := newID()
	historyExec(t, scene.store, "UPDATE user_identities SET source='huawei-corp' WHERE user_id=$1", scene.user)
	historyExec(t, scene.store, "INSERT INTO users(id,display_name,status) VALUES($1,'synthetic other identity','active')", other)
	historyExec(t, scene.store, "INSERT INTO user_identities(user_id,source,subject) VALUES($1,'huawei-global','12345')", other)
	scene.request.Identity.Source = "huawei-corp"
	scene.request.Identity.GlobalUserID = "0012345"
	scene.storage(t, nil, scene.data)
	_, _, err := scene.store.SessionHistory(t.Context(), &scene.request)
	historyFault(t, err, 409, "identity_conflict")
	if scene.storageRead.Load() != 0 {
		t.Fatal("conflicting identity fetched archive data")
	}
}
