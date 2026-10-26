-- Legacy manual workflow IssueRuns never had an executor (circus#75 S4; specs ADR
-- decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md D3).
--
-- Before the Cloud-side scheduler, a manual run posted with executor_type='workflow' was persisted
-- as `queued` (or `dispatched`) and nothing ever claimed it: the Agent dispatcher claims only
-- executor_type='agent'. These rows carry no frozen execution identity under the workflow contract
-- — no WorkflowRun binding, no snapshot id in the run, no per-slot model bindings, no Project or
-- Workspace ownership — so they cannot be executed, and D3 forbids migrating or replaying them.
--
-- Their status is corrected once: failed, with a reason naming the fact (the runtime had no
-- workflow execution capability), and completed_at. They never executed, so no result, Revision or
-- resource is lost; the row is the whole record of the attempt.
--
-- Only live rows are corrected. A soft-deleted row is outside the live dataset and never blocked a
-- later run (the pending-dedup index already excludes it), so its stored history stays untouched.
-- New rows of this shape stop being produced when the launch gate binds a manual workflow run to a
-- real WorkflowRun (circus#71); this migration does not invent that binding.
UPDATE issue_runs
SET status='failed', failure_reason='workflow_execution_unavailable',
    completed_at=now(), version=version+1, updated_at=now()
WHERE executor_type='workflow' AND status IN ('queued','dispatched') AND deleted_at IS NULL;
