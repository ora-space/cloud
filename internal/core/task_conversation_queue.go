package core

import (
	"context"
)

const taskConversationBatchSize = 100

func taskMessageFail(t *transaction, message Object, reason string) {
	t.exec(`UPDATE task_conversation_messages SET state='failed',error_code=$2,updated_at=now() WHERE id=$1 AND state='waiting'`, message.S("id"), reason)
	taskConversationChanged(t, message.S("tenantId"), message.S("issueId"))
}

func taskMessageSubmit(t *transaction, message Object, runID string, threadSeq any) {
	t.exec(`UPDATE task_conversation_messages SET state='submitted',run_id=$2,thread_seq=$3,updated_at=now() WHERE id=$1 AND state='waiting'`, message.S("id"), runID, threadSeq)
	taskConversationChanged(t, message.S("tenantId"), message.S("issueId"))
}

func taskRunInitiator(t *transaction, run Object) string {
	if binding := t.one("SELECT user_id FROM run_model_bindings WHERE run_id=$1", run.S("id")); binding != nil {
		return binding.S("userId")
	}
	actor := t.one(`SELECT DISTINCT actor_id FROM issue_activities WHERE tenant_id=$1 AND issue_id=$2 AND action='run.enqueued' AND actor_type='user' AND details->>'runId'=$3`, run.S("tenantId"), run.S("issueId"), run.S("id"))
	return actor.S("actorId")
}

func taskRunAppendAllowed(t *transaction, run Object, uid string) bool {
	if taskRunInitiator(t, run) != uid {
		return false
	}
	binding := t.one("SELECT revoked_at FROM run_model_bindings WHERE run_id=$1", run.S("id"))
	return binding == nil || binding["revokedAt"] == nil
}

func canEndForTaskHandoff(t *transaction, run Object, uid string) bool {
	if taskRunInitiator(t, run) == uid {
		return true
	}
	member := t.one("SELECT role FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", run.S("tenantId"), uid)
	return member != nil && member.S("role") == "admin"
}

func taskRunBlocker(t *transaction, message Object) Object {
	return t.one(`SELECT * FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND executor_type='agent' AND deleted_at IS NULL AND (status IN ('queued','dispatched','running','deferred') OR phase IS NOT NULL AND phase<>'done') ORDER BY created_at,id LIMIT 1`, message.S("issueId"), message.S("tenantId"))
}

func taskPreviousRunNeedsAttention(t *transaction, message Object) bool {
	if t.one("SELECT 1 FROM task_conversations WHERE issue_id=$1 AND paused_run_id IS NOT NULL", message.S("issueId")) != nil {
		return true
	}
	ack := t.one(`SELECT acknowledged_run_id FROM task_conversations WHERE issue_id=$1`, message.S("issueId"))
	previous := t.one(`SELECT * FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND executor_type='agent' AND deleted_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1`, message.S("issueId"), message.S("tenantId"))
	if previous == nil {
		return false
	}
	if previous.S("phase") != "" && previous.S("phase") != "done" {
		return true
	}
	if previous.S("workspaceId") != "" && t.one("SELECT id FROM workspaces WHERE id=$1 AND deleted_at IS NULL", previous.S("workspaceId")) != nil {
		return true
	}
	acknowledged := ack != nil && ack.S("acknowledgedRunId") == previous.S("id")
	if previous.O("result").S("deliveryState") == "failed" || previous.O("result").S("deliveryState") == "skipped" {
		return !acknowledged
	}
	return (previous.S("status") == "failed" || previous.S("status") == "cancelled") && !acknowledged
}

