-- Workflow plugin asset import: Cloud downloads the selected universal release of a
-- kind='workflow' plugin and imports each assets/workflows/*.json document into the tenant's
-- workflow library as a normal workflow plus a published snapshot. The imported copies are user
-- data; these rows only record provenance, idempotency and per-document outcomes.
--
-- skip_key is (space_id, source_namespace, identifier, document_path) on workflow_asset_documents:
-- a successful import permanently occupies the path, across package versions. Failed documents do
-- not occupy it, so the next trigger retries them. Package metadata is informational; it never
-- participates in the skip decision.
CREATE TABLE workflow_asset_imports (
 space_id uuid NOT NULL,
 tenant_id uuid NOT NULL,
 source_namespace text NOT NULL,
 identifier text NOT NULL,
 -- requested_revision advances on every administrator install/reinstall; the import worker claims
 -- one revision at a time so a reinstall during a download is not lost.
 requested_revision bigint NOT NULL DEFAULT 1 CHECK (requested_revision > 0),
 attempted_revision bigint NOT NULL DEFAULT 0 CHECK (attempted_revision >= 0),
 package_version text NOT NULL DEFAULT '',
 package_sha256 text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','importing','completed','failed')),
 failure_code text,
 lease_until timestamptz,
 attempted_at timestamptz,
 completed_at timestamptz,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (space_id, tenant_id) REFERENCES collab_workspaces(id, tenant_id),
 PRIMARY KEY (space_id, source_namespace, identifier)
);

-- One row per document path considered for a (space, plugin). outcome='imported' is the only
-- state that occupies skip_key; 'failed' rows are refreshed by the next attempt.
CREATE TABLE workflow_asset_documents (
 space_id uuid NOT NULL,
 source_namespace text NOT NULL,
 identifier text NOT NULL,
 document_path text NOT NULL,
 package_version text NOT NULL,
 package_sha256 text NOT NULL,
 outcome text NOT NULL CHECK (outcome IN ('imported','failed')),
 workflow_id uuid REFERENCES workflows(id),
 snapshot_id uuid REFERENCES workflow_snapshots(id),
 snapshot_version bigint CHECK (snapshot_version > 0),
 failure_code text,
 failure_detail jsonb,
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (space_id, source_namespace, identifier)
   REFERENCES workflow_asset_imports(space_id, source_namespace, identifier) ON DELETE CASCADE,
 PRIMARY KEY (space_id, source_namespace, identifier, document_path)
);

-- The import worker scans for due attempts; both predicates are low-cardinality and the table is
-- bounded by installed workflow plugins.
CREATE INDEX workflow_asset_imports_due ON workflow_asset_imports(state, lease_until);
