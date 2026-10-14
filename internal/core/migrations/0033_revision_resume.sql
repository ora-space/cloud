-- Resuming a new IssueRun from the Issue's latest Revision (issue-run 20261010 resume decision).
--
-- revisions.prior_revision_id   the Revision the run resumed from; NULL when it started fresh or
--                               restore never moved the checkout off the base commit.
-- revisions.bundle_revision_id  set only on a resumed run that added nothing: it stores no bundle
--                               and names the Revision that holds the identical one (always the
--                               holder itself, never a chain), so selection and restore take one hop.
-- revisions.resume_refused_*    written once by session settlement when a restore found the bundle's
--                               base commit gone from the remote; such a Revision is never selected.
-- issue_runs.resume_revision_id the Revision fixed into the run's session start, readable while the
--                               run is still in progress.
--
-- Existing rows keep their meaning: every new column is NULL for them, and the replaced CHECK
-- accepts exactly the rows the old one did plus the reused-bundle shape.

ALTER TABLE revisions ADD COLUMN IF NOT EXISTS prior_revision_id uuid REFERENCES revisions(id);
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS bundle_revision_id uuid REFERENCES revisions(id);
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS resume_refused_at timestamptz;
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS resume_refused_reason text;

ALTER TABLE revisions DROP CONSTRAINT IF EXISTS revisions_check;
ALTER TABLE revisions DROP CONSTRAINT IF EXISTS revisions_bundle_shape;
ALTER TABLE revisions ADD CONSTRAINT revisions_bundle_shape CHECK (
  (base_commit = final_commit AND num_nonnulls(bundle_key, bundle_size, bundle_sha256, bundle_revision_id) = 0)
  OR (base_commit <> final_commit AND bundle_revision_id IS NULL
      AND num_nonnulls(bundle_key, bundle_size, bundle_sha256) = 3
      AND bundle_size > 0 AND bundle_sha256 ~ '^[0-9a-f]{64}$')
  OR (base_commit <> final_commit AND bundle_revision_id IS NOT NULL AND prior_revision_id IS NOT NULL
      AND num_nonnulls(bundle_key, bundle_size, bundle_sha256) = 0)
);

ALTER TABLE revisions DROP CONSTRAINT IF EXISTS revisions_resume_refused;
ALTER TABLE revisions ADD CONSTRAINT revisions_resume_refused CHECK (
  (resume_refused_at IS NULL AND resume_refused_reason IS NULL)
  OR (resume_refused_at IS NOT NULL AND resume_refused_reason IS NOT NULL
      AND resume_refused_reason = 'prior_revision_base_unavailable')
);

ALTER TABLE issue_runs ADD COLUMN IF NOT EXISTS resume_revision_id uuid REFERENCES revisions(id);
