-- Business settlement never manufactures a Node terminal result or receipt. Legacy rows retain
-- their original facts and have no fabricated dispatch-instance identity.
ALTER TABLE node_executions
    ADD COLUMN business_settled_at timestamptz,
    ADD COLUMN business_settlement_source text,
    ADD COLUMN dispatch_identity jsonb,
    ADD COLUMN non_acceptance_proof jsonb,
    ADD CONSTRAINT delivery_business_settlement_pair CHECK (
        (business_settled_at IS NULL AND business_settlement_source IS NULL) OR
        (business_settled_at IS NOT NULL AND business_settlement_source IS NOT NULL AND business_settlement_source IN
            ('artifact_deadline','revision_abandoned','legacy_run_settled'))
    ),
    ADD CONSTRAINT delivery_business_settlement_kind CHECK (
        business_settled_at IS NULL OR kind IN ('deliver_revision','deliver_scheduled_artifacts')
    ),
    ADD CONSTRAINT execution_non_acceptance_proof_shape CHECK (
        non_acceptance_proof IS NULL OR
        (business_settled_at IS NOT NULL AND dispatch_identity IS NOT NULL
            AND result IS NULL AND last_event_sequence=0
            AND jsonb_typeof(non_acceptance_proof)='object')
    );

CREATE TABLE execution_non_acceptance_conflicts (
    execution_id text NOT NULL REFERENCES node_executions(execution_id),
    fact_key text NOT NULL,
    fact jsonb NOT NULL,
    source jsonb NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(execution_id,fact_key)
);

-- Only the immutable artifact deadline is used as a historical instant. Other closed runs are
-- explicitly observed at migration time; neither sequence=0 nor a current Node supplies identity.
UPDATE node_executions e
SET business_settled_at=d.deadline_at, business_settlement_source='artifact_deadline'
FROM execution_work w JOIN scheduled_run_artifact_deliveries d ON d.issue_run_id=w.run_id
WHERE e.work_id=w.id AND e.kind='deliver_scheduled_artifacts' AND e.result IS NULL
    AND d.state<>'delivering' AND d.settled_reason='deadline_expired';

UPDATE node_executions e
SET business_settled_at=clock_timestamp(), business_settlement_source='legacy_run_settled'
FROM execution_work w JOIN issue_runs r ON r.id=w.run_id
WHERE e.work_id=w.id AND e.kind IN ('deliver_revision','deliver_scheduled_artifacts')
    AND e.result IS NULL AND e.business_settled_at IS NULL
    AND r.phase IN ('releasing','done');
