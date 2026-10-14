-- Fixed business deadlines are independent of mutable run activity and process configuration.
ALTER TABLE issue_runs ADD COLUMN startup_ready_at timestamptz;
ALTER TABLE issue_runs ADD COLUMN startup_deadline_at timestamptz;
ALTER TABLE issue_runs ADD COLUMN startup_resume jsonb;
ALTER TABLE issue_runs ADD COLUMN startup_ending_deadline_at timestamptz;

-- Existing active runs are initialized once by the serialized business scan. A successful create
-- operation supplies a ready anchor; without one the explicitly recorded first observation is used.
CREATE TABLE agent_run_residuals (
 run_id uuid PRIMARY KEY REFERENCES issue_runs(id),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 project_id uuid NOT NULL REFERENCES projects(id),
 workspace_id uuid NOT NULL REFERENCES workspaces(id),
 runtime_generation bigint NOT NULL,
 responsibility jsonb NOT NULL,
 reason text NOT NULL CHECK (reason = 'agent_start_failed'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 session_ended_at timestamptz,
 cleared_at timestamptz
);
CREATE INDEX agent_run_residuals_scope ON agent_run_residuals(tenant_id,workspace_id);
CREATE INDEX agent_startup_due ON issue_runs(startup_deadline_at)
 WHERE executor_type='agent' AND phase='starting' AND deleted_at IS NULL;
CREATE INDEX agent_startup_ending_due ON issue_runs(startup_ending_deadline_at)
 WHERE phase IN ('starting','running') AND startup_ending_deadline_at IS NOT NULL;
