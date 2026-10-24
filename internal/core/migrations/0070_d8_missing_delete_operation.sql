-- Keep an absent D8 delete identity explicit instead of fabricating an operation or ledger.
ALTER TABLE agent_run_reclamation_intents
 ADD COLUMN legacy_delete_operation_missing boolean NOT NULL DEFAULT false;
ALTER TABLE agent_run_reclamation_intents
 DROP CONSTRAINT agent_run_reclamation_intents_check1;
ALTER TABLE agent_run_reclamation_intents
 ADD CONSTRAINT agent_run_reclamation_legacy_delete_identity CHECK (
  (source='legacy_d8' AND (
   (legacy_delete_operation_id IS NOT NULL AND NOT legacy_delete_operation_missing)
   OR (legacy_delete_operation_id IS NULL AND legacy_delete_operation_missing)))
  OR (source='residual_ledger' AND legacy_delete_operation_id IS NULL AND NOT legacy_delete_operation_missing)
 );
