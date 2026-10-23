package core

import (
	"fmt"
	"time"
)

// Cloud Scheduled Task 的公开 API 半部（ADR D1：管理仅限创建者，读取跟随绑定 issue 的可见性，
// 不存在与无权限统一 `404 not_found`）。写侧经 Store.Public 的写分支（Idempotency-Key 与
// 幂等记录由公共路径统一处理），读侧经 readPublic。触发与接纳是 worker 的事，这些端点只管
// 定义的生命周期：create / list / get / update / delete / enable / disable / revoke-consent /
// history。

// managedScheduledTask loads the task for a MANAGEMENT operation: missing, foreign and deleted
// all answer `404 not_found`, and so does a caller who is not the creator — the unattended
// consent is a personal authorization, so management never opens to other members (D1).
func managedScheduledTask(t *transaction, r *PublicRequest, uid string) Object {
	require(validID(r.ScheduledTaskID), 404, "not_found")
	task := t.one("SELECT * FROM scheduled_tasks WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", r.ScheduledTaskID, r.TenantID)
	require(task != nil, 404, "not_found")
	require(task.S("creatorUserId") == uid, 404, "not_found")
	return task
}

// readableScheduledTask loads the task for a READ operation: every effective tenant member who can
// read the bound issue may read the task and its history (D1 visibility follows the issue).
func readableScheduledTask(t *transaction, r *PublicRequest) Object {
	require(validID(r.ScheduledTaskID), 404, "not_found")
	task := t.one("SELECT * FROM scheduled_tasks WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", r.ScheduledTaskID, r.TenantID)
	require(task != nil, 404, "not_found")
	return task
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scheduledTaskOut is the REST projection of one definition. The frozen prompt, the schedule shape
// and the consent fact are the definition's own fields; occurrences and run outcomes live in
// history, not here.
func scheduledTaskOut(task Object) Object {
	return Object{
		"id":                    task.S("id"),
		"issueId":               task.S("issueId"),
		"creatorUserId":         task.S("creatorUserId"),
		"name":                  task.S("name"),
		"prompt":                task.S("prompt"),
		"agentId":               task.S("agentId"),
		"schedule":              task["schedule"],
		"timezone":              task.S("timezone"),
		"revision":              task["revision"],
		"version":               task.N("version"),
		"enabled":               task.B("enabled"),
		"unattendedPolicy":      task.S("unattendedPolicy"),
		"unattendedConsentedAt": task["unattendedConsentedAt"],
		"disabledReason":        nilIfEmpty(task.S("disabledReason")),
		"createdAt":             task["createdAt"],
		"updatedAt":             task["updatedAt"],
	}
}

// scheduledTaskCreate persists one definition (POST /scheduled-tasks). The creator is the caller;
// the binding, the execution ownership and the consent are fixed here. An enabled creation runs
// the same save-time eligibility table an admission would, so a task that could never run is never
// saved (D5).
func scheduledTaskCreate(t *transaction, r *PublicRequest, uid string) Object {
	issueID := r.Body.S("issueId")
	agentID := r.Body.S("agentId")
	name := r.Body.S("name")
	prompt := r.Body.S("prompt")
	timezone := r.Body.S("timezone")
	enabled := r.Body.B("enabled")
	require(validID(issueID) && validID(agentID) && name != "" && prompt != "" && timezone != "", 400, "invalid_request")

	approval := r.Body.O("approval")
	require(approval.S("policy") == unattendedPolicyCloud, 400, "invalid_request")
	// The consent instant is the server's, not the client's claim.
	consentedAt := dbNow(t)

	schedule, err := parseScheduledTaskSchedule(r.Body["schedule"])
	require(err == nil, 400, "invalid_request")
	scheduleDoc, err := scheduleJSON(schedule)
	require(err == nil, 400, "invalid_request")
	_, tzErr := time.LoadLocation(timezone)
	require(tzErr == nil, 400, "invalid_request")

	issue := scheduledTaskIssue(t, r.TenantID, issueID)
	require(issue != nil, 404, "not_found")
	agent, ready := scheduledTaskAgentReady(t, r.TenantID, agentID)
	require(ready, 409, "target_unavailable")
	agentRunGate(t, issue, agent)
	require(creatorModelReady(t, uid, agent.S("pluginId")), 409, "model_unavailable")

	// The unattended-consent risk statement (D5) is the creation form's own copy; the server
	// freezes the policy name and the server's consent instant.
	if enabled {
		// An enabled creation doubles as the admission-style eligibility check: everything the
		// worker would check at the first due point that can be checked without a due point.
		require(creatorMembershipActive(t, "create", r.TenantID, uid), 409, "target_unavailable")
	}

	id := newID()
	enabledAt := nilTime()
	if enabled {
		enabledAt = consentedAt
	}
	t.exec(`INSERT INTO scheduled_tasks
		(id, tenant_id, issue_id, creator_user_id, agent_id, name, prompt, schedule, timezone,
		 revision, version, enabled, enabled_at, disabled_reason,
		 unattended_policy, unattended_consented_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,1,$10,$11,NULL,$12,$13)`,
		id, r.TenantID, issueID, uid, agentID, name, prompt, jsonText(scheduleDoc), timezone,
		enabled, enabledAt, unattendedPolicyCloud, consentedAt)
	task := t.one("SELECT * FROM scheduled_tasks WHERE id=$1", id)
	return scheduledTaskOut(task)
}

// nilTime is the NULL timestamptz value for optional instants.
func nilTime() any { return nil }

// scheduledTaskUpdate replaces the editable fields (PATCH /scheduled-tasks/:stid). The binding and
// the execution ownership are immutable (D1); a change to prompt, agent, schedule or timezone bumps
// the revision, a pure display rename does not; the optimistic version CAS keeps concurrent edits
// honest. Consent is untouched — revoking is its own endpoint (D5).
func scheduledTaskUpdate(t *transaction, r *PublicRequest, uid string) Object {
	task := managedScheduledTask(t, r, uid)
	body := r.Body
	name := task.S("name")
	if body["name"] != nil {
		name = body.S("name")
		require(name != "", 400, "invalid_request")
	}
	prompt := task.S("prompt")
	if body["prompt"] != nil {
		prompt = body.S("prompt")
		require(prompt != "", 400, "invalid_request")
	}
	agentID := task.S("agentId")
	if body["agentId"] != nil {
		agentID = body.S("agentId")
		require(validID(agentID), 400, "invalid_request")
	}
	timezone := task.S("timezone")
	if body["timezone"] != nil {
		timezone = body.S("timezone")
		_, tzErr := time.LoadLocation(timezone)
		require(tzErr == nil, 400, "invalid_request")
	}
	scheduleDoc := task["schedule"]
	if body["schedule"] != nil {
		schedule, err := parseScheduledTaskSchedule(body["schedule"])
		require(err == nil, 400, "invalid_request")
		doc, err := scheduleJSON(schedule)
		require(err == nil, 400, "invalid_request")
		scheduleDoc = doc
	}
	revision := task.N("revision")
	contentChanged := prompt != task.S("prompt") || agentID != task.S("agentId") ||
		timezone != task.S("timezone") || scheduleDocJSONChanged(scheduleDoc, task["schedule"])
	if contentChanged {
		revision = task.N("revision") + 1
	}

	// Save-time eligibility follows the fields that changed (D5): an agent re-target re-checks the
	// agent and the project gate; a model change re-checks the creator's configuration.
	agent, ready := scheduledTaskAgentReady(t, r.TenantID, agentID)
	require(ready, 409, "target_unavailable")
	if agentID != task.S("agentId") {
		issue := scheduledTaskIssue(t, r.TenantID, task.S("issueId"))
		require(issue != nil, 409, "target_unavailable")
		agentRunGate(t, issue, agent)
	}
	require(creatorModelReady(t, uid, agent.S("pluginId")), 409, "model_unavailable")

	updated := t.execRows(`UPDATE scheduled_tasks
		SET name=$1, prompt=$2, agent_id=$3, schedule=$4, timezone=$5,
		    revision=$6, version=version+1, updated_at=now()
		WHERE id=$7 AND tenant_id=$8 AND creator_user_id=$9 AND deleted_at IS NULL
		  AND version=$10`, name, prompt, agentID, jsonText(scheduleDoc), timezone,
		revision, task.S("id"), r.TenantID, uid, body.N("version"))
	require(updated == 1, 409, "version_conflict")
	return scheduledTaskOut(t.one("SELECT * FROM scheduled_tasks WHERE id=$1", task.S("id")))
}

// scheduleDocJSONChanged compares the stored schedule document with the candidate after
// canonicalizing both through the parser, so key order and absent optional fields never read as an
// edit.
func scheduleDocJSONChanged(candidate, stored any) bool {
	parsedCandidate, errC := parseScheduledTaskSchedule(candidate)
	parsedStored, errS := parseScheduledTaskSchedule(stored)
	if errC != nil || errS != nil {
		return true
	}
	cDoc, errC := scheduleJSON(parsedCandidate)
	sDoc, errS := scheduleJSON(parsedStored)
	if errC != nil || errS != nil {
		return true
	}
	return jsonChanged(cDoc, sDoc)
}

// jsonChanged is a shallow-but-total comparison for the flat schedule documents.
func jsonChanged(left, right Object) bool {
	if len(left) != len(right) {
		return true
	}
	for k, v := range left {
		other, ok := right[k]
		if !ok || fmt.Sprint(v) != fmt.Sprint(other) {
			return true
		}
	}
	return false
}

// scheduledTaskEnable turns the task on (POST .../enable). The ADR's enable-time checks run here —
// the same table an admission would run, minus the parts that need a due point — and a failure
// answers with its mapped reason so the form can explain what is missing (D5).
func scheduledTaskEnable(t *transaction, r *PublicRequest, uid string) Object {
	task := managedScheduledTask(t, r, uid)
	require(task.S("unattendedPolicy") == unattendedPolicyCloud, 409, "approval_policy_invalid")
	creator := task.S("creatorUserId")
	require(creatorMembershipActive(t, task.S("id"), r.TenantID, creator), 409, "target_unavailable")
	issue := scheduledTaskIssue(t, r.TenantID, task.S("issueId"))
	require(issue != nil, 409, "target_unavailable")
	agent, ready := scheduledTaskAgentReady(t, r.TenantID, task.S("agentId"))
	require(ready, 409, "target_unavailable")
	agentRunGate(t, issue, agent)
	require(creatorModelReady(t, creator, agent.S("pluginId")), 409, "model_unavailable")

	t.exec(`UPDATE scheduled_tasks
		SET enabled=true, enabled_at=now(), disabled_reason=NULL, updated_at=now()
		WHERE id=$1 AND enabled=false`, task.S("id"))
	return scheduledTaskOut(t.one("SELECT * FROM scheduled_tasks WHERE id=$1", task.S("id")))
}

// scheduledTaskDisable turns the task off (POST .../disable). Only future admissions stop; the
// task's history and any live run are untouched (D7/D1).
func scheduledTaskDisable(t *transaction, r *PublicRequest, uid string) Object {
	task := managedScheduledTask(t, r, uid)
	t.exec(`UPDATE scheduled_tasks
		SET enabled=false, disabled_reason='manual', updated_at=now()
		WHERE id=$1 AND enabled`, task.S("id"))
	return scheduledTaskOut(t.one("SELECT * FROM scheduled_tasks WHERE id=$1", task.S("id")))
}

// scheduledTaskRevokeConsent clears the unattended consent and disables the task (D5: 撤销同意自动
// 停用). Existing run facts are never rewritten; the policy name and the consent instant leave the
// row so a re-enabled task must consent again through the create-shaped flow (the policy check in
// scheduledTaskEnable refuses a task without consent).
func scheduledTaskRevokeConsent(t *transaction, r *PublicRequest, uid string) Object {
	task := managedScheduledTask(t, r, uid)
	t.exec(`UPDATE scheduled_tasks
		SET unattended_policy=NULL, unattended_consented_at=NULL,
		    enabled=false, disabled_reason='consent_revoked', updated_at=now()
		WHERE id=$1`, task.S("id"))
	return scheduledTaskOut(t.one("SELECT * FROM scheduled_tasks WHERE id=$1", task.S("id")))
}

// scheduledTaskDelete soft-deletes the definition (D7: 墓碑保留，既有运行关联不变). History stays
// queryable through the issue it ran on; the occurrence rows keep their audit meaning.
func scheduledTaskDelete(t *transaction, r *PublicRequest, uid string) Object {
	task := managedScheduledTask(t, r, uid)
	t.exec(`UPDATE scheduled_tasks
		SET deleted_at=now(), enabled=false, updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL`, task.S("id"))
	return Object{"taskId": task.S("id")}
}

// scheduledTaskHistory reads the newest-first occurrence records with their projection (D6):
// `failed · timeout` requires the deadline fact AND a `cancelled` IssueRun terminal state, anything
// else projects the run's own terminal state. Admitted-but-unfinished runs show as running.
func scheduledTaskHistory(t *transaction, r *PublicRequest) Object {
	task := readableScheduledTask(t, r)
	entries := t.list(`
		SELECT o.scheduled_at, o.outcome, o.failure_reason, o.task_revision,
		       o.deadline_cancelled_at, o.observed_at,
		       r.id AS run_id, r.status AS run_status, r.failure_reason AS run_failure_reason
		FROM scheduled_task_occurrences o
		LEFT JOIN issue_runs r ON r.id = o.run_id
		WHERE o.task_id=$1
		ORDER BY o.scheduled_at DESC`, r.ScheduledTaskID)
	out := make([]Object, 0, len(entries))
	for _, entry := range entries {
		item := Object{
			"scheduledAt":  entry["scheduledAt"],
			"outcome":      entry["outcome"],
			"taskRevision": entry["taskRevision"],
			"observedAt":   entry["observedAt"],
		}
		switch entry.S("outcome") {
		case "missed":
			// A task-level observation (D2): no run, no failure reason beyond the miss itself.
		case "skipped_busy":
			item["outcome"] = "skipped_busy"
		case "failed":
			item["failureReason"] = entry["failureReason"]
		default:
			if entry.S("runId") == "" {
				item["outcome"] = "admitted"
				break
			}
			item["runId"] = entry.S("runId")
			status := entry.S("runStatus")
			if entry["deadlineCancelledAt"] != nil && status == "cancelled" {
				// D4: the deadline fact AND a cancelled terminal state — and only that pair —
				// is what the history is allowed to show as a timeout.
				item["outcome"] = "failed"
				item["failureReason"] = "timeout"
				break
			}
			// D6: project the run's own terminal state into the history vocabulary —
			// completed→succeeded, failed/cancelled stay as they are, any non-terminal
			// (queued/dispatched/running/deferred) shows as running.
			switch status {
			case "completed":
				item["outcome"] = "succeeded"
			case "failed", "cancelled":
				item["outcome"] = status
			default:
				item["outcome"] = "running"
			}
			if entry["runFailureReason"] != nil {
				item["failureReason"] = entry["runFailureReason"]
			}
		}
		out = append(out, item)
	}
	return Object{"entries": out, "taskId": task.S("id")}
}
