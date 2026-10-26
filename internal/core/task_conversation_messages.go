package core

import (
	"strings"
)

// TaskConversationPageLimit bounds one task-level message read independently of Thread pagination.
const TaskConversationPageLimit = 500

const taskConversationPageDefault = 100

// taskConversationChanged queues a lossy, post-commit invalidation notice. The rows remain the
// authority; a subscriber that missed a notice can always re-read them by task-local seq.
func taskConversationChanged(t *transaction, tid, iid string) {
	space := t.one("SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL", tid)
	if space == nil {
		return
	}
	t.events = append(t.events, SpaceEvent{Type: "issue.conversation_changed", SpaceID: space.S("id"), IssueID: iid})
}

func ensureTaskConversation(t *transaction, tid, iid string) {
	t.exec(`INSERT INTO task_conversations(issue_id,tenant_id) VALUES($1,$2) ON CONFLICT(issue_id) DO NOTHING`, iid, tid)
}

func taskMessageView(t *transaction, row Object) Object {
	out := Object{
		"id": row.S("id"), "seq": row.N("seq"), "userId": row.S("userId"),
		"agentId": row.S("agentId"), "body": row.S("body"), "state": row.S("state"),
		"runId": row["runId"], "threadSeq": row["threadSeq"], "createdAt": row["createdAt"],
		"errorCode": row["errorCode"],
	}
	if row.S("state") == "submitted" && row.S("runId") != "" {
		run := t.one("SELECT status FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3", row.S("runId"), row.S("tenantId"), row.S("issueId"))
		switch run.S("status") {
		case "failed", "cancelled":
			out["delivery"] = run.S("status")
		default:
			out["delivery"] = "pending"
		}
		if row["threadSeq"] != nil {
			entry := t.one("SELECT status FROM thread_entries WHERE run_id=$1 AND seq=$2", row.S("runId"), row.N("threadSeq"))
			if entry != nil && entry.S("status") != "" {
				out["delivery"] = entry.S("status")
			}
		}
	}
	return out
}

func taskConversationRead(t *transaction, r *PublicRequest) Object {
	task := issue(t, r.TenantID, r.IssueID)
	require(r.After == "" || r.Before == "", 400, "invalid_pagination")
	limit := r.Limit
	if limit == 0 {
		limit = taskConversationPageDefault
	}
	require(limit > 0 && limit <= TaskConversationPageLimit, 400, "invalid_pagination")
	var rows []Object
	switch {
	case r.After != "":
		rows = t.list(`SELECT * FROM task_conversation_messages WHERE tenant_id=$1 AND issue_id=$2 AND seq>$3 ORDER BY seq LIMIT $4`, r.TenantID, r.IssueID, threadCursor(r.After), limit)
	case r.Before != "":
		rows = t.list(`SELECT * FROM (SELECT * FROM task_conversation_messages WHERE tenant_id=$1 AND issue_id=$2 AND seq<$3 ORDER BY seq DESC LIMIT $4) page ORDER BY seq`, r.TenantID, r.IssueID, threadCursor(r.Before), limit)
	default:
		rows = t.list(`SELECT * FROM (SELECT * FROM task_conversation_messages WHERE tenant_id=$1 AND issue_id=$2 ORDER BY seq DESC LIMIT $3) page ORDER BY seq`, r.TenantID, r.IssueID, limit)
	}
	items := make([]Object, 0, len(rows))
	for _, row := range rows {
		items = append(items, taskMessageView(t, row))
	}
	conversation := t.one("SELECT paused_run_id FROM task_conversations WHERE issue_id=$1 AND tenant_id=$2", r.IssueID, r.TenantID)
	latest := taskStoppableRun(t, r.TenantID, r.IssueID)
	barrier := taskRecoveryRun(t, r.TenantID, r.IssueID)
	uid := identityWithAlias(t, r.Identity).S("id")
	canStop := latest != nil && barrier == nil && canEndForTaskHandoff(t, latest, uid)
	out := Object{"items": items, "nextCursor": nil, "prevCursor": nil, "pausedRunId": conversation["pausedRunId"], "blockedRunId": nullable(barrier.S("id")), "activeRunId": nullable(latest.S("id")), "canStop": canStop, "agentReadiness": taskAgentReadiness(t, task, uid)}
	if len(items) != 0 {
		out["nextCursor"] = items[len(items)-1]["seq"]
		out["prevCursor"] = items[0]["seq"]
	}
	return out
}

