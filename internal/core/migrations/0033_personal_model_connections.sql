-- Personal model metadata is Cloud-owned. Only model-gateway writes opaque encrypted payloads;
-- immutable credential references and run bindings preserve configuration across later edits.
ALTER TABLE node_instances ADD COLUMN model_proxy boolean NOT NULL DEFAULT false;
CREATE TABLE personal_model_credentials (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 ciphertext text NOT NULL CHECK (length(ciphertext) BETWEEN 32 AND 32768),
 key_id text NOT NULL CHECK (length(key_id) BETWEEN 1 AND 200),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(id,user_id)
);
CREATE TABLE personal_model_connections (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
 protocol text NOT NULL CHECK (protocol IN ('openai-completions','anthropic-messages')),
 base_url text NOT NULL CHECK (length(base_url) BETWEEN 1 AND 2048),
 auth_mode text NOT NULL CHECK (auth_mode IN ('bearer','x-api-key')),
 models jsonb NOT NULL CHECK (jsonb_typeof(models)='array' AND jsonb_array_length(models) BETWEEN 1 AND 100),
 enabled boolean NOT NULL DEFAULT true,
 credential_id uuid,
 version bigint NOT NULL DEFAULT 1 CHECK (version>=1),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 deleted_at timestamptz,
 UNIQUE(id,user_id),
 FOREIGN KEY(credential_id,user_id) REFERENCES personal_model_credentials(id,user_id)
);
CREATE TABLE personal_model_defaults (
 user_id uuid PRIMARY KEY REFERENCES users(id),
 connection_id uuid NOT NULL,
 model_id text NOT NULL CHECK(length(model_id) BETWEEN 1 AND 500),
 version bigint NOT NULL DEFAULT 1 CHECK(version>=1),
 FOREIGN KEY(connection_id,user_id) REFERENCES personal_model_connections(id,user_id)
);
-- This scope exists before a user joins any tenant; it therefore cannot use tenant idempotency.
CREATE TABLE personal_model_idempotency (
 user_id uuid NOT NULL REFERENCES users(id),
 key text NOT NULL CHECK(length(key) BETWEEN 1 AND 200),
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 status integer NOT NULL,
 PRIMARY KEY(user_id,key)
);
CREATE TABLE run_model_bindings (
 id uuid PRIMARY KEY,
 run_id uuid NOT NULL UNIQUE REFERENCES issue_runs(id),
 user_id uuid NOT NULL REFERENCES users(id),
 connection_id uuid NOT NULL,
 connection_version bigint NOT NULL CHECK(connection_version>=1),
 credential_id uuid NOT NULL,
 connection_name text NOT NULL,
 protocol text NOT NULL CHECK(protocol IN ('openai-completions','anthropic-messages')),
 base_url text NOT NULL,
 auth_mode text NOT NULL CHECK(auth_mode IN ('bearer','x-api-key')),
 model jsonb NOT NULL CHECK(jsonb_typeof(model)='object'),
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(connection_id,user_id) REFERENCES personal_model_connections(id,user_id),
 FOREIGN KEY(credential_id,user_id) REFERENCES personal_model_credentials(id,user_id)
);
CREATE TABLE model_access_grants (
 id uuid PRIMARY KEY,
 binding_id uuid NOT NULL REFERENCES run_model_bindings(id),
 execution_id text NOT NULL REFERENCES node_executions(execution_id),
 workspace_id uuid NOT NULL REFERENCES workspaces(id),
 runtime_generation bigint NOT NULL CHECK(runtime_generation>0),
 token_digest text NOT NULL UNIQUE CHECK(token_digest ~ '^[a-f0-9]{64}$'),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '15 minutes',
 revoked_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX model_access_grants_binding ON model_access_grants(binding_id) WHERE revoked_at IS NULL;
