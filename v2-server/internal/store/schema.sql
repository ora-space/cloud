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

-- The v2 stand-in for the shared Space Agent directory: one row per plugin installed in a space.
-- Agent squad members reference the id of a row whose observed state is installed. The columns
-- mirror the contract's SpacePlugin so the read route can answer without a translation layer.
CREATE TABLE IF NOT EXISTS v2_space_agents (
	id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id        uuid NOT NULL REFERENCES tenants (id),
	space_id         uuid REFERENCES v2_spaces (id),
	name             text NOT NULL,
	status           text NOT NULL DEFAULT 'active',
	source_namespace text NOT NULL DEFAULT 'official',
	identifier       text NOT NULL DEFAULT '',
	desired_state    text NOT NULL DEFAULT 'installed',
	desired_version  text NOT NULL DEFAULT '',
	observed_state   text NOT NULL DEFAULT 'installed',
	observed_version text NOT NULL DEFAULT '',
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
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS desired_state text NOT NULL DEFAULT 'installed';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS desired_version text NOT NULL DEFAULT '';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS observed_state text NOT NULL DEFAULT 'installed';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS observed_version text NOT NULL DEFAULT '';
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 1;
ALTER TABLE v2_space_agents ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS v2_space_agents_tenant ON v2_space_agents (tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS v2_space_agents_plugin ON v2_space_agents (space_id, source_namespace, identifier);

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

-- Workflow definitions follow desktop's authoring model instead of Cloud's: every definition owns
-- exactly one mutable draft snapshot (version 'draft') plus immutable published snapshots addressed
-- by a caller-chosen version, and the workflow row points at the version new runs freeze. The graph
-- is stored verbatim as text so the editor receives exactly the document it saved; parsing and
-- participation stay in the workflow domain, not in a JSON column. Published snapshots are frozen
-- before they are stored, so the editor's unsaved draft can be an unfinished canvas.
CREATE TABLE IF NOT EXISTS v2_workflows (
	id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id             uuid NOT NULL REFERENCES tenants (id),
	namespace             text NOT NULL DEFAULT 'local',
	name                  text NOT NULL,
	published_snapshot_id uuid,
	created_at            timestamptz NOT NULL DEFAULT now(),
	updated_at            timestamptz NOT NULL DEFAULT now(),
	deleted_at            timestamptz
);
-- Desktop refuses duplicate live names case-insensitively within a namespace; v2 has one namespace
-- per tenant, so the tenant is the namespace boundary.
CREATE UNIQUE INDEX IF NOT EXISTS v2_workflows_live_name
	ON v2_workflows (tenant_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS v2_workflow_snapshots (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid NOT NULL REFERENCES tenants (id),
	workflow_id uuid NOT NULL REFERENCES v2_workflows (id),
	version     text NOT NULL,
	graph       text NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz,
	deleted_at  timestamptz
);
-- A deleted version may be republished under the same spelling, as in desktop. The partial draft
-- index keeps the single mutable snapshot unambiguous even under concurrent creation.
CREATE UNIQUE INDEX IF NOT EXISTS v2_workflow_snapshots_live_version
	ON v2_workflow_snapshots (workflow_id, version) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS v2_workflow_snapshots_one_draft
	ON v2_workflow_snapshots (workflow_id) WHERE version = 'draft' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS v2_workflow_snapshots_history
	ON v2_workflow_snapshots (workflow_id, created_at DESC, id DESC) WHERE deleted_at IS NULL;
