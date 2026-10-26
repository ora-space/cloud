package core

// New inputs follow every accepted task message. A persisted queue worker instead checks only
// messages before its own sequence, so a later handoff cannot discard its accepted prefix.
const taskAppendAtQueueTail int64 = 0

func taskThreadAppendBlocker(t *transaction, run Object, beforeMessageSeq int64) string {
	if t.one(`SELECT 1 FROM task_conversations WHERE tenant_id=$1 AND issue_id=$2 AND paused_run_id IS NOT NULL`, run.S("tenantId"), run.S("issueId")) != nil {
		return "task_paused"
	}
	if t.one(`SELECT 1 FROM task_conversation_messages WHERE tenant_id=$1 AND issue_id=$2 AND state='failed'
		AND ($3::bigint=0 OR seq<$3) LIMIT 1`, run.S("tenantId"), run.S("issueId"), beforeMessageSeq) != nil {
		return "task_paused"
	}
	if t.one(`SELECT 1 FROM task_conversation_messages WHERE tenant_id=$1 AND issue_id=$2 AND state='waiting'
		AND (agent_id::text<>$3 OR user_id::text<>$4) AND ($5::bigint=0 OR seq<$5) LIMIT 1`,
		run.S("tenantId"), run.S("issueId"), run.S("executorId"), taskRunInitiator(t, run), beforeMessageSeq) != nil {
		return "task_handoff_pending"
	}
	return ""
}
