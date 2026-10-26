-- Per-attempt identity for workflow graph nodes (specs decisions/cloud/workflow
-- 20261021-node-retry-and-resume-contract.md D4).
--
-- Retrying an agent node means one (run, node) pair owns several agent_session executions over the
-- run's life. Two identities assumed one execution per node and would refuse every attempt after the
-- first, or serve the first attempt's end request to the second attempt's session:
--
--   * one_session_per_workflow_node (0041) allowed one agent_session per (run, node) for the run's
--     lifetime, so attempt 2's dispatch could never register;
--   * one_workflow_session_end (0049) allowed one end_session command per (run, node), so attempt 2
--     could neither release its own end request nor be sure the first attempt's request would not be
--     delivered to it.
--
-- Both are re-keyed on the attempt. The attempt travels the same path workflow_node_id already
-- travels (0041): the scheduler records it on the work item, the dispatch copies it onto the
-- execution, and the command names the attempt whose session it addresses. It is Cloud-internal
-- routing like the node id: the Controller wire shape is unchanged (root decision D8).
--
-- Existing rows all default to 1 — every execution and end request that exists today belongs to a
-- node's first attempt, and IssueRun rows have no attempts at all. Nothing is backfilled, and the
-- columns are NOT NULL with that default so a writer that does not name one keeps the old meaning.
-- Additive and forward-only.

ALTER TABLE execution_work ADD COLUMN workflow_node_attempt integer NOT NULL DEFAULT 1
  CHECK (workflow_node_attempt >= 1);
ALTER TABLE node_executions ADD COLUMN workflow_node_attempt integer NOT NULL DEFAULT 1
  CHECK (workflow_node_attempt >= 1);
ALTER TABLE thread_commands ADD COLUMN workflow_node_attempt integer NOT NULL DEFAULT 1
  CHECK (workflow_node_attempt >= 1);

DROP INDEX one_session_per_workflow_node;
CREATE UNIQUE INDEX one_session_per_workflow_node
  ON node_executions(operation_id, workflow_node_id, workflow_node_attempt)
  WHERE kind='agent_session' AND workflow_node_id IS NOT NULL;

DROP INDEX one_workflow_session_end;
CREATE UNIQUE INDEX one_workflow_session_end
  ON thread_commands(workflow_run_id, workflow_node_id, workflow_node_attempt)
  WHERE kind='end_session' AND workflow_run_id IS NOT NULL;
