-- Empty regular files are valid Node artifacts. Keep the upper bound while accepting their
-- measured zero length throughout the persisted registration contract.
ALTER TABLE scheduled_run_artifacts DROP CONSTRAINT scheduled_run_artifacts_size_bytes_check;
ALTER TABLE scheduled_run_artifacts ADD CONSTRAINT scheduled_run_artifacts_size_bytes_check
 CHECK (size_bytes >= 0 AND size_bytes <= 10485760);
