-- The residual row remains the sole reclamation authority. New defaults initialize legacy D4/D5
-- rows without rewriting their reason, responsibility, timestamps, or business settlement.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_attribute
    WHERE attrelid='agent_run_residuals'::regclass AND attname='reclaim_state' AND NOT attisdropped
  ) AND EXISTS (SELECT 1 FROM agent_run_residuals WHERE cleared_at IS NOT NULL) THEN
    RAISE EXCEPTION 'residual reclamation migration cannot initialize legacy rows with non-null cleared_at; explicit compatibility handling is required (A2)';
  END IF;
END $$;

ALTER TABLE agent_run_residuals
  ADD COLUMN IF NOT EXISTS reclaim_state text NOT NULL DEFAULT 'reconciling',
  ADD COLUMN IF NOT EXISTS disposition text,
  ADD COLUMN IF NOT EXISTS reclaim_id uuid,
  ADD COLUMN IF NOT EXISTS reclaim_mode text,
  ADD COLUMN IF NOT EXISTS reclaim_operation_id uuid,
  ADD COLUMN IF NOT EXISTS reclaim_authorization jsonb NOT NULL DEFAULT '{}',
  ADD COLUMN IF NOT EXISTS reclaim_targets jsonb NOT NULL DEFAULT '[]',
  ADD COLUMN IF NOT EXISTS reclaim_evidence jsonb NOT NULL DEFAULT '[]',
  ADD COLUMN IF NOT EXISTS reclaim_version bigint NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS reclaim_checked_at timestamptz,
  ADD COLUMN IF NOT EXISTS reclaim_conflicted_at timestamptz;

-- Named constraints make direct DDL replay safe too. They describe valid row shapes, not the
-- history of state transitions; conditional writes in the owning transaction enforce that history.
DO $reclaim_constraints$
DECLARE
  item record;
BEGIN
  FOR item IN SELECT * FROM (VALUES
    ('agent_run_residuals_reclaim_state_check',
      $$CHECK (reclaim_state IN ('reconciling','reclaiming','cleared'))$$),
    ('agent_run_residuals_disposition_check',
      $$CHECK (disposition IS NULL OR disposition IN ('released','terminated_data_retained','externally_confirmed'))$$),
    ('agent_run_residuals_reclaim_mode_check',
      $$CHECK (reclaim_mode IS NULL OR reclaim_mode IN ('owned_delete','controlled'))$$),
    ('agent_run_residuals_reclaim_json_check',
      $$CHECK (jsonb_typeof(reclaim_authorization)='object'
        AND jsonb_typeof(reclaim_targets)='array' AND jsonb_typeof(reclaim_evidence)='array')$$),
    ('agent_run_residuals_reclaim_version_check', $$CHECK (reclaim_version>=0)$$),
    ('agent_run_residuals_reclaim_responsibility_check',
      $$CHECK (
        (reclaim_state='reconciling' AND reclaim_id IS NULL AND reclaim_mode IS NULL
          AND reclaim_operation_id IS NULL AND reclaim_authorization='{}'::jsonb)
        OR (reclaim_state IN ('reclaiming','cleared') AND reclaim_id IS NOT NULL
          AND reclaim_mode IS NOT NULL AND reclaim_authorization<>'{}'::jsonb
          AND (reclaim_mode<>'owned_delete' OR reclaim_operation_id IS NOT NULL)))$$),
    ('agent_run_residuals_reclaim_completion_check',
      $$CHECK (
        (reclaim_state='cleared' AND cleared_at IS NOT NULL AND disposition IS NOT NULL)
        OR (reclaim_state<>'cleared' AND cleared_at IS NULL AND disposition IS NULL))$$),
    ('agent_run_residuals_reclaim_operation_fk',
      $$FOREIGN KEY (reclaim_operation_id) REFERENCES operations(id)$$)
  ) AS definitions(name, definition)
  LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
      WHERE conrelid='agent_run_residuals'::regclass AND conname=item.name) THEN
      EXECUTE format('ALTER TABLE agent_run_residuals ADD CONSTRAINT %I %s', item.name, item.definition);
    END IF;
  END LOOP;
END $reclaim_constraints$;

CREATE UNIQUE INDEX IF NOT EXISTS agent_run_residuals_reclaim_id_unique
  ON agent_run_residuals(reclaim_id) WHERE reclaim_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS agent_run_residuals_reclaim_operation_unique
  ON agent_run_residuals(reclaim_operation_id) WHERE reclaim_operation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS agent_run_residuals_reclaim_scan
  ON agent_run_residuals(reclaim_checked_at NULLS FIRST, created_at, run_id)
  WHERE reclaim_state IN ('reconciling','reclaiming');
