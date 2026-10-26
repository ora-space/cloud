package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// scheduledTaskState is the persisted scheduled-task contract relevant to management writes. It
// deliberately includes immutable ownership as well as every PATCH-managed field so 404/CAS
// failures prove they did not leave a partial change behind.
type scheduledTaskState struct {
	issueID, creatorUserID, agentID, name, prompt, schedule, timezone, disabledReason, unattendedPolicy string
	revision, version                                                                                   int64
	enabled, hasEnabledAt, hasUnattendedConsent                                                         bool
}

func readScheduledTaskState(t *testing.T, store *core.Store, taskID string) scheduledTaskState {
	t.Helper()
	var state scheduledTaskState
	must(t, store.Pool.QueryRow(`SELECT issue_id, creator_user_id, agent_id, name, prompt,
		schedule::text, timezone, revision, version, enabled, COALESCE(disabled_reason,''),
		COALESCE(unattended_policy,''), enabled_at IS NOT NULL, unattended_consented_at IS NOT NULL
		FROM scheduled_tasks WHERE id=$1`, taskID).Scan(
		&state.issueID, &state.creatorUserID, &state.agentID, &state.name, &state.prompt,
		&state.schedule, &state.timezone, &state.revision, &state.version, &state.enabled,
		&state.disabledReason, &state.unattendedPolicy, &state.hasEnabledAt, &state.hasUnattendedConsent,
	))
	return state
}

