-- Executor slots reach the runtime (specs decisions 20261015-executor-slots and
-- 20261015-executor-slot-mappings). A WorkflowRun freezes one run_model_bindings row per executor
-- slot its graph references, while plain OpenCode IssueRuns keep their single binding under the
-- reserved 'default' slot. The binding owner becomes a discriminator pair — exactly one of
-- run_id / workflow_run_id — and the previous whole-table UNIQUE(run_id) splits into one
-- (run, slot) uniqueness per owner, so a run may carry several bindings at once. Personal
-- executor-slot mappings (slot name -> executor) are the invoker-owned resolution table the
-- creation gate reads; 'default' is deliberately excluded there because it IS the personal
-- default model (personal_model_defaults), never a stored mapping.

ALTER TABLE run_model_bindings ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE run_model_bindings ADD COLUMN workflow_run_id uuid REFERENCES workflow_runs(id);
ALTER TABLE run_model_bindings ADD COLUMN slot_name text NOT NULL DEFAULT 'default';
ALTER TABLE run_model_bindings DROP CONSTRAINT run_model_bindings_run_id_key;
ALTER TABLE run_model_bindings ADD CONSTRAINT run_model_binding_owner_exclusive CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL));
CREATE UNIQUE INDEX run_model_bindings_run_slot_key ON run_model_bindings(run_id, slot_name) WHERE run_id IS NOT NULL;
CREATE UNIQUE INDEX run_model_bindings_workflow_run_slot_key ON run_model_bindings(workflow_run_id, slot_name) WHERE workflow_run_id IS NOT NULL;

CREATE TABLE executor_slot_mappings (
  user_id uuid NOT NULL REFERENCES users(id),
  slot_name text NOT NULL CHECK(slot_name <> 'default' AND slot_name ~ '^[a-z][a-z0-9_-]{0,63}$'),
  agent_cli text NOT NULL CHECK(agent_cli = 'official/ora-space.opencode'),
  connection_id uuid NOT NULL,
  model_id text NOT NULL,
  version bigint NOT NULL CHECK(version >= 1),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, slot_name),
  FOREIGN KEY (connection_id, user_id) REFERENCES personal_model_connections(id, user_id)
);
