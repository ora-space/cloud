-- Revisions are delivered over Git (ADR cloud/revision "Git Server delivery and verified delivery
-- refs" D4). This is a breaking switch: no data is migrated, development data is rebuilt.
--
-- A Revision delivered over Git records the delivery ref Cloud verified through the ref query. Rows
-- registered before the switch keep their bundle shape and meaning and are never rewritten; the
-- two shapes are mutually exclusive.
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS delivery_ref text;
ALTER TABLE revisions DROP CONSTRAINT IF EXISTS revisions_bundle_shape;
ALTER TABLE revisions DROP CONSTRAINT IF EXISTS revisions_shape;
ALTER TABLE revisions ADD CONSTRAINT revisions_shape CHECK (
  (delivery_ref IS NULL AND (
    (base_commit = final_commit AND num_nonnulls(bundle_key, bundle_size, bundle_sha256, bundle_revision_id) = 0)
    OR (base_commit <> final_commit AND bundle_revision_id IS NULL
        AND num_nonnulls(bundle_key, bundle_size, bundle_sha256) = 3
        AND bundle_size > 0 AND bundle_sha256 ~ '^[0-9a-f]{64}$')
    OR (base_commit <> final_commit AND bundle_revision_id IS NOT NULL AND prior_revision_id IS NOT NULL
        AND num_nonnulls(bundle_key, bundle_size, bundle_sha256) = 0)))
  OR (delivery_ref IS NOT NULL
      AND delivery_ref ~ '^refs/ora/deliveries/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
      AND num_nonnulls(bundle_key, bundle_size, bundle_sha256, bundle_revision_id) = 0)
);
-- One delivery attempt, one ref: a second Revision can never claim a ref another one verified.
CREATE UNIQUE INDEX IF NOT EXISTS revisions_delivery_ref ON revisions(delivery_ref) WHERE delivery_ref IS NOT NULL;
