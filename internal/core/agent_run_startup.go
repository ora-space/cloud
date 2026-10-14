package core

import "fmt"

// reconcileAgentStartup separates declared intent from registered, possibly external execution.
// It never fabricates a Node result or receipt. Registered work retains its recovery responsibility.
func (s *Store) reconcileAgentStartup(t *transaction, runID string) bool {
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND executor_type='agent'
		AND (phase='starting' OR (phase='running' AND startup_ending_deadline_at IS NOT NULL))
		AND status IN ('dispatched','running') AND deleted_at IS NULL`, runID)
	if run == nil {
		return true
	}
	s.freezeAgentStartup(t, runID)
	if s.abandonExpiredAgentStartup(t, runID) {
		return true
	}
	evidence := inspectAgentStartup(t, run)
	ending := run.S("threadState") == "ending" || run.S("cancelRequestedAt") != ""
	if !evidence.registered {
		if ending {
			s.releaseUndispatchedAgentStartup(t, run, "cancelled", "", evidence.endReason)
			return true
		}
		if evidence.modelUnavailable {
			s.releaseUndispatchedAgentStartup(t, run, "failed", "model_proxy_unavailable", "capability_rejected")
			return true
		}
		if evidence.expired {
			s.releaseUndispatchedAgentStartup(t, run, "failed", "agent_start_failed", "startup_timeout")
			return true
		}
	} else if evidence.expired && run.O("result").O("startup").S("state") != "awaiting_node" {
		// A timeout cannot prove whether the Node acted. Request termination and make the
		// unresolved responsibility visible without discarding commands, work, turns or storage.
		// The cancel pass may not have materialized ending yet. Its accepted intent still owns
		// the outcome, even when this pass is the one that releases the termination command.
		policyCancel := !ending
		if run.S("threadState") != "ending" {
			s.endAgentThread(t, run, "cancelled")
		}
		s.freezeAgentEnding(t, runID)
		startup := Object{"state": "awaiting_node", "policyCancellation": policyCancel}
		t.exec(`UPDATE issue_runs SET failure_reason='agent_start_failed',
			result=COALESCE(result,'{}'::jsonb)||jsonb_build_object('startup',$2::jsonb),
			version=version+1,updated_at=now() WHERE id=$1`, runID, jsonText(startup))
		threadChanged(t, run)
		return true
	}
	return run.S("threadState") != ""
}

// releaseUndispatchedAgentStartup settles known absence of external execution and declares cleanup.
// Retiring control intent, the business terminal and its owned delete commit or roll back together.
func (s *Store) releaseUndispatchedAgentStartup(t *transaction, run Object, status, code, reason string) {
	rid := run.S("id")
	if !withdrawUndispatchedAgentSession(t, rid) {
		panic(databaseFailure{fmt.Errorf("startup %s gained dispatch evidence under the transaction lock", rid)})
	}
	startup := Object{"state": "withdrawn", "reason": reason}
	if moved := t.execRows(`UPDATE issue_runs SET phase='releasing',status=$2,completed_at=now(),failure_reason=$3,
		thread_state=CASE WHEN thread_state IS NULL THEN NULL ELSE 'ended' END,idle_since=NULL,
		result=COALESCE(result,'{}'::jsonb)||jsonb_build_object('revisionId',NULL,'deliveryState','skipped','startup',$4::jsonb),
		version=version+1,updated_at=now() WHERE id=$1 AND phase='starting' AND status='dispatched'`,
		rid, status, code, jsonText(startup)); moved != 1 {
		panic(databaseFailure{fmt.Errorf("startup %s was not released (rows %d)", rid, moved)})
	}
	t.exec("UPDATE thread_entries SET status='discarded' WHERE run_id=$1 AND source='user' AND status='queued'", rid)
	if run.S("threadState") != "" {
		if code != "" {
			appendSessionFailure(t, run, code)
		}
		threadChanged(t, run)
	}
	appendActivity(t, run.S("tenantId"), run.S("issueId"), activityActor(run.S("executorType")), run.S("executorId"),
		"run."+status, runActivityDetails(rid, run.S("executorType"), run.S("executorId"), Object{"deliveryState": "skipped"}))
	if err := s.declareDelete(t, run); err != nil {
		panic(databaseFailure{err})
	}
}
