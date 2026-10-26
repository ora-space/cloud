-- Retire execution intents accepted before WorkflowRun linkage existed. A status-only settlement
-- cannot invent the snapshot/input those historical rows would have executed.
UPDATE issue_runs
SET status = 'failed',
    failure_reason = 'workflow_execution_unavailable_before_invocation_linkage',
    completed_at = now(), updated_at = now(), version = version + 1
WHERE executor_type = 'workflow' AND workflow_invocation_ref IS NULL
  AND status NOT IN ('completed', 'failed', 'cancelled');

ALTER TABLE issue_runs
  ADD CONSTRAINT issue_runs_workflow_invocation_fk
    -- The composite reference targets workflow_run_tenant_identity, the tenant-scoped UNIQUE(id,
    -- tenant_id) that 0044 already added; no new constraint is needed. workflow_invocation_ref had
    -- no writer before this migration, so dangling references are not expected and the FK fails closed.
    FOREIGN KEY (workflow_invocation_ref, tenant_id) REFERENCES workflow_runs(id, tenant_id),
  ADD CONSTRAINT issue_runs_workflow_invocation_uniq UNIQUE (workflow_invocation_ref),
  ADD CONSTRAINT issue_runs_workflow_invocation_shape
    CHECK ((executor_type <> 'workflow' AND workflow_invocation_ref IS NULL)
        OR (executor_type = 'workflow' AND (workflow_invocation_ref IS NOT NULL
            OR status IN ('completed', 'failed', 'cancelled')))) NOT VALID;

-- Historical terminal rows remain editable without inventing executions. New invocations enqueue
-- non-terminal rows and must carry a reference. Fresh and upgraded databases validate the same shape.
ALTER TABLE issue_runs VALIDATE CONSTRAINT issue_runs_workflow_invocation_shape;
