package core

import "strings"

// Cloud Scheduled Task 的公开 API 分派（ADR D1）。写侧由 Store.Public 的写分支进入
// scheduledTaskPublic；读侧由 readPublic 进入 scheduledTaskPublicRead。管理端点的创建者边界、
// `404` 统一语义与读可见性都在这两个分派函数里收口。

// scheduledTaskPublic dispatches one management write to its store method. The methods themselves
// own the authorization (creator-only) and the eligibility table (D5).
func scheduledTaskPublic(t *transaction, s *Store, r *PublicRequest, uid string) Object {
	switch {
	case strings.HasSuffix(r.Path, "/scheduled-tasks") && r.Method == "POST":
		require(s.ScheduledTaskAcceptanceEnabled, 409, "scheduled_task_acceptance_pending")
		return scheduledTaskCreate(t, r, uid)
	case r.ScheduledTaskID != "" && strings.HasSuffix(r.Path, "/enable"):
		require(r.Method == "POST", 404, "not_found")
		require(s.ScheduledTaskAcceptanceEnabled, 409, "scheduled_task_acceptance_pending")
		return scheduledTaskEnable(t, r, uid)
	case r.ScheduledTaskID != "" && strings.HasSuffix(r.Path, "/disable"):
		require(r.Method == "POST", 404, "not_found")
		return scheduledTaskDisable(t, r, uid)
	case r.ScheduledTaskID != "" && strings.HasSuffix(r.Path, "/revoke-consent"):
		require(r.Method == "POST", 404, "not_found")
		return scheduledTaskRevokeConsent(t, r, uid)
	case r.ScheduledTaskID != "" && r.Method == "PATCH":
		require(s.ScheduledTaskAcceptanceEnabled, 409, "scheduled_task_acceptance_pending")
		return scheduledTaskUpdate(t, r, uid)
	case r.ScheduledTaskID != "" && r.Method == "DELETE":
		return scheduledTaskDelete(t, r, uid)
	default:
		require(false, 404, "not_found")
		return nil
	}
}

// scheduledTaskPublicRead dispatches one management read. List and history are member-visible
// reads; the single definition read follows the same visibility.
func scheduledTaskPublicRead(t *transaction, r *PublicRequest, uid string, acceptanceEnabled bool) Object {
	switch {
	case r.ScheduledTaskID != "" && strings.HasSuffix(r.Path, "/history"):
		return scheduledTaskHistory(t, r)
	case r.ScheduledTaskID != "":
		return scheduledTaskOut(readableScheduledTask(t, r))
	default:
		return Object{"tasks": scheduledTaskList(t, r.TenantID), "acceptanceEnabled": acceptanceEnabled}
	}
}

// scheduledTaskList returns the tenant's non-deleted definitions. Every effective tenant member
// can read them (D1: visibility follows the bound issue, and issues are member-readable), so no
// per-task creator filter applies to the read.
func scheduledTaskList(t *transaction, tenantID string) []Object {
	return t.list("SELECT * FROM scheduled_tasks WHERE tenant_id=$1 AND deleted_at IS NULL ORDER BY created_at, id", tenantID)
}