// taskConversationSend creates one durable message. An eligible message to the same active Agent
// shares the existing Thread in this transaction; every other message waits for ordered dispatch.
func taskConversationSend(t *transaction, s *Store, r *PublicRequest, uid string) Object {
	i := issue(t, r.TenantID, r.IssueID)
	agentID, body := r.Body.S("agentId"), r.Body.S("body")
	require(validID(agentID), 400, "invalid_target")
	require(strings.TrimSpace(body) != "" && len(body) <= threadMessageTextLimit, 400, "content_too_large")
	agent, ok := agentRunEvidence(t, r.TenantID, agentID)
	require(ok, 404, "not_found")
	agentRunGate(t, i, agent)
	ensureTaskConversation(t, r.TenantID, i.S("id"))
	seq := t.one("SELECT last_seq FROM task_conversations WHERE issue_id=$1 AND tenant_id=$2", i.S("id"), r.TenantID).N("lastSeq") + 1
	id := newID()
	state := "waiting"
	var runID, threadSeq any
	queueAhead := t.one("SELECT id FROM task_conversation_messages WHERE issue_id=$1 AND state IN ('waiting','failed') LIMIT 1", i.S("id"))
	if queueAhead == nil {
		live := t.one(`SELECT * FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND executor_type='agent' AND phase IN ('starting','running') AND status IN ('dispatched','running') AND thread_state IN ('pending','active','idle') AND cancel_requested_at IS NULL AND deleted_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1`, i.S("id"), r.TenantID)
		if live != nil && live.S("executorId") == agentID && taskRunAppendAllowed(t, live, uid) && taskThreadAppendBlocker(t, live, taskAppendAtQueueTail) == "" {
			// appendThreadMessage checks the frozen initiator's model authorization again, atomically.
			input := *r
			input.RunID = live.S("id")
			input.Body = Object{"content": []any{map[string]any{"type": "text", "text": body}}}
			entry := appendThreadMessage(t, s, &input, uid, taskAppendAtQueueTail)
			state, runID, threadSeq = "submitted", live.S("id"), entry.N("seq")
		}
	}
	if state == "waiting" {
		_, gap := taskAgentModelGap(t, uid, agent, "")
		require(gap == "", 409, gap)
	}
	t.exec(`INSERT INTO task_conversation_messages(id,tenant_id,issue_id,seq,user_id,agent_id,body,state,run_id,thread_seq) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, r.TenantID, i.S("id"), seq, uid, agentID, body, state, runID, threadSeq)
	t.exec(`UPDATE task_conversations SET last_seq=$2,updated_at=now() WHERE issue_id=$1`, i.S("id"), seq)
	taskConversationChanged(t, r.TenantID, i.S("id"))
	return taskMessageView(t, t.one("SELECT * FROM task_conversation_messages WHERE id=$1", id))
}

func taskConversationCancel(t *transaction, r *PublicRequest, uid string) Object {
	issue(t, r.TenantID, r.IssueID)
	require(validID(r.ConversationMessageID), 404, "not_found")
	row := t.one("SELECT * FROM task_conversation_messages WHERE id=$1 AND tenant_id=$2 AND issue_id=$3", r.ConversationMessageID, r.TenantID, r.IssueID)
	require(row != nil, 404, "not_found")
	require(row.S("userId") == uid, 403, "forbidden")
	require(row.S("state") == "waiting" || row.S("state") == "failed", 409, "message_already_submitted")
	t.exec("UPDATE task_conversation_messages SET state='cancelled',error_code=NULL,updated_at=now() WHERE id=$1", row.S("id"))
	taskConversationChanged(t, r.TenantID, r.IssueID)
	return taskMessageView(t, t.one("SELECT * FROM task_conversation_messages WHERE id=$1", row.S("id")))
}

func taskConversationRetry(t *transaction, r *PublicRequest, uid string) Object {
	issue(t, r.TenantID, r.IssueID)
	require(validID(r.ConversationMessageID), 404, "not_found")
	row := t.one("SELECT * FROM task_conversation_messages WHERE id=$1 AND tenant_id=$2 AND issue_id=$3", r.ConversationMessageID, r.TenantID, r.IssueID)
	require(row != nil, 404, "not_found")
	require(row.S("userId") == uid, 403, "forbidden")
	require(row.S("state") == "failed", 409, "message_not_retryable")
	t.exec("UPDATE task_conversation_messages SET state='waiting',error_code=NULL,updated_at=now() WHERE id=$1", row.S("id"))
	taskConversationChanged(t, r.TenantID, r.IssueID)
	return taskMessageView(t, t.one("SELECT * FROM task_conversation_messages WHERE id=$1", row.S("id")))
}
