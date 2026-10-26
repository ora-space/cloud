-- A tombstone keeps revisions monotonic across clear/recreate cycles. Plugin identity is
-- canonical and user-owned; a space supplies editing context, never ownership.
CREATE TABLE personal_agent_configurations (
 user_id uuid NOT NULL REFERENCES users(id),
 plugin_id text NOT NULL CHECK(length(plugin_id) BETWEEN 3 AND 300),
 declaration_fingerprint text NOT NULL CHECK(declaration_fingerprint ~ '^[a-f0-9]{64}$'),
 settings jsonb NOT NULL CHECK(jsonb_typeof(settings)='object'),
 connection_id uuid,
 model_id text,
 revision bigint NOT NULL CHECK(revision > 0),
 cleared boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(user_id,plugin_id),
 FOREIGN KEY(connection_id,user_id) REFERENCES personal_model_connections(id,user_id),
 CHECK((connection_id IS NULL) = (model_id IS NULL))
);

-- Private execution facts are separate from the public issue_runs.input document.
-- No history is backfilled; old OpenCode bindings retain their existing meaning.
CREATE TABLE run_agent_configurations (
 run_id uuid PRIMARY KEY REFERENCES issue_runs(id),
 plugin_id text NOT NULL,
 plugin_version text NOT NULL,
 target text NOT NULL,
 package_sha256 text NOT NULL CHECK(package_sha256 ~ '^[a-f0-9]{64}$'),
 declaration_schema_version integer NOT NULL CHECK(declaration_schema_version > 0),
 declaration_fingerprint text NOT NULL CHECK(declaration_fingerprint ~ '^[a-f0-9]{64}$'),
 adapter_contract_version integer NOT NULL CHECK(adapter_contract_version > 0),
 configuration_revision bigint NOT NULL CHECK(configuration_revision >= 0),
 selection_revision bigint NOT NULL CHECK(selection_revision >= 0),
 settings jsonb NOT NULL CHECK(jsonb_typeof(settings)='object'),
 release jsonb NOT NULL CHECK(jsonb_typeof(release)='object'),
 created_at timestamptz NOT NULL DEFAULT now()
);