// dispatchTaskMessage takes exactly one waiting message under the same transaction-scoped advisory
// lock as existing Run, Thread and Workspace mutations. Returning no Run means the message remains
// durable and another pass can re-evaluate the actual current execution and authorization.
func (s *Store) dispatchTaskMessage(ctx context.Context, messageID string) (string, error) {
	out, err := s.transact(ctx, func(t *transaction) Object {
		message := t.one("SELECT * FROM task_conversation_messages WHERE id=$1 AND state='waiting'", messageID)
		if message == nil || t.one("SELECT 1 FROM task_conversation_messages WHERE issue_id=$1 AND state IN ('waiting','failed') AND seq<$2 LIMIT 1", message.S("issueId"), message.N("seq")) != nil {
			return Object{}
		}
		member := t.one("SELECT 1 FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", message.S("tenantId"), message.S("userId"))
		if member == nil {
			taskMessageFail(t, message, "member_unavailable")
			return Object{}
		}
		blocker := taskRunBlocker(t, message)
		if blocker != nil {
			if taskThreadAppendBlocker(t, blocker, message.N("seq")) != "" {
				return Object{}
			}
			if blocker.S("executorId") == message.S("agentId") && threadAcceptStates[blocker.S("threadState")] && blocker.S("cancelRequestedAt") == "" {
				ref := &PublicRequest{TenantID: message.S("tenantId"), IssueID: message.S("issueId"), RunID: blocker.S("id"), Body: Object{"content": []any{map[string]any{"type": "text", "text": message.S("body")}}}}
				if taskRunAppendAllowed(t, blocker, message.S("userId")) {
					entry := appendThreadMessage(t, s, ref, message.S("userId"), message.N("seq"))
					taskMessageSubmit(t, message, blocker.S("id"), entry.N("seq"))
					return Object{}
				}
			}
			if blocker.S("threadState") == "idle" && canEndForTaskHandoff(t, blocker, message.S("userId")) {
				s.endAgentThread(t, blocker, "user_ended")
			}
			return Object{}
		}
		if taskPreviousRunNeedsAttention(t, message) {
			return Object{}
		}
		agent, ok := agentRunEvidence(t, message.S("tenantId"), message.S("agentId"))
		if !ok {
			taskMessageFail(t, message, "agent_unavailable")
			return Object{}
		}
		i := issue(t, message.S("tenantId"), message.S("issueId"))
		if i.S("projectRef") == "" || t.one("SELECT id FROM projects WHERE id=$1 AND tenant_id=$2 AND space_id=$3 AND deleted_at IS NULL", i.S("projectRef"), message.S("tenantId"), agent.S("spaceId")) == nil {
			taskMessageFail(t, message, "project_unavailable")
			return Object{}
		}
		if _, code := taskAgentModelGap(t, message.S("userId"), agent, ""); code != "" {
			taskMessageFail(t, message, code)
			return Object{}
		}
		input := snapshotAgentRunInput(buildRunContext(t, i, message.S("body"), "agent", message.S("agentId"), Object{}, nil), agent)
		run := enqueueAdmittedRun(t, message.S("tenantId"), message.S("issueId"), "agent", message.S("agentId"), input, "", nil, "user", message.S("userId"))
		taskMessageSubmit(t, message, run.S("id"), nil)
		return Object{"runId": run.S("id")}
	})
	if err != nil {
		return "", err
	}
	return out.S("runId"), nil
}

const taskConversationScan = `SELECT m.id,m.created_at FROM task_conversation_messages m
	WHERE m.state='waiting' AND NOT EXISTS (
	  SELECT 1 FROM task_conversation_messages earlier
	  WHERE earlier.issue_id=m.issue_id AND earlier.state IN ('waiting','failed') AND earlier.seq<m.seq
	)`

func (s *Store) nextTaskMessages(ctx context.Context) ([]string, error) {
	var ids []string
	_, err := s.transact(ctx, func(t *transaction) Object {
		cursor := t.one("SELECT last_created_at,last_id FROM task_conversation_scan_state WHERE singleton=true")
		var rows []Object
		if cursor["lastCreatedAt"] != nil {
			rows = t.list(taskConversationScan+` AND (m.created_at,m.id)>($2::timestamptz,$3::uuid) ORDER BY m.created_at,m.id LIMIT $1`, taskConversationBatchSize, cursor.S("lastCreatedAt"), cursor.S("lastId"))
		}
		if len(rows) == 0 {
			rows = t.list(taskConversationScan+` ORDER BY m.created_at,m.id LIMIT $1`, taskConversationBatchSize)
		}
		for _, row := range rows {
			ids = append(ids, row.S("id"))
		}
		if len(rows) != 0 {
			last := rows[len(rows)-1]
			t.exec("UPDATE task_conversation_scan_state SET last_created_at=$1,last_id=$2 WHERE singleton=true", last["createdAt"], last.S("id"))
		}
		return Object{}
	})
	return ids, err
}

