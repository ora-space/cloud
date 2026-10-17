package core

import "context"

// Settling repository Operations from what the executor reported. The worker has already turned
// the executor's report into a verdict where Cloud's own check is required (a sync's refs are
// verified through the ref query before it may succeed); these methods only record it, fenced on
// the lease generation that ran the task.

// SyncResult is the worker's verdict on one sync.
type SyncResult struct {
	// State is succeeded, failed or superseded.
	State     string
	ErrorCode string
	// CommitOID and Forced describe a succeeded sync: the snapshot the Git Server now holds and
	// whether it is not a descendant of the previous tracking value (external force-push).
	CommitOID string
	Forced    bool
	// Observed is the tracking value a superseded sync saw.
	Observed string
	// Detail is the executor's redacted, bounded explanation.
	Detail string
}

// SettleRepositorySync records a running sync's verdict.
func (s *Store) SettleRepositorySync(ctx context.Context, id string, gen int64, r SyncResult) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := leasedOperation(t, id, gen, "running")
		require(o.S("type") == RepositorySync, 409, "operation_type_mismatch")
		result := Object{}
		if r.Detail != "" {
			result["detail"] = boundedDetail(r.Detail)
		}
		switch r.State {
		case "succeeded":
			require(GitObjectID(r.CommitOID) && !gitZeroID(r.CommitOID), 400, "invalid_commit")
			result["commitOid"], result["forced"] = r.CommitOID, r.Forced
			settleSync(t, o, "succeeded", "", result)
		case "superseded":
			if GitObjectID(r.Observed) {
				result["observedOid"] = r.Observed
			}
			settleSync(t, o, "superseded", "", result)
		case "failed":
			settleSync(t, o, "failed", SyncErrorCode(r.ErrorCode), result)
		default:
			reject(400, "invalid_state")
		}
		return nil
	})
	return err
}

// boundedDetail keeps an already-redacted explanation within the audit and result budget.
func boundedDetail(s string) string {
	const limit = 2048
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// PublishResult is the executor's classification of one publish task (executor ADR D8), plus
// whether the connection to the executor was lost after the task was sent.
type PublishResult struct {
	Classification string
	ErrorCode      string
	Detail         string
	PreObserved    string
	PostObserved   string
	// Confirmed reports that the outcome was established by the post-push ls-remote rather than
	// by the push report.
	Confirmed bool
}

// publishErrorCodes are the codes a publish may end with; anything else becomes publish_failed.
var publishErrorCodes = map[string]bool{
	"not_fast_forward": true, "source_unavailable": true, "external_rejected": true, "push_denied": true,
	"credential_rejected": true, "credential_unavailable": true, "redemption_failed": true, "timeout": true,
	"executor_busy": true, "executor_unreachable": true, "egress_denied": true, "repository_url_invalid": true,
	"authorization_invalid": true, "authorization_expired": true, "authorization_mismatch": true, "fetch_failed": true,
	"git_server_unavailable": true, "not_applied": true, "outcome_unknown": true, "moved_after_uncertain": true,
}

func publishErrorCode(code string) string {
	if publishErrorCodes[code] {
		return code
	}
	return "publish_failed"
}

// SettleRepositoryPublish records a running publish's classification (publish ADR D3 table).
func (s *Store) SettleRepositoryPublish(ctx context.Context, id string, gen int64, r PublishResult) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := leasedOperation(t, id, gen, "running")
		require(o.S("type") == RepositoryPublish, 409, "operation_type_mismatch")
		result := Object{"confirmedByVerification": r.Confirmed}
		for k, v := range map[string]string{"preObservedOid": r.PreObserved, "postObservedOid": r.PostObserved} {
			if GitObjectID(v) {
				result[k] = v
			}
		}
		if r.Detail != "" {
			result["detail"] = boundedDetail(r.Detail)
		}
		auditRepositoryOperation(t, id, "executed", "", Object{"classification": r.Classification, "preObservedOid": result["preObservedOid"], "postObservedOid": result["postObservedOid"], "detail": result["detail"]})
		switch r.Classification {
		case "applied":
			finishRepositoryOperation(t, id, "succeeded", "", result)
			if r.PostObserved != "" && r.PostObserved != o.S("sourceCommit") {
				t.exec("UPDATE repository_operations SET result=result||'{\"movedAfterPublish\":true}'::jsonb WHERE id=$1", id)
			}
			requestSyncAfterPublish(t, o)
		case "conflict":
			finishRepositoryOperation(t, id, "conflict", "", result)
		case "rejected", "not_fast_forward":
			code := "external_rejected"
			switch {
			case r.Classification == "not_fast_forward":
				code = "not_fast_forward"
			case r.ErrorCode == "push_denied":
				// The platform refused the token itself: the version has not proven publish.
				code = "push_denied"
				dropCapability(t, o.S("credentialRefId"), o.N("credentialVersion"), CapabilityPublish)
			}
			finishRepositoryOperation(t, id, "rejected", code, result)
		case "source_unavailable":
			finishRepositoryOperation(t, id, "failed", "source_unavailable", result)
		case "failed":
			code := publishErrorCode(r.ErrorCode)
			finishRepositoryOperation(t, id, "failed", code, result)
			switch code {
			case "push_denied":
				dropCapability(t, o.S("credentialRefId"), o.N("credentialVersion"), CapabilityPublish)
			case "credential_rejected":
				markCredentialInvalid(t, o.S("credentialRefId"), o.N("credentialVersion"))
			}
		case "uncertain":
			t.exec("UPDATE repository_operations SET result=result||$2::jsonb WHERE id=$1", id, jsonText(result))
			enterUncertain(t, o, "outcome_unknown")
		default:
			reject(400, "invalid_classification")
		}
		return nil
	})
	return err
}

// enterUncertain parks a publish whose push may have happened. The first check waits for the
// task deadline, after which the executor can no longer push (publish ADR D5 item 5).
func enterUncertain(t *transaction, o Object, reason string) {
	t.exec(`UPDATE repository_operations SET state='uncertain',next_check_at=GREATEST(COALESCE(task_deadline,clock_timestamp()),clock_timestamp()),
	 result=result||jsonb_build_object('uncertainReason',$2::text) WHERE id=$1 AND state='running'`, o.S("id"), reason)
	auditRepositoryOperation(t, o.S("id"), "uncertain", "", Object{"reason": reason})
	invalidateRepository(t, o.S("id"))
}
