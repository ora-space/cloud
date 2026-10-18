-- Git Server as the Project's code hub (ADR cloud/git-server, Project repository as the code hub):
-- a Project's external repository moves from Project columns to its Binding; the Binding's current
-- snapshot and its sync and publish Operations get their final shape here, once, so the internal
-- and external lines only add columns. Phase one has exactly one Binding per Project.
CREATE TABLE repository_bindings (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 project_id uuid NOT NULL,
 repository_url text NOT NULL CHECK(length(repository_url) BETWEEN 1 AND 2048),
 branch text NOT NULL CHECK(length(branch) BETWEEN 1 AND 200),
 credential_ref_id uuid,
 repository_credential_ref_id uuid,
 -- A publish that changed the external repository asks for a sync once nothing else is running.
 resync_requested boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(project_id,tenant_id) REFERENCES projects(id,tenant_id),
 FOREIGN KEY(credential_ref_id,tenant_id) REFERENCES credential_refs(id,tenant_id),
 FOREIGN KEY(repository_credential_ref_id,tenant_id) REFERENCES credential_refs(id,tenant_id),
 CONSTRAINT repository_binding_per_project UNIQUE(project_id),
 UNIQUE(id,project_id),
 UNIQUE(id,tenant_id)
);

-- Projects created before this migration keep their repository as their Binding.
INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch,credential_ref_id,repository_credential_ref_id,created_at)
 SELECT gen_random_uuid(),tenant_id,id,repository_url,default_branch,credential_ref_id,repository_credential_ref_id,created_at FROM projects;

ALTER TABLE projects DROP COLUMN repository_url, DROP COLUMN default_branch, DROP COLUMN credential_ref_id, DROP COLUMN repository_credential_ref_id;

-- States and terminal states are the contract's (D5 item 3); later lines add columns, never states.
CREATE TABLE repository_operations (
 id uuid PRIMARY KEY,
 type text NOT NULL CHECK(type IN ('repository_sync','repository_publish')),
 tenant_id uuid NOT NULL,
 project_id uuid NOT NULL,
 binding_id uuid NOT NULL,
 state text NOT NULL,
 trigger text NOT NULL CHECK(trigger ~ '^[a-z][a-z_]{0,63}$'),
 error_code text CHECK(error_code ~ '^[a-z][a-z_]{0,63}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 FOREIGN KEY(project_id,tenant_id) REFERENCES projects(id,tenant_id),
 FOREIGN KEY(binding_id,project_id) REFERENCES repository_bindings(id,project_id),
 UNIQUE(id,binding_id),
 CONSTRAINT repository_operation_state CHECK(
  type='repository_sync' AND state IN ('pending','leased','running','succeeded','failed','superseded','cancelled')
  OR type='repository_publish' AND state IN ('awaiting_approval','pending','leased','running','uncertain','succeeded','conflict','rejected','failed','cancelled')),
 CONSTRAINT repository_operation_finished CHECK((finished_at IS NULL) = (state IN ('awaiting_approval','pending','leased','running','uncertain')))
);
-- At most one unfinished sync per Binding: a second trigger finds the first instead of racing it.
CREATE UNIQUE INDEX repository_sync_unfinished ON repository_operations(binding_id) WHERE type='repository_sync' AND state IN ('pending','leased','running');
CREATE INDEX repository_operation_project ON repository_operations(project_id,state);

-- The Binding's current snapshot: the latest sync result Cloud verified on the Git Server. It may
-- lag the tracking ref, never lead it, and always names an immutable snapshot ref.
CREATE TABLE repository_snapshots (
 binding_id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 project_id uuid NOT NULL,
 commit_oid text NOT NULL CHECK(commit_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$' AND commit_oid !~ '^0+$'),
 snapshot_ref text NOT NULL,
 sync_operation_id uuid NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(binding_id,project_id) REFERENCES repository_bindings(id,project_id),
 FOREIGN KEY(project_id,tenant_id) REFERENCES projects(id,tenant_id),
 FOREIGN KEY(sync_operation_id,binding_id) REFERENCES repository_operations(id,binding_id),
 CONSTRAINT repository_snapshot_ref CHECK(snapshot_ref = 'refs/ora/upstreams/' || binding_id::text || '/snapshots/' || commit_oid)
);
