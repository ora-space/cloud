-- D5 of the bounded-agent-startup ADR: a run Workspace creation whose result stayed unknown gets
-- a bounded business abandonment instead of an unbounded `blocked` occupation of the Project slot.
--
-- The deadline columns are the business half of the control plane's `blocked` write: the first
-- transaction that persists `blocked` for a run-bound create_workspace operation anchors the
-- absolute deadline (workspace_blocked_source='blocked_write'). Pre-0035 blocked creations carry no
-- immutable first-blocked evidence (operations.updated_at is mutable), so nothing is backfilled:
-- the first policy scan anchors them as 'first_observed' and grants one default window, explicitly
-- not claiming the original window had been in force before the upgrade. Renewal is forbidden
-- everywhere: defer, manual retry, re-claiming, restarts and configuration changes never rewrite a
-- set deadline, and a temporary return to running/retry_wait does not erase it. Only the creation
-- reaching a real terminal ends the window, because the run then leaves `provisioning`.
ALTER TABLE issue_runs
  ADD COLUMN workspace_blocked_at timestamptz,
  ADD COLUMN workspace_deadline_at timestamptz,
  ADD COLUMN workspace_blocked_source text CHECK (workspace_blocked_source IN ('blocked_write', 'first_observed'));

-- The abandonment scan reads only provisioning-phase agent runs; the partial index keeps the
-- 10-second tick from touching the run table's live rows.
CREATE INDEX agent_workspace_blocked_due ON issue_runs(workspace_deadline_at)
  WHERE executor_type = 'agent' AND phase = 'provisioning' AND deleted_at IS NULL;

-- D4's ledger recorded only the startup-abandonment reason. D5 adds the creation-abandonment
-- reason for the same table and the same protections; the constraint keeps everything else about
-- the ledger unchanged.
ALTER TABLE agent_run_residuals DROP CONSTRAINT agent_run_residuals_reason_check;
ALTER TABLE agent_run_residuals
  ADD CONSTRAINT agent_run_residuals_reason_check CHECK (reason IN ('agent_start_failed', 'workspace_unavailable'));
