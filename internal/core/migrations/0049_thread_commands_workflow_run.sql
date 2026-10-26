-- thread_commands accept a WorkflowRun owner (circus decision #292 option A; specs ADR
-- decisions/cloud/workflow/20261019-workflow-session-end-after-initial-turn.md).
--
-- 0024 bound the command queue to issue_runs because only an IssueRun had a Thread and only a
-- Thread had a session-ending writer. A WorkflowRun's session is single-turn (the publish gate
-- refuses interactive nodes), so once its initial turn ends there is no future user turn and nobody
-- to end it: the scheduler reaches the same decision from the durable `turnEnded` receipt and
-- releases the existing Controller EndSession relay. That needs the command queue to accept a
-- WorkflowRun, using the discriminator + exactly-one-owner pattern 0039 gave execution_work and
-- 0044 gave model_request_attempts, so the Controller wire contract is unchanged (D8).
--
-- A WorkflowRun executes one agent node at a time but several over its life (D4), so a workflow
-- command names the graph node whose session it addresses. The per-node end command is also what
-- makes releasing it idempotent: the scheduler recomputes every tick and must not enqueue a second
-- EndSession for the same node.
--
-- Every existing row carries run_id: 0024 required it and no WorkflowRun command could ever be
-- inserted. There is nothing to backfill. Additive and forward-only.

ALTER TABLE thread_commands ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE thread_commands ADD COLUMN workflow_run_id uuid REFERENCES workflow_runs(id);
ALTER TABLE thread_commands ADD COLUMN workflow_node_id text
  CHECK (workflow_node_id IS NULL OR length(workflow_node_id) BETWEEN 1 AND 200);

-- Exactly one owner selects the applicable row, the way 0039/0044 split their owners. run_id keeps
-- 0024's foreign key; a polymorphic reference cannot be one foreign key, so the new owner carries
-- its own reference above.
ALTER TABLE thread_commands ADD CONSTRAINT thread_command_owner_exclusive
  CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL));

-- A workflow command is only meaningful against one graph node's session; an IssueRun command has
-- no graph node. Together with the constraint above this makes the node column present exactly when
-- the workflow owner is.
ALTER TABLE thread_commands ADD CONSTRAINT thread_command_workflow_scope
  CHECK ((workflow_run_id IS NULL) = (workflow_node_id IS NULL));

-- One end request per node's session. The partial predicate keeps IssueRun end requests (run_id,
-- no graph node) out of it: an IssueRun may hold several end requests over its life and 0024 gave
-- them no uniqueness.
CREATE UNIQUE INDEX one_workflow_session_end
  ON thread_commands(workflow_run_id, workflow_node_id)
  WHERE kind='end_session' AND workflow_run_id IS NOT NULL;
