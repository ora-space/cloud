-- WorkflowRun identity and bindings (specs decisions/cloud/workflow
-- 0-cloud-side-scheduler-owns-workflow-execution.md D3/D8, landing order step 4 "运行身份";
-- semantics inherited from decisions/cloud/workflow/20261015-executor-slots-replace-agent-model-ids.md
-- and decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md).
--
-- The WorkflowRun is the execution aggregate for workflow invocations, so it needs the frozen identity
-- the run Workspace lifecycle reads (who invoked it, which project its Workspace provisions in), and
-- the control plane's work registry must accept a WorkflowRun as an alternative work owner without a
-- new Controller contract (D8: 判别列 + 恰好一个约束). Session execution itself stays unwired until
-- the executor-slot resolution bridge lands per the slots decision's 落地顺序 — until then Cloud does
-- not execute workflows, and behavior is unchanged.

-- 1. Frozen run identity. Both nullable: rows written before this migration carry no recoverable
-- actor, and project_id is filled by the invocation path that knows a project (the @-path; the
-- editor's run button does not). New runs freeze both at creation.
ALTER TABLE workflow_runs ADD COLUMN created_by uuid REFERENCES users(id);
ALTER TABLE workflow_runs ADD COLUMN project_id uuid REFERENCES projects(id);

-- 2. A run Workspace can belong to a WorkflowRun. The binding is exclusive with the IssueRun one;
-- workspaces without either stay the public kind.
ALTER TABLE workspaces ADD COLUMN workflow_run_id uuid UNIQUE REFERENCES workflow_runs(id);
ALTER TABLE workspaces ADD CONSTRAINT workspace_run_binding_exclusive
 CHECK (issue_run_id IS NULL OR workflow_run_id IS NULL);

-- 3. Execution work carries the same optional ownership: a work item names exactly one run, of either
-- kind. The single-column uniqueness splits into one partial index per owner, so a run of each kind
-- keeps at most one unregistered item and the two ownerships never collide.
ALTER TABLE execution_work ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE execution_work ADD COLUMN workflow_run_id uuid REFERENCES workflow_runs(id);
ALTER TABLE execution_work ADD CONSTRAINT execution_work_owner_exclusive
 CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL));
DROP INDEX one_unregistered_execution_work;
CREATE UNIQUE INDEX one_unregistered_execution_work ON execution_work(run_id)
 WHERE execution_id IS NULL AND workflow_run_id IS NULL;
CREATE UNIQUE INDEX one_unregistered_workflow_execution_work ON execution_work(workflow_run_id)
 WHERE execution_id IS NULL AND run_id IS NULL;

-- 4. Runtime control's run reservation names the run of either kind that owns the Workspace's next
-- control epoch. The IssueRun FK is dropped, not reworked: a polymorphic run reference cannot be a
-- foreign key, so the liveness check moves to the code that grants on it (runtime_permit re-reads
-- the live run before a Node binds, reconciliation clears the reservation), and 0026's own CHECKs —
-- exactly one of operation/run id while maintenance is reserved — are unchanged.
ALTER TABLE runtime_controls DROP CONSTRAINT runtime_controls_maintenance_run_id_fkey;
