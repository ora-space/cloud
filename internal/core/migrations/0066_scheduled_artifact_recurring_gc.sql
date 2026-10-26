-- A sweep is an observation, not proof that an admitted PUT can no longer complete. Keep every
-- settled delivery eligible, including delivered runs whose index retention can prune later.
DROP INDEX scheduled_run_artifact_deliveries_gc;
CREATE INDEX scheduled_run_artifact_deliveries_gc
 ON scheduled_run_artifact_deliveries(artifacts_gc_at NULLS FIRST, settled_at, issue_run_id)
 WHERE state IN ('delivered','failed','partial','skipped');
COMMENT ON COLUMN scheduled_run_artifact_deliveries.artifacts_gc_at IS
 'Last successful sweep; settled deliveries are checked again for late PUTs and pruned objects';