// DispatchTaskConversationsOnce processes the oldest pending message per task. The committed scan
// cursor eventually moves past paused tasks, and a wrap revisits them when their condition changes.
// A failed individual dispatch leaves its row intact; no external work runs under the DB lock.
func (s *Store) DispatchTaskConversationsOnce(ctx context.Context) error {
	ids, err := s.nextTaskMessages(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		runID, err := s.dispatchTaskMessage(ctx, id)
		if err != nil {
			continue
		}
		if runID != "" {
			_ = s.agentRunDispatcher().Dispatch(ctx, runID)
		}
	}
	return nil
}

func taskConversationResume(t *transaction, r *PublicRequest, uid string) Object {
	issue(t, r.TenantID, r.IssueID)
	runID := r.Body.S("runId")
	require(validID(runID), 400, "invalid_run")
	prior := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND executor_type='agent' AND deleted_at IS NULL", runID, r.TenantID, r.IssueID)
	require(prior != nil, 404, "not_found")
	paused := t.one("SELECT 1 FROM task_conversations WHERE issue_id=$1 AND paused_run_id=$2", r.IssueID, runID) != nil
	require(taskRecoveryRun(t, r.TenantID, r.IssueID).S("id") == runID, 409, "stale_run")
	require(prior.S("phase") == "done" && (prior.S("status") == "failed" || prior.S("status") == "cancelled" || paused), 409, "run_not_resumable")
	require(prior.O("result").S("deliveryState") == "saved" || prior.O("result").S("deliveryState") == "unchanged", 409, "run_not_resumable")
	require(prior.S("workspaceId") == "" || t.one("SELECT id FROM workspaces WHERE id=$1 AND deleted_at IS NULL", prior.S("workspaceId")) == nil, 409, "workspace_unreleased")
	require(canEndForTaskHandoff(t, prior, uid), 403, "model_run_end_forbidden")
	ensureTaskConversation(t, r.TenantID, r.IssueID)
	t.exec("UPDATE task_conversations SET acknowledged_run_id=$2,paused_run_id=NULL,updated_at=now() WHERE issue_id=$1", r.IssueID, runID)
	taskConversationChanged(t, r.TenantID, r.IssueID)
	return Object{"acknowledgedRunId": runID}
}

// taskConversationDiscard requires a separate, explicit acknowledgement before using the latest
// previously saved Revision after a run produced no deliverable result. It never claims that the
// failed run's workspace content was saved or rewrites its terminal outcome.
func taskConversationDiscard(t *transaction, r *PublicRequest, uid string) Object {
	issue(t, r.TenantID, r.IssueID)
	require(r.Body.B("acknowledgeLostWork"), 400, "lost_work_acknowledgement_required")
	runID := r.Body.S("runId")
	require(validID(runID), 400, "invalid_run")
	prior := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND executor_type='agent' AND deleted_at IS NULL", runID, r.TenantID, r.IssueID)
	require(prior != nil, 404, "not_found")
	require(taskRecoveryRun(t, r.TenantID, r.IssueID).S("id") == runID, 409, "stale_run")
	state := prior.O("result").S("deliveryState")
	// Preparation can fail before a session or delivery result exists. A missing result is
	// still undelivered work: only the explicit acknowledgement above may release its barrier.
	missingFailedResult := state == "" && (prior.S("status") == "failed" || prior.S("status") == "cancelled")
	require(prior.S("phase") == "done" && (state == "failed" || state == "skipped" || missingFailedResult), 409, "run_not_resumable")
	require(prior.S("workspaceId") == "" || t.one("SELECT id FROM workspaces WHERE id=$1 AND deleted_at IS NULL", prior.S("workspaceId")) == nil, 409, "workspace_unreleased")
	require(canEndForTaskHandoff(t, prior, uid), 403, "model_run_end_forbidden")
	ensureTaskConversation(t, r.TenantID, r.IssueID)
	t.exec("UPDATE task_conversations SET acknowledged_run_id=$2,paused_run_id=NULL,updated_at=now() WHERE issue_id=$1", r.IssueID, runID)
	taskConversationChanged(t, r.TenantID, r.IssueID)
	return Object{"acknowledgedRunId": runID}
}
