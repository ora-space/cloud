-- Model attempts record WorkflowRun-owned bindings (circus decision #262 option A; specs ADR
-- 20261016-recorded-http-attempt-statistics). 0040 gave run_model_bindings an exactly-one owner
-- discriminator, but 0035's attempt ledger still required an IssueRun, so every WorkflowRun
-- request failed its foreign key before reaching the provider. The ledger now mirrors 0040: exactly
-- one of run_id / workflow_run_id, each with its own tenant-scoped run reference and binding
-- reference. Registration is the only consumer; WorkflowRun statistics reads stay closed.
--
-- Every existing row carries run_id: 0035 required it, and no WorkflowRun attempt could ever be
-- inserted. There is nothing to backfill. Additive and forward-only.

ALTER TABLE model_request_attempts ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE model_request_attempts ADD COLUMN workflow_run_id uuid;
ALTER TABLE model_request_attempts ADD CONSTRAINT model_attempt_owner_exclusive
 CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL));

-- Tenant-scoped identity for the new owner, and the binding identity its composite reference
-- needs; 0035 already added the IssueRun halves.
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_run_tenant_identity UNIQUE(id,tenant_id);
ALTER TABLE run_model_bindings ADD CONSTRAINT model_binding_workflow_run_identity UNIQUE(id,workflow_run_id);

-- Replace each single-owner reference with one per owner. A composite foreign key is not enforced
-- when any referencing column is NULL, so the discriminator above decides which one applies.
ALTER TABLE model_request_attempts DROP CONSTRAINT model_request_attempts_run_id_tenant_id_fkey;
ALTER TABLE model_request_attempts ADD CONSTRAINT model_attempt_issue_run_scope
 FOREIGN KEY(run_id,tenant_id) REFERENCES issue_runs(id,tenant_id);
ALTER TABLE model_request_attempts ADD CONSTRAINT model_attempt_workflow_run_scope
 FOREIGN KEY(workflow_run_id,tenant_id) REFERENCES workflow_runs(id,tenant_id);

ALTER TABLE model_request_attempts DROP CONSTRAINT model_request_attempts_binding_id_run_id_fkey;
ALTER TABLE model_request_attempts ADD CONSTRAINT model_attempt_issue_binding
 FOREIGN KEY(binding_id,run_id) REFERENCES run_model_bindings(id,run_id);
ALTER TABLE model_request_attempts ADD CONSTRAINT model_attempt_workflow_binding
 FOREIGN KEY(binding_id,workflow_run_id) REFERENCES run_model_bindings(id,workflow_run_id);

-- The IssueRun read path keeps model_attempt_run_order; WorkflowRun reads are not opened yet, but
-- its rows still need an owner-ordered index once one is.
CREATE INDEX model_attempt_workflow_run_order
 ON model_request_attempts(tenant_id,workflow_run_id,started_at,id);
