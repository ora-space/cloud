-- The ledger remains authoritative for D4/D5. A root intent owns a D8 responsibility only
-- when no ledger exists; a dependent delete intent never acquires a second responsibility.
CREATE TABLE agent_run_reclamation_intents (
 id uuid PRIMARY KEY,
 root_intent_id uuid REFERENCES agent_run_reclamation_intents(id),
 command_kind text NOT NULL CHECK(command_kind IN ('terminate','delete_data')),
 source text NOT NULL CHECK(source IN ('residual_ledger','legacy_d8')),
 tenant_id uuid NOT NULL REFERENCES tenants(id), project_id uuid NOT NULL REFERENCES projects(id),
 run_id uuid NOT NULL REFERENCES issue_runs(id), workspace_id uuid NOT NULL REFERENCES workspaces(id),
 runtime_generation bigint NOT NULL CHECK(runtime_generation>=0),
 reclaim_id uuid, legacy_delete_operation_id uuid REFERENCES operations(id),
 delete_operation_id uuid REFERENCES operations(id),
 state text NOT NULL DEFAULT 'registered' CHECK(state IN ('registered','terminating','succeeded')),
 disposition text CHECK(disposition IN ('terminated_data_retained','released','externally_confirmed')),
 control_epoch bigint NOT NULL, actor_subject text NOT NULL CHECK(length(actor_subject)>0),
 operator_label text NOT NULL DEFAULT '' CHECK(length(operator_label)<=200),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 2000),
 impact_confirmed boolean NOT NULL DEFAULT false, data_delete_confirmed boolean NOT NULL DEFAULT false,
 targets jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(targets)='array'),
 evidence jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(evidence)='array'),
 audit jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(audit)='array'),
 conflicted_at timestamptz,
 registered_at timestamptz NOT NULL DEFAULT clock_timestamp(), started_at timestamptz,
 completed_at timestamptz, last_checked_at timestamptz, version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 UNIQUE(id,tenant_id,project_id,run_id,workspace_id),
 FOREIGN KEY(root_intent_id,tenant_id,project_id,run_id,workspace_id)
 REFERENCES agent_run_reclamation_intents(id,tenant_id,project_id,run_id,workspace_id),
 CHECK((command_kind='terminate' AND root_intent_id IS NULL AND reclaim_id IS NOT NULL AND impact_confirmed)
    OR (command_kind='delete_data' AND root_intent_id IS NOT NULL AND reclaim_id IS NULL AND data_delete_confirmed)),
 CHECK((source='legacy_d8' AND legacy_delete_operation_id IS NOT NULL)
    OR (source='residual_ledger' AND legacy_delete_operation_id IS NULL)),
 CHECK((state='succeeded' AND completed_at IS NOT NULL AND disposition IS NOT NULL)
    OR (state<>'succeeded' AND completed_at IS NULL AND disposition IS NULL))
);
CREATE UNIQUE INDEX agent_run_reclamation_root_workspace ON agent_run_reclamation_intents(workspace_id)
 WHERE root_intent_id IS NULL;
CREATE UNIQUE INDEX agent_run_reclamation_root_responsibility ON agent_run_reclamation_intents(reclaim_id)
 WHERE reclaim_id IS NOT NULL;
CREATE UNIQUE INDEX agent_run_reclamation_one_delete ON agent_run_reclamation_intents(root_intent_id)
 WHERE command_kind='delete_data';
CREATE UNIQUE INDEX agent_run_reclamation_delete_operation ON agent_run_reclamation_intents(delete_operation_id)
 WHERE command_kind='delete_data' AND delete_operation_id IS NOT NULL;
CREATE INDEX agent_run_reclamation_scope ON agent_run_reclamation_intents(tenant_id,project_id,run_id,workspace_id);
CREATE INDEX agent_run_reclamation_pending ON agent_run_reclamation_intents(last_checked_at NULLS FIRST,registered_at,id)
 WHERE state<>'succeeded';

-- sandbox_id deliberately has no sandbox_instances FK: an ensure plan is already a target
-- before an allocating sandbox row has arrived. Its original identity is verified in the transaction.
CREATE TABLE agent_run_reclamation_targets (
 id uuid PRIMARY KEY, root_intent_id uuid NOT NULL REFERENCES agent_run_reclamation_intents(id),
 sandbox_id uuid NOT NULL, generation bigint NOT NULL CHECK(generation>=0),
 state text NOT NULL DEFAULT 'planned' CHECK(state IN ('planned','succeeded')),
 request jsonb NOT NULL CHECK(jsonb_typeof(request)='object'),
 result jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(result)='object'),
 confirmed_epoch bigint, confirmed_at timestamptz,
 UNIQUE(root_intent_id,sandbox_id),
 CHECK((state='planned' AND confirmed_at IS NULL)
    OR (state='succeeded' AND confirmed_at IS NOT NULL AND confirmed_epoch IS NOT NULL
        AND result @> '{"terminated":true,"lateEnsureFenced":true}'::jsonb))
);
CREATE TABLE agent_run_reclamation_command_receipts (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL REFERENCES tenants(id),
 idempotency_key text NOT NULL CHECK(length(idempotency_key) BETWEEN 1 AND 200),
 command_input jsonb NOT NULL CHECK(jsonb_typeof(command_input)='object'),
 intent_id uuid NOT NULL REFERENCES agent_run_reclamation_intents(id),
 actor_subject text NOT NULL, result jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(result)='object'),
 audit jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(audit)='array'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), UNIQUE(tenant_id,idempotency_key)
);
