-- Wall-clock timestamps are not an execution order: NTP may move them backwards.
ALTER TABLE node_executions ADD COLUMN workflow_execution_order bigint
 CHECK(workflow_execution_order IS NULL OR (kind='agent_session' AND workflow_execution_order>=0));
CREATE UNIQUE INDEX workflow_session_execution_order ON node_executions(operation_id,workflow_execution_order)
 WHERE workflow_execution_order IS NOT NULL;
-- Old attempts retain unknown order; do not manufacture historical workflow delivery inputs.
