-- Runtime Workspaces follow the desktop Node (specs decisions/cloud/operation/0-workspace-runtime-
-- follows-desktop-node.md): create goes sandbox -> node -> clone, cleanup deletes one Workspace's data,
-- and the Project shared volume, bare repository and linked worktrees are retired. Nothing here drops
-- or rewrites project_storage / workspace_worktrees rows: they stay as history and audit evidence.

-- The clone step replaces storage/worktree; the retired step names stay valid for history only.
ALTER TABLE operations DROP CONSTRAINT operations_step_check;
ALTER TABLE operations ADD CONSTRAINT operations_step_check
 CHECK (step IN ('storage','worktree','sandbox','node','clone','ready','quiesce','terminate','cleanup','storage_delete','plugin','done'));
ALTER TABLE external_effects DROP CONSTRAINT external_effects_kind_check;
ALTER TABLE external_effects ADD CONSTRAINT external_effects_kind_check
 CHECK (kind IN ('storage_ensure','worktree_ensure','sandbox_ensure','sandbox_terminate','worktree_delete','storage_delete','workspace_data_delete','plugin_ensure','plugin_delete'));

-- In-flight operations still in a retired step have no sandbox and no Node activity, so they end
-- here instead of waiting on an effect no Controller will plan again. A project deletion already
-- past its worktree cleanup returns to cleanup, which now deletes each Workspace's data instead.
UPDATE operations SET state='failed',error_code='lifecycle_flow_retired',version=version+1,updated_at=now()
 WHERE step IN ('storage','worktree') AND state IN ('queued','running','retry_wait','blocked');
UPDATE operations SET step='cleanup',version=version+1,updated_at=now()
 WHERE step='storage_delete' AND state IN ('queued','running','retry_wait','blocked');

-- Retired steps, effects and storage rows are history: the database refuses to create new ones.
CREATE FUNCTION reject_retired_lifecycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='operations' THEN
   IF NEW.step IN ('storage','worktree','storage_delete') AND (TG_OP='INSERT' OR OLD.step IS DISTINCT FROM NEW.step) THEN
     RAISE EXCEPTION 'retired lifecycle step %', NEW.step USING ERRCODE='23514';
   END IF;
 ELSIF TG_TABLE_NAME='external_effects' THEN
   IF NEW.kind IN ('storage_ensure','worktree_ensure','worktree_delete','storage_delete') THEN
     RAISE EXCEPTION 'retired effect kind %', NEW.kind USING ERRCODE='23514';
   END IF;
 ELSE
   RAISE EXCEPTION 'retired lifecycle table %', TG_TABLE_NAME USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retired_operation_step BEFORE INSERT OR UPDATE OF step ON operations FOR EACH ROW EXECUTE FUNCTION reject_retired_lifecycle();
CREATE TRIGGER retired_effect_kind BEFORE INSERT ON external_effects FOR EACH ROW EXECUTE FUNCTION reject_retired_lifecycle();
CREATE TRIGGER retired_project_storage BEFORE INSERT ON project_storage FOR EACH ROW EXECUTE FUNCTION reject_retired_lifecycle();
CREATE TRIGGER retired_workspace_worktree BEFORE INSERT ON workspace_worktrees FOR EACH ROW EXECUTE FUNCTION reject_retired_lifecycle();

-- The Workspace itself now owns the ref its Node clones and the commit that clone produced. The
-- requested ref is copied from the Workspace's worktree row, or from its Project's default branch
-- when it has none; the old worktree's commit is not copied: it lives on a retired shared volume,
-- not in the Workspace's own data. Every ALTER on workspaces precedes the UPDATE, because the
-- deferred project_main trigger leaves events pending that forbid altering the table afterwards;
-- the temporary default only satisfies NOT NULL until the UPDATE sets the real value.
ALTER TABLE workspaces
 ADD COLUMN requested_ref text NOT NULL DEFAULT 'HEAD' CHECK (length(requested_ref) BETWEEN 1 AND 200),
 ADD COLUMN base_commit_id text CHECK (base_commit_id ~ '^([0-9a-f]{40}|[0-9a-f]{64})$');
ALTER TABLE workspaces ALTER COLUMN requested_ref DROP DEFAULT;
UPDATE workspaces w SET requested_ref=COALESCE(
 (SELECT wt.requested_ref FROM workspace_worktrees wt WHERE wt.workspace_id=w.id),
 (SELECT p.default_branch FROM projects p WHERE p.id=w.project_id));

-- Controller-reported Nodes carry the desktop two-part identity: a NodeId fixed per Workspace data
-- and one incarnation per Node process. Each row is one incarnation, allocated idempotently.
ALTER TABLE node_instances ADD COLUMN node_id text CHECK (length(node_id) BETWEEN 1 AND 200);
ALTER TABLE node_instances ADD COLUMN node_incarnation_id text CHECK (length(node_incarnation_id) BETWEEN 1 AND 200);
ALTER TABLE node_instances ADD UNIQUE (sandbox_instance_id, node_incarnation_id);

-- clone_executions now also registers the clone step of a Workspace operation. operation_id names
-- either a tenant clone request (workspace_id NULL) or a Workspace operation (workspace_id set); each
-- kind keeps a real foreign key. A tenant request still has at most one execution ever; a Workspace
-- operation has at most one execution without a result, and a retry after a failure is a new one.
ALTER TABLE clone_executions DROP CONSTRAINT clone_executions_operation_id_fkey;
ALTER TABLE clone_executions DROP CONSTRAINT clone_executions_operation_id_key;
ALTER TABLE clone_executions ADD COLUMN workspace_id uuid REFERENCES workspaces(id);
ALTER TABLE clone_executions ADD COLUMN clone_request_id uuid
 GENERATED ALWAYS AS (CASE WHEN workspace_id IS NULL THEN operation_id END) STORED REFERENCES clone_requests(id);
ALTER TABLE operations ADD UNIQUE (id, workspace_id);
ALTER TABLE clone_executions ADD FOREIGN KEY (operation_id, workspace_id) REFERENCES operations(id, workspace_id);
CREATE UNIQUE INDEX one_request_execution ON clone_executions(operation_id) WHERE workspace_id IS NULL;
CREATE UNIQUE INDEX one_pending_workspace_clone ON clone_executions(operation_id) WHERE workspace_id IS NOT NULL AND result IS NULL;
