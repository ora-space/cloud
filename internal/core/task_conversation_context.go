package core

import "unicode/utf8"

// Earlier Agent prose arrives as an explicit Node projection; Cloud never parses ACP content or
// guesses whether a tool/thought record is user-facing text. This reader freezes only completed
// runs and never treats a prior Workspace or Git Revision as proof that chat history was restored.

const (
	taskConversationHistoryTurns = 20
	taskConversationPromptBudget = 64 << 10
)

func conversationHistory(t *transaction, input Object, tid, iid string) {
	rows := t.list(`
		WITH recent AS (
		  SELECT te.run_id,r.created_at AS run_created_at,te.seq,te.turn_id,te.visible_text,te.visible_text_truncated
		  FROM issue_runs r JOIN thread_entries te ON te.run_id=r.id
		  WHERE r.tenant_id=$1 AND r.issue_id=$2 AND r.executor_type='agent'
		    AND r.phase='done' AND r.deleted_at IS NULL AND te.visible_text IS NOT NULL
		  ORDER BY r.created_at DESC,r.id DESC,te.seq DESC LIMIT 501
		), kept AS (
		  SELECT * FROM recent ORDER BY run_created_at DESC,run_id DESC,seq DESC LIMIT 500
		)
		SELECT run_id,turn_id::text AS turn_id,string_agg(visible_text,'' ORDER BY seq) AS assistant_text,
		       bool_or(visible_text_truncated) AS truncated, max(seq) AS last_seq,run_created_at,
		       (SELECT count(*)>500 FROM recent) AS overflow
		FROM kept GROUP BY run_id,turn_id,run_created_at ORDER BY run_created_at DESC,run_id DESC,last_seq DESC LIMIT $3`,
		tid, iid, taskConversationHistoryTurns)
	// A past session with no projection is a known gap, including pre-upgrade rows. It remains
	// readable through its Thread/verified archive, but cannot be described as recovered context.
	unavailable := t.one(`SELECT 1 FROM issue_runs r WHERE r.tenant_id=$1 AND r.issue_id=$2 AND r.executor_type='agent'
		AND r.phase='done' AND r.deleted_at IS NULL AND EXISTS (SELECT 1 FROM thread_entries te WHERE te.run_id=r.id AND te.source='node')
		AND NOT EXISTS (SELECT 1 FROM thread_entries te WHERE te.run_id=r.id AND te.visible_text IS NOT NULL) LIMIT 1`, tid, iid) != nil
	chosen := make([]Object, 0, len(rows))
	truncated := len(rows) == taskConversationHistoryTurns
	for index, row := range rows {
		if row.B("overflow") {
			truncated = true
		}
		user := t.one(`SELECT m.body FROM task_conversation_messages m
			JOIN thread_entries te ON te.run_id=m.run_id AND te.seq=COALESCE(m.thread_seq,1)
			WHERE m.run_id=$1 AND m.state='submitted' AND te.turn_id::text=$2
			ORDER BY m.seq DESC LIMIT 1`, row.S("runId"), row.S("turnId"))
		turn := Object{"id": row.S("runId") + ":" + row.S("turnId"), "assistant": row.S("assistantText")}
		if user != nil {
			turn["user"] = user.S("body")
		} else {
			// Older runs predate task_conversation_messages. Their first frozen task is still
			// trustworthy for the initial turn, but later turns without a linked user message are
			// an explicit gap rather than an invitation to invent one from a tool record.
			initial := t.one("SELECT turn_id FROM thread_entries WHERE run_id=$1 ORDER BY seq LIMIT 1", row.S("runId"))
			if row.S("turnId") != "" && initial.S("turnId") == row.S("turnId") {
				prior := t.one("SELECT input->>'task' AS task FROM issue_runs WHERE id=$1", row.S("runId"))
				if prior.S("task") != "" {
					turn["user"] = prior.S("task")
				} else {
					truncated = true
				}
			} else {
				truncated = true
			}
		}
		if row.B("truncated") || row.B("overflow") && index == len(rows)-1 {
			turn["assistantTruncated"] = true
		}
		if !conversationTurnFits(input, turn, chosen) {
			truncated = true
			turn["assistantTruncated"] = true
			text := turn.S("assistant")
			// Search encoded prompt size, not raw text size: JSON escaping can expand a legal
			// projection several times. Each probe ends on a UTF-8 boundary.
			low, high := 0, len(text)
			for low < high {
				mid := low + (high-low+1)/2
				end := mid
				for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
					end--
				}
				turn["assistant"] = text[:end]
				if conversationTurnFits(input, turn, chosen) {
					low = mid
				} else {
					high = mid - 1
				}
			}
			for low > 0 && low < len(text) && !utf8.RuneStart(text[low]) {
				low--
			}
			turn["assistant"] = text[:low]
			if conversationTurnFits(input, turn, chosen) {
				chosen = append([]Object{turn}, chosen...)
			}
			break
		}
		chosen = append([]Object{turn}, chosen...)
	}
	if len(chosen) != 0 {
		input["conversationHistory"] = chosen
	}
	if truncated || unavailable {
		input["conversationHistoryIncomplete"] = true
	}
}

// Leave room for a frozen Revision reference added only at Session Start and for the missing
// history annotation. The renderer owns JSON escaping and all surrounding prompt metadata.
func conversationTurnFits(input, turn Object, chosen []Object) bool {
	probe := make(Object, len(input)+2)
	for k, value := range input {
		probe[k] = value
	}
	probe["conversationHistory"] = append([]Object{turn}, chosen...)
	probe["conversationHistoryIncomplete"] = true
	return len(renderAgentInitialTurn(probe, nil)) <= taskConversationPromptBudget-1024
}
