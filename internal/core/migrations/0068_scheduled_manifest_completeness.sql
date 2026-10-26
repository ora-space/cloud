-- Keep the bounded collector's completeness facts independently of the listed refusal details.
-- The immutable session result retains the original declaration; these columns support run reads
-- after occurrence trimming without interpreting a truncated list as a complete one.
ALTER TABLE scheduled_run_artifact_deliveries
 ADD COLUMN omitted_rejections bigint NOT NULL DEFAULT 0
  CHECK (omitted_rejections >= 0 AND omitted_rejections <= 4294967295),
 ADD COLUMN walk_truncated boolean NOT NULL DEFAULT false;
