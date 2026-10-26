package core

import (
	"context"
	"strings"
)

const maxMetricInteger int64 = 9007199254740991

// BeginModelAttempt records durable intent before an outbound request. Scope and frozen model
// come from the current grant's database binding, never from client headers or request bodies.
// Reusing an attempt ID is allowed only for an identical start, not another network attempt.
func (s *Store) BeginModelAttempt(ctx context.Context, grant *ModelGrant, attemptID string, streamed bool) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(grant != nil && validID(attemptID) && validID(grant.ID), 400, "invalid_model_attempt")
		g := t.one("SELECT * FROM model_access_grants WHERE id=$1 AND revoked_at IS NULL AND expires_at>now()", grant.ID)
		require(g != nil, 403, "model_grant_revoked")
		scope := modelGrantScope(t, g)
		b := liveModelBinding(t, scope, g.S("bindingId"), g.S("executionId"))
		require(b.S("runId") == grant.RunID && g.S("bindingId") == grant.BindingID && g.S("executionId") == grant.ExecutionID, 403, "model_grant_revoked")
		if old := t.one("SELECT grant_id,streamed,outcome FROM model_request_attempts WHERE id=$1", attemptID); old != nil {
			require(old.S("grantId") == grant.ID && old.B("streamed") == streamed && old.S("outcome") == "started", 409, "model_attempt_conflict")
			return nil
		}
		// The binding names its owner with exactly one of run_id / workflow_run_id (0040), and the
		// ledger mirrors that discriminator (0044). liveModelBinding maps either owner into runId for
		// the runtime gates, so the raw workflow_run_id column decides which foreign key this row
		// satisfies — an IssueRun attempt must never be written under the WorkflowRun reference.
		runID, workflowRunID := b.S("runId"), ""
		if b.S("workflowRunId") != "" {
			runID, workflowRunID = "", b.S("workflowRunId")
		}
		t.exec(`INSERT INTO model_request_attempts(id,tenant_id,run_id,workflow_run_id,binding_id,grant_id,execution_id,model_id,protocol,streamed)
		 VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10)`, attemptID, scope.TenantID, runID, workflowRunID, g.S("bindingId"), grant.ID, g.S("executionId"), b.O("model").S("id"), b.S("protocol"), streamed)
		return nil
	})
	return err
}

// FinishModelAttempt commits one immutable content-free observation. Finalization remains
// permitted after cancellation/revocation: it closes existing intent and cannot authorize work.
// An identical retry is harmless; changing an already finalized observation is a conflict.
func (s *Store) FinishModelAttempt(ctx context.Context, attemptID string, finish *ModelAttemptFinish) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(finish != nil && validID(attemptID), 400, "invalid_model_attempt")
		validateModelAttemptFinish(finish)
		encoded := jsonText(finish)
		old := t.one("SELECT outcome,observation=$2::jsonb AS identical FROM model_request_attempts WHERE id=$1 FOR UPDATE", attemptID, encoded)
		require(old != nil, 404, "not_found")
		if old.S("outcome") != "started" {
			require(old.B("identical"), 409, "model_attempt_conflict")
			return nil
		}
		u := finish.Usage
		t.exec(`UPDATE model_request_attempts SET finished_at=clock_timestamp(),outcome=$2,error_code=NULLIF($3,''),
		 http_status=$4,duration_ms=$5,first_output_ms=$6,terminal_observed=$7,input_tokens=$8,output_tokens=$9,
		 total_tokens=$10,cache_read_tokens=$11,cache_write_tokens=$12,reasoning_tokens=$13,input_semantics=$14,
		 usage_availability=$15,observation=$16::jsonb WHERE id=$1`, attemptID, finish.Outcome, finish.ErrorCode,
			finish.HTTPStatus, finish.DurationMS, finish.FirstOutputMS, finish.TerminalObserved, u.InputTokens,
			u.OutputTokens, u.TotalTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens,
			u.InputSemantics, u.Availability, encoded)
		return nil
	})
	return err
}

func validateModelAttemptFinish(f *ModelAttemptFinish) {
	switch f.Outcome {
	case "succeeded", "failed", "cancelled", "interrupted", "incomplete":
	default:
		require(false, 400, "invalid_model_attempt")
	}
	require(f.DurationMS >= 0 && f.DurationMS <= maxMetricInteger, 400, "invalid_model_attempt")
	require(f.FirstOutputMS == nil || (*f.FirstOutputMS >= 0 && *f.FirstOutputMS <= f.DurationMS), 400, "invalid_model_attempt")
	require(f.HTTPStatus == nil || (*f.HTTPStatus >= 100 && *f.HTTPStatus <= 599), 400, "invalid_model_attempt")
	if f.ErrorCode != "" {
		require(len(f.ErrorCode) <= 80 && f.ErrorCode[0] >= 'a' && f.ErrorCode[0] <= 'z' && strings.Trim(f.ErrorCode, "abcdefghijklmnopqrstuvwxyz0123456789_") == "", 400, "invalid_model_attempt")
	}
	require(f.Outcome != "succeeded" || f.TerminalObserved, 400, "invalid_model_attempt")
	u := f.Usage
	require(u.InputSemantics == "includes_cache" || u.InputSemantics == "excludes_cache" || u.InputSemantics == "unknown", 400, "invalid_model_attempt")
	known := 0
	for _, value := range []*int64{u.InputTokens, u.OutputTokens, u.TotalTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens} {
		if value != nil {
			require(*value >= 0 && *value <= maxMetricInteger, 400, "invalid_model_attempt")
			known++
		}
	}
	switch u.Availability {
	case "complete":
		require(f.TerminalObserved && u.InputTokens != nil && u.OutputTokens != nil, 400, "invalid_model_attempt")
	case "partial":
		require(known > 0, 400, "invalid_model_attempt")
	case "unavailable":
		require(known == 0, 400, "invalid_model_attempt")
	default:
		require(false, 400, "invalid_model_attempt")
	}
}
