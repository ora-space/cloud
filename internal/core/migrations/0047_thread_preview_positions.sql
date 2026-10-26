-- Node assigns this stable history position before a record settles. Preview content stays in
-- memory; only its position on the already durable record is retained for final-over-preview merge.
ALTER TABLE thread_entries ADD COLUMN history_sequence bigint
  CHECK (history_sequence IS NULL OR history_sequence BETWEEN 0 AND 4294967295);
CREATE INDEX thread_history_position ON thread_entries(run_id, history_sequence)
  WHERE history_sequence IS NOT NULL;
