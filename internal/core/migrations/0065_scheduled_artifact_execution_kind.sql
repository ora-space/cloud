-- The scheduled artifact upload is a run-scoped Node execution like the session and the Revision
-- delivery, so it belongs to the same closed kind set.

ALTER TABLE node_executions DROP CONSTRAINT node_executions_kind_check;
ALTER TABLE node_executions ADD CONSTRAINT node_executions_kind_check
 CHECK (kind IN ('install_plugins','remove_plugins','agent_session','deliver_revision','deliver_scheduled_artifacts'));

-- Keep the existing session/Revision mutual exclusion. Artifact uploads have their own pending
-- uniqueness, allowing a Revision and an upload together without weakening the historical fence.
CREATE UNIQUE INDEX one_pending_scheduled_artifact_execution ON node_executions(operation_id)
 WHERE kind='deliver_scheduled_artifacts' AND result IS NULL AND terminated_by_force_stop_id IS NULL;
