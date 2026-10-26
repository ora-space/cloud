package core

// appendSessionFailure records a Cloud-owned terminal classification alongside unmodified Node
// history. It runs in terminal takeover's transaction, so replay cannot append a second entry.
func appendSessionFailure(t *transaction, run Object, detail string) {
	next := t.one("SELECT COALESCE(MAX(seq),0)+1 AS next FROM thread_entries WHERE run_id=$1", run.S("id")).N("next")
	code := publicSessionFailureCode(detail)
	t.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record)
		VALUES($1,$2,'system','session_failed',$3)`, run.S("id"), next, jsonText(Object{"type": "sessionFailed", "code": code}))
	threadAppended(t, run)
}

// publicSessionFailureCode accepts only bounded startup or Node classifications. Raw ACP/provider
// diagnostics are never a public failureReason, even when supplied by a registered Node.
//
// The plugin-owned adapter codes stay distinct because ADR D2 tells an incomplete configuration, an
// incompatible one, a refused grant and a failed preparation apart; collapsing them into
// `agent_failed` would leave the user no way to know which one to fix. `agent_plugin_unavailable` is
// the same kind of user-actionable cause — the run's pinned plugin package is not installed on its
// Workspace — and it is already a public code in the personal-agent admission table.
func publicSessionFailureCode(detail string) string {
	switch detail {
	case "agent_turn_failed", "agent_stream_closed", "agent_unavailable", "agent_start_failed",
		"agent_authentication_required", "model_proxy_unavailable", "model_access_unavailable",
		"model_access_expired", "thread_unavailable",
		"agent_adapter_unavailable", "agent_model_configuration_required",
		"agent_model_configuration_invalid", "agent_model_protocol_mismatch", "model_grant_mismatch",
		"agent_model_adapter_failed", "agent_model_adapter_not_applied", "agent_plugin_unavailable":
		return detail
	default:
		return "agent_failed"
	}
}
