package core

import "time"

func startupDuration(configured, fallback time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return fallback
}

// freezeAgentStartup fixes both the prior Revision and the ready-based window in one transaction.
// Legacy rows use completed creation evidence, or one explicitly recorded observation when absent.
func (s *Store) freezeAgentStartup(t *transaction, runID string) {
	run := t.one("SELECT * FROM issue_runs WHERE id=$1 AND executor_type='agent' AND deleted_at IS NULL AND startup_deadline_at IS NULL AND phase='starting'", runID)
	if run == nil {
		return
	}
	choice := selectPriorRevision(t, run)
	window := startupDuration(s.AgentStartTimeout, 2*time.Minute)
	if work := t.one("SELECT input FROM execution_work WHERE run_id=$1 AND kind='agent_session'", runID); work != nil {
		// A pre-upgrade declaration owns its frozen input; do not select a different Revision.
		choice.prior = work.O("input").O("priorRevision")
		if len(choice.prior) == 0 {
			choice.prior = nil
		}
	}
	if choice.prior != nil {
		window = startupDuration(s.AgentRestoreTimeout, 6*time.Minute)
	}
	t.exec(`UPDATE issue_runs SET startup_ready_at=COALESCE(
	 (SELECT max(updated_at) FROM operations WHERE workspace_id=issue_runs.workspace_id
	  AND kind='create_workspace' AND state='succeeded'),clock_timestamp()),
	 startup_resume=$2 WHERE id=$1`, runID, jsonText(Object{"prior": choice.prior, "context": choice.context}))
	t.exec("UPDATE issue_runs SET startup_deadline_at=startup_ready_at+make_interval(secs=>$2) WHERE id=$1", runID, window.Seconds())
	if run.S("threadState") == "ending" {
		s.freezeAgentEnding(t, runID)
	}
}

func frozenAgentResume(t *transaction, runID string) resumeChoice {
	stored := t.one("SELECT startup_resume FROM issue_runs WHERE id=$1", runID).O("startupResume")
	choice := resumeChoice{}
	if prior := stored.O("prior"); len(prior) != 0 {
		choice.prior = prior
	}
	if context := stored.O("context"); len(context) != 0 {
		choice.context = context
	}
	return choice
}

// freezeAgentEnding never renews the first ending window, including across replicas and restarts.
func (s *Store) freezeAgentEnding(t *transaction, runID string) {
	window := startupDuration(s.AgentEndConfirmTimeout, 5*time.Minute)
	t.exec(`UPDATE issue_runs SET startup_ending_deadline_at=COALESCE(
	 (SELECT min(created_at) FROM thread_commands WHERE run_id=$1 AND kind='end_session'),clock_timestamp())
	 +make_interval(secs=>$2) WHERE id=$1 AND startup_ending_deadline_at IS NULL`, runID, window.Seconds())
}

// abandonExpiredAgentStartup commits a business terminal and its residual ownership together.
// It preserves every execution fact and never fabricates termination, receipts, delivery or deletion.
func (s *Store) abandonExpiredAgentStartup(t *transaction, runID string) bool {
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND phase IN ('starting','running')
	 AND thread_state='ending' AND startup_ending_deadline_at<=clock_timestamp()`, runID)
	if run == nil {
		return false
	}
	e := t.one("SELECT execution_id FROM node_executions WHERE operation_id=$1 AND kind='agent_session' AND result IS NULL", runID)
	if e == nil {
		return false
	}
	wid := run.S("workspaceId")
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND issue_run_id=$2", wid, runID)
	refs := Object{
		"executions": t.list("SELECT execution_id,node_id,operation_id,work_id FROM node_executions WHERE workspace_id=$1", wid),
		"sandboxes":  t.list("SELECT id,generation FROM sandbox_instances WHERE workspace_id=$1", wid),
		"nodes":      t.list("SELECT id,sandbox_instance_id FROM node_instances WHERE workspace_id=$1", wid),
		"operations": t.list("SELECT id FROM operations WHERE workspace_id=$1", wid),
		"effects":    t.list("SELECT id,operation_id FROM external_effects WHERE workspace_id=$1", wid),
		"clones":     t.list("SELECT execution_id FROM clone_executions WHERE workspace_id=$1", wid),
	}
	t.exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason)
	 VALUES($1,$2,$3,$4,$5,$6,'agent_start_failed') ON CONFLICT(run_id) DO NOTHING`, runID, run.S("tenantId"), w.S("projectId"), wid, w.N("runtimeGeneration"), jsonText(refs))
	// The expired business window says nothing about physical stop. Retain one named observation
	// on this same residual, addressed by the original execution, without rewriting responsibility.
	t.exec(`UPDATE agent_run_residuals SET stop_evidence=stop_evidence||jsonb_build_object(
	 'stop_unconfirmed',jsonb_build_object('kind','stop_unconfirmed','source','cloud',
	 'observedAt',clock_timestamp(),'executionId',$2::text))
	 WHERE run_id=$1 AND NOT stop_evidence ? 'stop_unconfirmed'`, runID, e.S("executionId"))
	t.exec(`UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,version=version+1 WHERE id=$1`, wid)
	t.exec(`UPDATE runtime_controls SET state='reconciling',binding_confirmed=false,input_closed=false,
	 version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1`, wid)
	t.exec("UPDATE run_model_bindings SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE run_id=$1", runID)
	t.exec(`UPDATE model_access_grants SET revoked_at=COALESCE(revoked_at,clock_timestamp())
	 WHERE binding_id IN (SELECT id FROM run_model_bindings WHERE run_id=$1)`, runID)
	t.exec("UPDATE thread_entries SET status='discarded' WHERE run_id=$1 AND source='user' AND status='queued'", runID)
	t.exec(`UPDATE issue_runs SET phase='done',status='failed',thread_state='ended',idle_since=NULL,
	 failure_reason='agent_start_failed',completed_at=clock_timestamp(),version=version+1,updated_at=clock_timestamp(),
	 result=COALESCE(result,'{}'::jsonb)||'{"deliveryState":"failed","startup":{"state":"abandoned","reason":"confirmation_timeout","source":"cloud"}}'::jsonb
	 WHERE id=$1`, runID)
	appendSessionFailure(t, run, "agent_start_failed")
	threadChanged(t, run)
	appendActivity(t, run.S("tenantId"), run.S("issueId"), activityActor(run.S("executorType")), run.S("executorId"),
		"run.failed", runActivityDetails(runID, run.S("executorType"), run.S("executorId"), Object{"deliveryState": "failed"}))
	return true
}
