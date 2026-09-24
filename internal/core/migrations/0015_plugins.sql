-- Plugin marketplace: cloud keeps the authoritative per-space plugin selection state and its own
-- catalog snapshot; the Node execution plane only downloads and runs what cloud fans out.
--
-- plugin_sources is deployment-global (not tenant-scoped): the configured marketplace is one
-- catalog the whole deployment serves. The default source keeps the 'official' namespace, which
-- matches the desktop marketplace identity model.
CREATE TABLE plugin_sources (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 namespace text NOT NULL UNIQUE,
 url text NOT NULL UNIQUE,
 branch text NOT NULL DEFAULT 'main',
 enabled boolean NOT NULL DEFAULT true,
 synced_at timestamptz,
 sync_error text,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Catalog snapshot produced by the sync loop. Reads never leave the database: the UI lists
-- plugins from this table only. One row per (source, identifier); a successful sync replaces the
-- whole source's rows in one transaction so readers never observe a half-synced catalog.
CREATE TABLE plugin_catalog_entries (
 source_namespace text NOT NULL REFERENCES plugin_sources(namespace),
 identifier text NOT NULL,
 title text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('workbench','agent','webview','skill','mcp','hook','pack','workflow')),
 version text NOT NULL,
 description text NOT NULL DEFAULT '',
 homepage text,
 license text,
 logo jsonb,
 url text,
 sha256 text,
 targets jsonb,
 pack_members jsonb,
 readme text,
 marketplace_visible boolean NOT NULL DEFAULT true,
 source_url text NOT NULL,
 indexed_at timestamptz NOT NULL,
 PRIMARY KEY (source_namespace, identifier)
);

-- Authoritative per-space plugin selection (desired) with the fan-out aggregate (observed). The
-- canonical plugin identity is the explicit (source_namespace, identifier) pair, never a JSON blob.
CREATE TABLE space_plugins (
 space_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 source_namespace text NOT NULL,
 identifier text NOT NULL,
 desired_state text NOT NULL CHECK (desired_state IN ('installed','removed')),
 desired_version text NOT NULL,
 observed_state text NOT NULL DEFAULT 'pending'
   CHECK (observed_state IN ('pending','installing','installed','failed','removing','removed')),
 observed_version text,
 install_error text,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (space_id, tenant_id) REFERENCES collab_workspaces(id, tenant_id),
 PRIMARY KEY (space_id, source_namespace, identifier)
);

-- Fan-out execution facts: one row per live runtime workspace an install/remove was expanded to.
-- Effect results write observed state back here; the space_plugins aggregate is recomputed from
-- these rows in the same transaction.
CREATE TABLE workspace_plugin_instances (
 workspace_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 owner_user_id uuid NOT NULL,
 project_id uuid NOT NULL,
 source_namespace text NOT NULL,
 identifier text NOT NULL,
 observed_state text NOT NULL DEFAULT 'pending'
   CHECK (observed_state IN ('pending','installing','installed','failed','removing','removed')),
 observed_version text,
 install_error text,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (workspace_id, tenant_id, owner_user_id)
   REFERENCES workspaces(id, tenant_id, owner_user_id),
 FOREIGN KEY (workspace_id, project_id) REFERENCES workspaces(id, project_id),
 PRIMARY KEY (workspace_id, source_namespace, identifier)
);

-- Widen the operation/effect kind and step enumerations for the plugin install/remove lifecycle.
-- The original CHECK constraints live in 0001; PostgreSQL names them table_column_check.
ALTER TABLE operations DROP CONSTRAINT operations_kind_check;
ALTER TABLE operations ADD CONSTRAINT operations_kind_check
 CHECK (kind IN ('create_project','create_workspace','start','stop','delete_workspace','delete_project','administrative_stop','install_plugin','remove_plugin'));
ALTER TABLE operations DROP CONSTRAINT operations_step_check;
ALTER TABLE operations ADD CONSTRAINT operations_step_check
 CHECK (step IN ('storage','worktree','sandbox','node','ready','quiesce','terminate','cleanup','storage_delete','plugin','done'));
ALTER TABLE external_effects DROP CONSTRAINT external_effects_kind_check;
ALTER TABLE external_effects ADD CONSTRAINT external_effects_kind_check
 CHECK (kind IN ('storage_ensure','worktree_ensure','sandbox_ensure','sandbox_terminate','worktree_delete','storage_delete','plugin_ensure','plugin_delete'));
