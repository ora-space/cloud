-- WorkflowRun owns one logical Revision and an independently observable delivery lifecycle.
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_run_delivery_identity UNIQUE(id,tenant_id,project_id);
ALTER TABLE workspaces ADD CONSTRAINT workspace_workflow_delivery_identity UNIQUE(id,workflow_run_id,project_id,tenant_id);
ALTER TABLE revisions ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE revisions ADD COLUMN workflow_run_id uuid UNIQUE;
ALTER TABLE revisions ADD CONSTRAINT revision_owner_exclusive CHECK ((run_id IS NULL) <> (workflow_run_id IS NULL));
ALTER TABLE revisions ADD CONSTRAINT revision_workflow_owner FOREIGN KEY(workflow_run_id,tenant_id,project_id)
 REFERENCES workflow_runs(id,tenant_id,project_id);
ALTER TABLE revisions ADD CONSTRAINT revision_workflow_workspace FOREIGN KEY(workspace_id,workflow_run_id,project_id,tenant_id)
 REFERENCES workspaces(id,workflow_run_id,project_id,tenant_id);
ALTER TABLE revisions ADD COLUMN history_kind text NOT NULL DEFAULT 'agent_jsonl'
 CHECK(history_kind IN ('agent_jsonl','workflow_manifest_v1'));
ALTER TABLE revisions ADD CONSTRAINT revision_history_owner CHECK (
 (workflow_run_id IS NULL AND history_kind='agent_jsonl') OR
 (workflow_run_id IS NOT NULL AND history_kind='workflow_manifest_v1' AND prior_revision_id IS NULL));

ALTER TABLE workflow_runs ADD COLUMN delivery_state text NOT NULL DEFAULT 'pending'
 CHECK(delivery_state IN ('pending','delivering','saved','unchanged','failed','skipped','not_required'));
ALTER TABLE workflow_runs ADD COLUMN delivery_error text NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN delivery_started_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN delivery_settled_at timestamptz;
ALTER TABLE workflow_runs ADD COLUMN delivery_revision_id uuid REFERENCES revisions(id);
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_delivery_revision CHECK (
 (delivery_state IN ('saved','unchanged')) = (delivery_revision_id IS NOT NULL));
ALTER TABLE node_instances ADD COLUMN workflow_history_delivery boolean NOT NULL DEFAULT false;

DROP INDEX one_unregistered_workflow_execution_work;
CREATE UNIQUE INDEX one_unregistered_workflow_execution_work ON execution_work(workflow_run_id,kind)
 WHERE execution_id IS NULL AND run_id IS NULL;
CREATE INDEX workflow_run_delivery_pending ON workflow_runs(updated_at,id)
 WHERE deleted_at IS NULL AND status IN ('succeeded','failed','cancelled')
 AND delivery_state IN ('pending','delivering');
