-- Named stop evidence is neither a new residual state nor permission to reclaim resources.
-- Keep the original reason and immutable responsibility independent of this observation.
ALTER TABLE agent_run_residuals
    ADD COLUMN stop_evidence jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(stop_evidence) = 'object');