// scheduledTaskOtherMember seeds a second active tenant member (distinct from the task creator) with
// a verified identity, and returns the Claims the store resolves that member from.
func scheduledTaskOtherMember(t *testing.T, store *core.Store, tenantID string) *core.Claims {
	t.Helper()
	userID := uuid.NewString()
	_, err := store.Pool.Exec(`INSERT INTO users(id, display_name, status) VALUES($1, 'Other member', 'active')`, userID)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id, user_id, role, status) VALUES($1, $2, 'member', 'active')`, tenantID, userID)
	must(t, err)
	subject := "scheduled-other-" + userID[:8]
	_, err = store.Pool.Exec(`INSERT INTO user_identities(user_id,source,subject) VALUES($1,'test-scheduled',$2)`, userID, subject)
	must(t, err)
	return &core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subject}, Source: "test-scheduled", Caller: "gateway", DisplayName: "Other member"}
}

// TestScheduledTaskManagementIsCreatorOnly pins D1 创建者边界：管理写操作（disable/DELETE/PATCH）只对
// 创建者开放，任何其他有效成员都得到与不存在一致的 `404 not_found`，不暴露任务是否存在。
func TestScheduledTaskManagementIsCreatorOnly(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Creator boundary", 65)
	other := scheduledTaskOtherMember(t, store, s.tenantID)
	base := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID

	_, _, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "other-disable", Path: base + "/disable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: other,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("non-creator disable must be a 404 not_found fault, got %v", err)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "DELETE", Key: "other-delete", Path: base,
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: other,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("non-creator delete must be a 404 not_found fault, got %v", err)
	}

	beforePatch := readScheduledTaskState(t, store, taskID)
	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "other-patch", Path: base,
		TenantID: s.tenantID, ScheduledTaskID: taskID,
		Body:     core.Object{"name": "Other member edit", "version": beforePatch.version},
		Identity: other,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("non-creator PATCH must be a 404 not_found fault, got %v", err)
	}
	if afterPatch := readScheduledTaskState(t, store, taskID); afterPatch != beforePatch {
		t.Fatalf("non-creator PATCH must not modify the task: before=%+v after=%+v", beforePatch, afterPatch)
	}

	// The creator still owns it: the management write they are entitled to must keep working.
	creator := scheduledTaskCaller(t, store, s.tenantID)
	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "creator-disable", Path: base + "/disable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("creator disable must succeed with 200, got %d", status)
	}
}

// TestScheduledTaskCreationOverThePublicHTTPBoundary drives the documented create and PATCH bodies
// through the real router, service credential and user credential. It is the regression guard for
// the transport gate: the domain tests call Store.Public directly, so only this path proves that the
// ScheduledTaskSchedule and ScheduledTaskApproval objects the contract and the browser send are not
// rejected as invalid_field_type before the core sees them.
func TestScheduledTaskCreationOverThePublicHTTPBoundary(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	credentials, err := simulator.NewCredentials()
	must(t, err)
	auth, err := core.NewAuthenticator("ora-cloud", credentials.Trust)
	must(t, err)
	server := httptest.NewServer(router.NewDevelopment(store, auth, zap.NewNop()))
	t.Cleanup(server.Close)
	client := &simulator.Client{
		URL: server.URL, Credentials: credentials,
		HTTP: &http.Client{Timeout: 10 * time.Second}, Subject: "controller-a",
	}
	caller := scheduledTaskCaller(t, store, s.tenantID)
	gateway := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"

	call := func(method, target, key string, body core.Object, want int) core.Object {
		t.Helper()
		out, status, callErr := client.Call(context.Background(), method, target, "gateway", gateway, caller, key, body)
		must(t, callErr)
		if status != want {
			t.Fatalf("%s %s = %d %v, want %d", method, target, status, out, want)
		}
		return out
	}

	created := call("POST", path, "scheduled-create-over-http", core.Object{
		"issueId": s.issueID, "agentId": agentID, "name": "HTTP create", "prompt": "run the acceptance",
		"timezone": "Asia/Shanghai",
		"schedule": core.Object{"kind": "daily", "hour": int64(3), "minute": int64(5)},
		"approval": core.Object{"policy": "full_auto_in_cloud_workspace"}, "enabled": false,
	}, 200)
	taskID := created.O("resource").S("id")
	if taskID == "" {
		t.Fatalf("created task = %v", created)
	}
	if state := readScheduledTaskState(t, store, taskID); state.name != "HTTP create" || state.revision != 1 {
		t.Fatalf("persisted task = %+v", state)
	}

	// PATCH carries the same cadence object; a create-only fix would still leave the definition
	// uneditable through the public API.
	patched := call("PATCH", path+"/"+taskID, "scheduled-patch-over-http", core.Object{
		"version": int64(1), "name": "HTTP rename", "prompt": "edited",
		"schedule": core.Object{"kind": "every_hours", "intervalHours": int64(2)}, "timezone": "UTC",
	}, 200)
	if patched.O("resource").N("revision") != 2 {
		t.Fatalf("PATCH revision = %v, want 2", patched.O("resource")["revision"])
	}
}

// TestScheduledTaskMissingTaskIsNotFound pins the `404` 统一语义：不存在的任务 ID 在管理读与写上都
// 与无权限一样回答 `404 not_found`。
func TestScheduledTaskMissingTaskIsNotFound(t *testing.T) {
	store, s, _ := scheduledTaskStore(t)
	creator := scheduledTaskCaller(t, store, s.tenantID)
	missing := uuid.NewString()
	base := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + missing

	_, _, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "missing-patch", Path: base,
		TenantID: s.tenantID, ScheduledTaskID: missing,
		Body:     core.Object{"name": "Ghost", "version": int64(1)},
		Identity: creator,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("PATCH of a missing task must be a 404 not_found fault, got %v", err)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "GET", Key: "missing-get", Path: base,
		TenantID: s.tenantID, ScheduledTaskID: missing, Identity: creator,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("GET of a missing task must be a 404 not_found fault, got %v", err)
	}
}

// TestScheduledTaskVersionConflict pins the `version` 乐观锁：用过期 version 提交的编辑以
// `409 version_conflict` 拒绝，且不产生部分写入。
func TestScheduledTaskVersionConflict(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Version CAS", 65)
	creator := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID

	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "committed-version", Path: path,
		TenantID: s.tenantID, ScheduledTaskID: taskID,
		Body:     core.Object{"name": "Committed edit", "prompt": "Committed prompt", "version": int64(1)},
		Identity: creator,
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("current-version edit must succeed with 200, got %d", status)
	}
	committed := readScheduledTaskState(t, store, taskID)
	if committed.name != "Committed edit" || committed.prompt != "Committed prompt" || committed.revision != 2 || committed.version != 2 {
		t.Fatalf("successful content edit must persist and advance revision/version, got %+v", committed)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "stale-version", Path: path,
		TenantID: s.tenantID, ScheduledTaskID: taskID,
		Body:     core.Object{"name": "Stale edit", "prompt": "Stale prompt", "version": int64(1)},
		Identity: creator,
	})
	if fault := core.ErrorCode(err); fault.Status != 409 || fault.Code != "version_conflict" {
		t.Fatalf("stale version must be a 409 version_conflict fault, got %v", err)
	}
	if afterStale := readScheduledTaskState(t, store, taskID); afterStale != committed {
		t.Fatalf("a stale edit must retain every committed field and version: committed=%+v after=%+v", committed, afterStale)
	}
}

// TestScheduledTaskRevokeConsentDisables pins D5 撤销同意自动停用：revoke-consent 后任务被禁用、原因
// 为 `consent_revoked`，且同意策略与同意时刻被清空，既有运行事实不改写。
func TestScheduledTaskRevokeConsentDisables(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Revoke consent", 65)
	creator := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/revoke-consent"

	_, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "revoke-consent", Path: path,
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: creator,
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("revoke-consent must succeed with 200, got %d", status)
	}

	var enabled, consentedAtCleared bool
	var reason, policy string
	must(t, store.Pool.QueryRow(`SELECT enabled, COALESCE(disabled_reason,''), COALESCE(unattended_policy,''), unattended_consented_at IS NULL FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&enabled, &reason, &policy, &consentedAtCleared))
	if enabled || reason != "consent_revoked" || policy != "" || !consentedAtCleared {
		t.Fatalf("revoke-consent must disable the task and clear consent, got enabled=%v reason=%q policy=%q consentedAtCleared=%v", enabled, reason, policy, consentedAtCleared)
	}
}
