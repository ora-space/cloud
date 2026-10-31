-- v2-server's whole schema. The server is temporary, so it applies this idempotently at
-- startup instead of keeping migrations.
CREATE TABLE IF NOT EXISTS users (
	id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	source       text NOT NULL,
	subject      text NOT NULL,
	display_name text NOT NULL,
	status       text NOT NULL DEFAULT 'active',
	version      bigint NOT NULL DEFAULT 1,
	created_at   timestamptz NOT NULL DEFAULT now(),
	deleted_at   timestamptz,
	UNIQUE (source, subject)
);

CREATE TABLE IF NOT EXISTS tenants (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	name       text NOT NULL UNIQUE,
	status     text NOT NULL DEFAULT 'active',
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS memberships (
	tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
	user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	role      text NOT NULL,
	PRIMARY KEY (tenant_id, user_id)
);

-- Only the SHA-256 of a session token is stored.
CREATE TABLE IF NOT EXISTS v2_schedule_configs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 user_id uuid NOT NULL REFERENCES users(id),
 name text NOT NULL,
 project_id text NOT NULL DEFAULT '',
 target jsonb NOT NULL,
 rule jsonb NOT NULL,
 enabled boolean NOT NULL DEFAULT true,
 version bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS v2_schedule_configs_owner ON v2_schedule_configs(tenant_id,user_id);

CREATE TABLE IF NOT EXISTS v2_schedule_occurrences (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 schedule_id uuid NOT NULL REFERENCES v2_schedule_configs(id),
 kind text NOT NULL CHECK(kind IN ('scheduled','manual')),
 trigger_key text NOT NULL,
 scheduled_at timestamptz NOT NULL,
 config_version bigint NOT NULL CHECK(config_version>0),
 snapshot jsonb NOT NULL,
 status text NOT NULL DEFAULT 'recorded',
 recorded_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(schedule_id,kind,trigger_key)
);
CREATE INDEX IF NOT EXISTS v2_schedule_occurrences_history ON v2_schedule_occurrences(schedule_id,recorded_at DESC,id DESC);

ALTER TABLE v2_schedule_occurrences ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '';
ALTER TABLE v2_schedule_occurrences ADD COLUMN IF NOT EXISTS execution_ref text NOT NULL DEFAULT '';
ALTER TABLE v2_schedule_occurrences ADD COLUMN IF NOT EXISTS released boolean NOT NULL DEFAULT true;
ALTER TABLE v2_schedule_occurrences ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1;
ALTER TABLE v2_schedule_occurrences DROP CONSTRAINT IF EXISTS v2_schedule_occurrences_status_check;
ALTER TABLE v2_schedule_occurrences ADD CONSTRAINT v2_schedule_occurrences_status_check CHECK(status IN ('recorded','starting','reconciling','running','cancelling','succeeded','failed','cancelled','interrupted','skipped'));
CREATE UNIQUE INDEX IF NOT EXISTS v2_schedule_active_occurrence ON v2_schedule_occurrences(schedule_id) WHERE NOT released;

CREATE TABLE IF NOT EXISTS sessions (
	token_hash bytea PRIMARY KEY,
	user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at timestamptz NOT NULL
);

-- Squads group people and Space Agents inside one workspace. Each tenant has exactly one
-- collaboration space in v2, so tenant_id is the space boundary.
CREATE TABLE IF NOT EXISTS v2_squads (
	id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id       uuid NOT NULL REFERENCES tenants (id),
	name            text NOT NULL,
	description     text NOT NULL DEFAULT '',
	charter         text NOT NULL DEFAULT '',
	leader_agent_id uuid,
	archived_at     timestamptz,
	version         bigint NOT NULL DEFAULT 1,
	created_by      uuid NOT NULL REFERENCES users (id),
	created_at      timestamptz NOT NULL DEFAULT now(),
	updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS v2_squads_tenant ON v2_squads (tenant_id, archived_at);

-- v2 has exactly one collaboration space per tenant, created with the demo tenant. It exists so
-- the contract's space and installed-plugin reads answer from real rows instead of a fixture.
CREATE TABLE IF NOT EXISTS v2_spaces (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid NOT NULL REFERENCES tenants (id),
	name        text NOT NULL,
	slug        text NOT NULL,
	description text NOT NULL DEFAULT '',
	created_by  uuid NOT NULL REFERENCES users (id),
	version     bigint NOT NULL DEFAULT 1,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now(),
	archived_at timestamptz,
	UNIQUE (tenant_id, slug)
);

-- The temporary marketplace snapshot. v2 does not run the marketplace Git sync loop; these rows
-- are seeded with the same PluginCatalogEntry shape Cloud serves from its PostgreSQL snapshot.
CREATE TABLE IF NOT EXISTS v2_plugin_catalog (
	source_namespace text NOT NULL,
	identifier       text NOT NULL,
	kind             text NOT NULL,
	version          text NOT NULL,
	data             jsonb NOT NULL,
	synced_at        timestamptz NOT NULL,
	PRIMARY KEY (source_namespace, identifier)
);

-- The v2 stand-in for the shared Space Agent directory: one row per plugin installed in a space.
-- Agent squad members reference the id of an agent row whose observed state is installed. The
-- columns mirror the contract's SpacePlugin so every installed market package can be read from
-- the same authoritative space selection state.
CREATE TABLE IF NOT EXISTS v2_space_agents (
	id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id        uuid NOT NULL REFERENCES tenants (id),
	space_id         uuid REFERENCES v2_spaces (id),
	name             text NOT NULL,
	status           text NOT NULL DEFAULT 'active',
	source_namespace text NOT NULL DEFAULT 'official',
	identifier       text NOT NULL DEFAULT '',
	kind             text NOT NULL DEFAULT 'agent',
	desired_state    text NOT NULL DEFAULT 'installed',
	desired_version  text NOT NULL DEFAULT '',
	observed_state   text NOT NULL DEFAULT 'installed',
	observed_version text NOT NULL DEFAULT '',
	install_error    text,
	requested_by_user_id uuid REFERENCES users (id),
	version          bigint NOT NULL DEFAULT 1,
	created_at       timestamptz NOT NULL DEFAULT now(),
	updated_at       timestamptz NOT NULL DEFAULT now()
);
-- Columns added after the first v2 schema; existing demo databases keep their rows. They run
-- before the indexes below, which reference them: on an existing table the CREATE TABLE above is
-- a no-op, so the columns exist only once these statements have run.
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS space_id uuid REFERENCES v2_spaces (id);
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS source_namespace text NOT NULL DEFAULT 'official';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS identifier text NOT NULL DEFAULT '';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'agent';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS desired_state text NOT NULL DEFAULT 'installed';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS desired_version text NOT NULL DEFAULT '';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS observed_state text NOT NULL DEFAULT 'installed';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS observed_version text NOT NULL DEFAULT '';
ALTER TABLE v2_space_agents ALTER COLUMN observed_version DROP NOT NULL;
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS install_error text;
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS requested_by_user_id uuid REFERENCES users (id);
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1;
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS v2_space_agents_tenant ON v2_space_agents (tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS v2_space_agents_plugin ON v2_space_agents (space_id, source_namespace, identifier);

-- Successful plugin mutations are replayed by Idempotency-Key. The request digest separates a
-- legitimate retry from reuse of the same key for a different selection.
CREATE TABLE IF NOT EXISTS v2_plugin_mutations (
	tenant_id       uuid NOT NULL REFERENCES tenants (id),
	user_id         uuid NOT NULL REFERENCES users (id),
	method          text NOT NULL,
	path            text NOT NULL,
	idempotency_key text NOT NULL,
	request_hash    bytea NOT NULL,
	response        jsonb NOT NULL,
	created_at      timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, user_id, method, path, idempotency_key)
);

-- Members are typed references: a human row points at a user, an agent row at a Space Agent.
-- Ending a membership keeps the row as history; rejoining inserts a new grant.
CREATE TABLE IF NOT EXISTS v2_squad_members (
	id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	squad_id       uuid NOT NULL REFERENCES v2_squads (id) ON DELETE CASCADE,
	member_type    text NOT NULL CHECK (member_type IN ('human', 'agent')),
	user_id        uuid REFERENCES users (id),
	space_agent_id uuid REFERENCES v2_space_agents (id),
	role           text NOT NULL DEFAULT '',
	maintainer     boolean NOT NULL DEFAULT false,
	joined_at      timestamptz NOT NULL DEFAULT now(),
	ended_at       timestamptz,
	CHECK ((member_type = 'human') = (user_id IS NOT NULL)),
	CHECK ((member_type = 'agent') = (space_agent_id IS NOT NULL)),
	CHECK (member_type = 'human' OR NOT maintainer)
);
-- At most one live grant per typed identity; ended grants stay as history.
CREATE UNIQUE INDEX IF NOT EXISTS v2_squad_members_active
	ON v2_squad_members (squad_id, member_type, COALESCE(user_id, space_agent_id))
	WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS v2_squad_members_squad ON v2_squad_members (squad_id);

-- Execution state belongs to the authenticated owner; projects are tenant-shared.
CREATE TABLE IF NOT EXISTS v2_execution_sessions (
 id text PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 user_id uuid NOT NULL REFERENCES users(id),
 data jsonb NOT NULL,
 ended_at timestamptz,
 event_sequence bigint NOT NULL DEFAULT 0,
 turn_position bigint NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS v2_execution_turns (
 id text PRIMARY KEY,
 session_id text NOT NULL REFERENCES v2_execution_sessions(id),
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 user_id uuid NOT NULL REFERENCES users(id),
 caller jsonb NOT NULL,
 position bigint NOT NULL,
 data jsonb NOT NULL,
 ended_at timestamptz,
 UNIQUE (tenant_id, user_id, caller),
 UNIQUE (session_id, position)
);
CREATE UNIQUE INDEX IF NOT EXISTS v2_execution_active_turn
 ON v2_execution_turns(session_id) WHERE ended_at IS NULL;
CREATE TABLE IF NOT EXISTS v2_execution_events (
 session_id text NOT NULL REFERENCES v2_execution_sessions(id),
 sequence bigint NOT NULL,
 data jsonb NOT NULL,
 PRIMARY KEY (session_id, sequence)
);
CREATE TABLE IF NOT EXISTS v2_projects (
 id text PRIMARY KEY,
 tenant_id uuid NOT NULL REFERENCES tenants(id),
 data jsonb NOT NULL
);

-- Chat conversations are page-owned: title, project and executor choice. Each conversation runs
-- in exactly one execution session (v2_chat_sessions), in a directory the execution service makes
-- for it; once that session ends the conversation is read-only. create_key is the Idempotency-Key
-- of the request that created the row, so a retried creation finds the same conversation and
-- replays the same attempt identity.
CREATE TABLE IF NOT EXISTS v2_chat_conversations (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
	user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	create_key  text NOT NULL,
	project_id  text NOT NULL,
	executor_id text NOT NULL,
	title       text NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now(),
	UNIQUE (tenant_id, user_id, create_key)
);
CREATE INDEX IF NOT EXISTS v2_chat_conversations_owner ON v2_chat_conversations (tenant_id, user_id);

-- end_reason records why the session ended (stopped, deleted, interrupted). It is written before
-- chat asks the port to end the session, so a page seeing session_ended can already read it; with
-- ended_at still unset it means the end is in progress, and it is cleared if ending fails.
CREATE TABLE IF NOT EXISTS v2_chat_sessions (
	conversation_id uuid PRIMARY KEY REFERENCES v2_chat_conversations (id) ON DELETE CASCADE,
	session_id      text NOT NULL UNIQUE,
	started_at      timestamptz NOT NULL DEFAULT now(),
	last_attempt_id text NOT NULL,
	last_turn_id    text NOT NULL,
	ended_at        timestamptz,
	end_reason      text NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS v2_chat_project_order (
	tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
	user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	project_ids text[] NOT NULL,
	PRIMARY KEY (tenant_id, user_id)
);

-- Tasks are v2's minimal work item: a title, a description, one project, at most one typed
-- assignee and a status the team moves by hand. Starting execution, comments and runs are a later
-- step; nothing in this table starts one. The assignee is a typed id without a foreign key, because
-- its target table depends on assignee_type; writes validate the target and reads resolve its name.
CREATE TABLE IF NOT EXISTS v2_tasks (
	id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id     uuid NOT NULL REFERENCES tenants (id),
	title         text NOT NULL,
	description   text NOT NULL DEFAULT '',
	project_id    text NOT NULL,
	assignee_type text,
	assignee_id   uuid,
	status        text NOT NULL DEFAULT 'pending',
	created_by    uuid NOT NULL REFERENCES users (id),
	version       bigint NOT NULL DEFAULT 1,
	created_at    timestamptz NOT NULL DEFAULT now(),
	updated_at    timestamptz NOT NULL DEFAULT now(),
	CHECK (assignee_type IS NULL OR assignee_type IN ('human', 'agent', 'squad')),
	CHECK ((assignee_type IS NULL) = (assignee_id IS NULL)),
	CHECK (status IN ('pending', 'running', 'review', 'done', 'cancelled'))
);
CREATE INDEX IF NOT EXISTS v2_tasks_tenant ON v2_tasks (tenant_id, updated_at DESC, id);
