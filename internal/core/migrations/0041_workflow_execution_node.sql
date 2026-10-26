-- The graph node a WorkflowRun's session belongs to (specs decisions/cloud/workflow
-- 0-cloud-side-scheduler-owns-workflow-execution.md D1 条件② and D4; D8 owns the WorkflowRun
-- attribution of execution_work on Cloud's own tables).
--
-- D4 executes one run's agent nodes serially through the run's single Workspace, so a run has as
-- many agent sessions in its life as it has agent nodes. Two run-scoped identities assumed the
-- opposite and made every node after the first unreachable:
--
--   * the scheduler resolved a running node's terminal evidence as "the newest agent_session result
--     for this run", so the second node inherited the first node's result and was committed
--     succeeded without ever executing (invariant 5: a node with no committed result is never
--     recorded as succeeded);
--   * one_session_per_run (0025) allowed one agent_session per run for its lifetime, so the second
--     node's dispatch could never register.
--
-- Both are corrected to the granularity the design intends — per graph node. The graph node is
-- recorded where the session is released (the work item, which the scheduler owns) and where the
-- execution is registered (the node execution), and the lifetime uniqueness is split by owner: an
-- IssueRun still runs one session in its life, a WorkflowRun one per agent node.

-- 1. The graph node a work item admits. IssueRun work and tenant clone work carry no graph node.
ALTER TABLE execution_work ADD COLUMN workflow_node_id text
  CHECK (workflow_node_id IS NULL OR length(workflow_node_id) BETWEEN 1 AND 200);
ALTER TABLE execution_work ADD CONSTRAINT execution_work_node_belongs_to_workflow_run
  CHECK (workflow_node_id IS NULL OR workflow_run_id IS NOT NULL);

-- 2. The graph node an execution ran for. The dispatch copies it off the work item it consumes, the
-- way it already copies work_id. It is denormalized rather than read through work_id because a
-- partial index predicate cannot join.
ALTER TABLE node_executions ADD COLUMN workflow_node_id text
  CHECK (workflow_node_id IS NULL OR length(workflow_node_id) BETWEEN 1 AND 200);

-- 3. Split the lifetime uniqueness by owner. IssueRuns keep 0025's rule exactly (they have no graph
-- node); WorkflowRuns get one session per agent node instead of one per run.
DROP INDEX one_session_per_run;
CREATE UNIQUE INDEX one_session_per_run ON node_executions(operation_id)
  WHERE kind='agent_session' AND workflow_node_id IS NULL;
CREATE UNIQUE INDEX one_session_per_workflow_node ON node_executions(operation_id, workflow_node_id)
  WHERE kind='agent_session' AND workflow_node_id IS NOT NULL;
