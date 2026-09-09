CREATE TABLE users (
 id uuid PRIMARY KEY, display_name text NOT NULL DEFAULT '', status text NOT NULL CHECK(status IN ('active','disabled')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz
);
CREATE TABLE user_identities (
 user_id uuid NOT NULL REFERENCES users(id), source text NOT NULL CHECK(length(source) BETWEEN 1 AND 128),
 subject text NOT NULL CHECK(length(subject) BETWEEN 1 AND 512), created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(source,subject)
);
CREATE TABLE tenants (
 id uuid PRIMARY KEY, name text NOT NULL CHECK(length(name) BETWEEN 1 AND 200), status text NOT NULL CHECK(status IN ('active','disabled')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz
);
CREATE TABLE tenant_memberships (
 tenant_id uuid NOT NULL REFERENCES tenants(id), user_id uuid NOT NULL REFERENCES users(id), role text NOT NULL CHECK(role IN ('admin','member')),
 status text NOT NULL CHECK(status IN ('active','disabled')), version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,user_id)
);
CREATE TABLE credential_refs (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_user_id uuid NOT NULL, purpose text NOT NULL CHECK(purpose='git'), secret_ref text NOT NULL CHECK(length(secret_ref)>0),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 FOREIGN KEY(tenant_id,owner_user_id) REFERENCES tenant_memberships(tenant_id,user_id), UNIQUE(id,tenant_id,owner_user_id)
);
CREATE TABLE projects (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_user_id uuid NOT NULL, name text NOT NULL CHECK(length(name) BETWEEN 1 AND 200), repository_url text NOT NULL CHECK(length(repository_url)>0),
 default_branch text NOT NULL, credential_ref_id uuid, lifecycle text NOT NULL CHECK(lifecycle IN ('provisioning','active','deleting','deleted')),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 FOREIGN KEY(tenant_id,owner_user_id) REFERENCES tenant_memberships(tenant_id,user_id),
 FOREIGN KEY(credential_ref_id,tenant_id,owner_user_id) REFERENCES credential_refs(id,tenant_id,owner_user_id), UNIQUE(id,tenant_id,owner_user_id)
);
CREATE TABLE project_storage (
 project_id uuid PRIMARY KEY REFERENCES projects(id), substrate_storage_id text UNIQUE, storage_profile text NOT NULL DEFAULT 'rwx-v1', layout_version integer NOT NULL DEFAULT 1 CHECK(layout_version=1),
 observed_state text NOT NULL CHECK(observed_state IN ('pending','ready','deleting','deleted')), version bigint NOT NULL DEFAULT 1 CHECK(version>0)
);
CREATE TABLE workspaces (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, owner_user_id uuid NOT NULL, project_id uuid NOT NULL, kind text NOT NULL CHECK(kind IN ('main','isolated')),
 desired_state text NOT NULL CHECK(desired_state IN ('running','stopped','deleted')),
 observed_state text NOT NULL CHECK(observed_state IN ('provisioning','starting','ready','stopping','stopped','unavailable','deleting','deleted')),
 runtime_generation bigint NOT NULL DEFAULT 0 CHECK(runtime_generation>=0), version bigint NOT NULL DEFAULT 1 CHECK(version>0), admission_open boolean NOT NULL DEFAULT false,
 admission_epoch bigint NOT NULL DEFAULT 0 CHECK(admission_epoch>=0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz,
 FOREIGN KEY(project_id,tenant_id,owner_user_id) REFERENCES projects(id,tenant_id,owner_user_id), UNIQUE(id,tenant_id,owner_user_id), UNIQUE(id,project_id),
 CHECK(NOT admission_open OR (desired_state='running' AND observed_state='ready' AND deleted_at IS NULL))
);
CREATE UNIQUE INDEX one_main ON workspaces(project_id) WHERE kind='main' AND deleted_at IS NULL;
CREATE INDEX project_list ON projects(tenant_id,owner_user_id,id);
CREATE INDEX workspace_list ON workspaces(tenant_id,owner_user_id,project_id,id);
CREATE TABLE workspace_worktrees (
 workspace_id uuid PRIMARY KEY REFERENCES workspaces(id), relative_path text NOT NULL, branch_name text NOT NULL, requested_ref text NOT NULL, base_commit_id text CHECK(base_commit_id ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
 provisioning_state text NOT NULL CHECK(provisioning_state IN ('pending','ready','deleting','deleted')),
 CHECK(relative_path='workspaces/' || workspace_id::text || '/checkout'), CHECK(branch_name='ora/' || workspace_id::text)
);
CREATE TABLE tasks (
 id uuid PRIMARY KEY, workspace_id uuid NOT NULL UNIQUE REFERENCES workspaces(id), title text NOT NULL CHECK(length(title) BETWEEN 1 AND 200),
 version bigint NOT NULL DEFAULT 1 CHECK(version>0), created_at timestamptz NOT NULL DEFAULT now(), deleted_at timestamptz
);
CREATE TABLE sandbox_instances (
 id uuid PRIMARY KEY, workspace_id uuid NOT NULL REFERENCES workspaces(id), generation bigint NOT NULL CHECK(generation>0), substrate_sandbox_id text UNIQUE,
 observed_state text NOT NULL CHECK(observed_state IN ('allocating','starting','running','terminating','terminated')),
 created_at timestamptz NOT NULL DEFAULT now(), terminated_at timestamptz, UNIQUE(workspace_id,generation), UNIQUE(id,workspace_id,generation),
 CHECK((observed_state='terminated')=(terminated_at IS NOT NULL))
);
CREATE UNIQUE INDEX one_live_sandbox ON sandbox_instances(workspace_id) WHERE terminated_at IS NULL;
CREATE TABLE node_instances (
 id uuid PRIMARY KEY, sandbox_instance_id uuid NOT NULL REFERENCES sandbox_instances(id), service_subject text NOT NULL,
 connection_state text NOT NULL CHECK(connection_state IN ('connected','disconnected','ended')), protocol_version integer NOT NULL CHECK(protocol_version=1),
 initialized boolean NOT NULL DEFAULT false, last_seen_at timestamptz NOT NULL DEFAULT now(), ended_at timestamptz,
 idle_admission_epoch bigint, version bigint NOT NULL DEFAULT 1 CHECK(version>0)
);
CREATE UNIQUE INDEX one_live_node ON node_instances(sandbox_instance_id) WHERE ended_at IS NULL;
CREATE TABLE controller_leases (
 name text PRIMARY KEY CHECK(name='global'), holder_id text NOT NULL, epoch bigint NOT NULL CHECK(epoch>0), expires_at timestamptz NOT NULL
);
CREATE TABLE operations (
 id uuid PRIMARY KEY, tenant_id uuid NOT NULL, actor_user_id uuid NOT NULL, project_id uuid NOT NULL REFERENCES projects(id), workspace_id uuid REFERENCES workspaces(id),
 kind text NOT NULL CHECK(kind IN ('create_project','create_workspace','start','stop','delete_workspace','delete_project','administrative_stop')),
 state text NOT NULL CHECK(state IN ('queued','running','retry_wait','blocked','succeeded','failed')), step text NOT NULL CHECK(step IN ('storage','worktree','sandbox','node','ready','quiesce','terminate','cleanup','storage_delete','done')),
 request jsonb NOT NULL CHECK(jsonb_typeof(request)='object'), result jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(result)='object'), error_code text,
 idempotency_key text NOT NULL, request_hash text NOT NULL, controller_epoch bigint, retry_at timestamptz, version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY(tenant_id,actor_user_id) REFERENCES tenant_memberships(tenant_id,user_id),
 FOREIGN KEY(workspace_id,project_id) REFERENCES workspaces(id,project_id)
);
CREATE UNIQUE INDEX one_project_operation ON operations(project_id) WHERE state IN ('queued','running','retry_wait','blocked');
CREATE TABLE idempotency_records (
 tenant_id uuid NOT NULL, user_id uuid NOT NULL, key text NOT NULL CHECK(length(key) BETWEEN 1 AND 200), request_hash text NOT NULL,
 response jsonb NOT NULL CHECK(jsonb_typeof(response)='object'), status integer NOT NULL CHECK(status BETWEEN 200 AND 299), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(tenant_id,user_id,key), FOREIGN KEY(tenant_id,user_id) REFERENCES tenant_memberships(tenant_id,user_id)
);
-- Unknown activity remains active until the bound Node explicitly finishes it.
CREATE TABLE execution_tickets (
 id uuid PRIMARY KEY, workspace_id uuid NOT NULL REFERENCES workspaces(id), node_instance_id uuid NOT NULL REFERENCES node_instances(id), actor_user_id uuid NOT NULL REFERENCES users(id),
 admission_epoch bigint NOT NULL, kind text NOT NULL CHECK(kind IN ('task','interaction')), state text NOT NULL CHECK(state IN ('active','finished')),
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz
);
-- Write-ahead effects survive response loss and lease takeover.
CREATE TABLE external_effects (
 id uuid PRIMARY KEY, operation_id uuid NOT NULL REFERENCES operations(id), project_id uuid NOT NULL REFERENCES projects(id), workspace_id uuid REFERENCES workspaces(id),
 kind text NOT NULL CHECK(kind IN ('storage_ensure','worktree_ensure','sandbox_ensure','sandbox_terminate','worktree_delete','storage_delete')),
 state text NOT NULL CHECK(state IN ('planned','running','succeeded','failed')), external_id text, result jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(result)='object'),
 reconciled_epoch bigint NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(operation_id,kind,workspace_id)
);
CREATE UNIQUE INDEX singleton_effect ON external_effects(operation_id,kind) WHERE workspace_id IS NULL;
CREATE FUNCTION check_project_main() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE pid uuid; live boolean; n integer;
BEGIN
 IF TG_TABLE_NAME='projects' THEN pid:=COALESCE(NEW.id,OLD.id); ELSE pid:=COALESCE(NEW.project_id,OLD.project_id); END IF;
 SELECT deleted_at IS NULL INTO live FROM projects WHERE id=pid;
 IF live THEN
   SELECT count(*) INTO n FROM workspaces WHERE project_id=pid AND kind='main' AND deleted_at IS NULL;
   IF n<>1 THEN RAISE EXCEPTION 'live project requires exactly one main workspace' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER project_main AFTER INSERT OR UPDATE OR DELETE ON projects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_project_main();
CREATE CONSTRAINT TRIGGER workspace_main AFTER INSERT OR UPDATE OR DELETE ON workspaces DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_project_main();
CREATE FUNCTION check_last_admin() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE tid uuid;
BEGIN
 tid:=COALESCE(NEW.tenant_id,OLD.tenant_id);
 PERFORM 1 FROM tenants WHERE id=tid FOR UPDATE;
 IF EXISTS(SELECT 1 FROM tenants WHERE id=tid AND status='active') AND NOT EXISTS(
 SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=tid AND m.role='admin' AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)
 THEN RAISE EXCEPTION 'last active administrator' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER last_admin AFTER INSERT OR UPDATE OR DELETE ON tenant_memberships DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_last_admin();
