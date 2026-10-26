package integration

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestScheduledTaskModelSelectionPersistsAndBumpsRevision pins D1 模型选择持久化：创建时携带的 model
// 原样落库并在读取时返回；修改 model 与修改 prompt/agent/schedule 一样递增 revision。
func TestScheduledTaskModelSelectionPersistsAndBumpsRevision(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"

	out, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "create-with-model", Path: path,
		TenantID: s.tenantID, Identity: caller,
		Body: core.Object{
			"issueId": s.issueID, "agentId": agentID, "name": "Model task", "prompt": "Sync",
			"model":    "vendor/model-v1",
			"schedule": core.Object{"kind": "every_hours", "intervalHours": 1},
			"timezone": "Asia/Shanghai",
			"approval": core.Object{"policy": "full_auto_in_cloud_workspace"},
			"enabled":  false,
		},
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("create with model must succeed, got %d", status)
	}
	task := out.O("resource")
	if task.S("model") != "vendor/model-v1" {
		t.Fatalf("model must round-trip through create, got %q", task.S("model"))
	}

	_, status, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "PATCH", Key: "patch-model", Path: path + "/" + task.S("id"),
		TenantID: s.tenantID, ScheduledTaskID: task.S("id"), Identity: caller,
		Body: core.Object{"model": "vendor/model-v2", "version": task.N("version")},
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("model update must succeed, got %d", status)
	}
	var model string
	var revision int64
	must(t, store.Pool.QueryRow(`SELECT COALESCE(model,''), revision FROM scheduled_tasks WHERE id=$1`, task.S("id")).Scan(&model, &revision))
	if model != "vendor/model-v2" {
		t.Fatalf("model update must persist, got %q", model)
	}
	if revision != 2 {
		t.Fatalf("model change must bump revision, got %d", revision)
	}
}

// TestScheduledTaskDisabledCreateDoesNotPersistConsent keeps an unstarted definition a draft:
// supplying a stale approval field cannot create a future unattended grant. Enabling must receive
// a fresh explicit approval, which lets the UI present re-consent at the action boundary.
func TestScheduledTaskDisabledCreateDoesNotPersistConsent(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks"

	out, status, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "create-disabled-draft", Path: path,
		TenantID: s.tenantID, Identity: caller,
		Body: core.Object{
			"issueId": s.issueID, "agentId": agentID, "name": "Disabled draft", "prompt": "Sync",
			"schedule": core.Object{"kind": "every_hours", "intervalHours": 1},
			"timezone": "Asia/Shanghai", "enabled": false,
			// A stale client may still include it; disabled creation must discard rather than persist it.
			"approval": core.Object{"policy": "full_auto_in_cloud_workspace"},
		},
	})
	must(t, err)
	if status != 200 {
		t.Fatalf("disabled draft create status = %d, want 200", status)
	}
	taskID := out.O("resource").S("id")
	var policy string
	var consentedAt any
	must(t, store.Pool.QueryRow(`SELECT COALESCE(unattended_policy,''), unattended_consented_at FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&policy, &consentedAt))
	if policy != "" || consentedAt != nil {
		t.Fatalf("disabled draft must not retain consent, got policy=%q consentedAt=%v", policy, consentedAt)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "enable-disabled-draft-without-consent", Path: path + "/" + taskID + "/enable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller, Body: core.Object{},
	})
	if fault := core.ErrorCode(err); fault.Code != "approval_policy_invalid" {
		t.Fatalf("draft enable without fresh consent must be approval_policy_invalid, got %v", err)
	}
}

// TestScheduledTaskReconsentAfterRevoke pins D5 撤销同意后重新同意：撤销清空 consent 并停用；不带新
// 同意的启用被拒，携带新同意的启用恢复 consent 与启用状态。
func TestScheduledTaskReconsentAfterRevoke(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Reconsent", 65)
	path := "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID

	_, _, err := store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "revoke", Path: path + "/revoke-consent",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller, Body: core.Object{},
	})
	must(t, err)

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "enable-no-consent", Path: path + "/enable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller, Body: core.Object{},
	})
	if fault := core.ErrorCode(err); fault.Code != "approval_policy_invalid" {
		t.Fatalf("enable without fresh consent must be approval_policy_invalid, got %v", err)
	}

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "POST", Key: "enable-reconsent", Path: path + "/enable",
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller,
		Body: core.Object{"approval": core.Object{"policy": "full_auto_in_cloud_workspace"}},
	})
	must(t, err)
	var enabled bool
	var policy string
	must(t, store.Pool.QueryRow(`SELECT enabled, COALESCE(unattended_policy,'') FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&enabled, &policy))
	if !enabled || policy != "full_auto_in_cloud_workspace" {
		t.Fatalf("re-consent enable must restore consent, got enabled=%v policy=%q", enabled, policy)
	}
}

// TestScheduledTaskReadRequiresBoundIssue pins D1 读取跟随绑定 issue 可见性：绑定 issue 已删除后，
// 任务的读取与历史读取都回答 404 not_found。
func TestScheduledTaskReadRequiresBoundIssue(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	caller := scheduledTaskCaller(t, store, s.tenantID)
	taskID := seedEnabledScheduledTask(t, store, s, agentID, "Orphan read", 65)
	_, err := store.Pool.Exec(`UPDATE issues SET deleted_at=now() WHERE id=$1`, s.issueID)
	must(t, err)

	_, _, err = store.Public(context.Background(), &core.PublicRequest{
		Method: "GET", Key: "orphan-get", Path: "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID,
		TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller,
	})
	if fault := core.ErrorCode(err); fault.Status != 404 || fault.Code != "not_found" {
		t.Fatalf("read of a task whose issue is deleted must be 404 not_found, got %v", err)
	}
}
