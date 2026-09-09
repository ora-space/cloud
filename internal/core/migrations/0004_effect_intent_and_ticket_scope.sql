ALTER TABLE external_effects ADD COLUMN request jsonb;

UPDATE external_effects e
SET request=jsonb_strip_nulls(jsonb_build_object(
 'kind',e.kind,
 'projectId',e.project_id::text,
 'workspaceId',e.workspace_id::text,
 'repositoryUrl',CASE WHEN e.kind='worktree_ensure' THEN p.repository_url END,
 'requestedRef',CASE WHEN e.kind='worktree_ensure' THEN (
   SELECT wt.requested_ref FROM workspace_worktrees wt WHERE wt.workspace_id=e.workspace_id
 ) END,
 'sandboxInstanceId',CASE WHEN e.kind='sandbox_terminate' THEN (
   SELECT s.id::text FROM sandbox_instances s
   WHERE s.workspace_id=e.workspace_id
   ORDER BY s.generation DESC LIMIT 1
 ) END
))
FROM projects p
WHERE p.id=e.project_id;

ALTER TABLE external_effects ALTER COLUMN request SET NOT NULL;
ALTER TABLE external_effects ADD CHECK(jsonb_typeof(request)='object');

ALTER TABLE execution_tickets ADD COLUMN tenant_id uuid;
UPDATE execution_tickets ticket
SET tenant_id=workspace.tenant_id
FROM workspaces workspace
WHERE workspace.id=ticket.workspace_id;
ALTER TABLE execution_tickets ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE execution_tickets ADD FOREIGN KEY(workspace_id,tenant_id,actor_user_id)
 REFERENCES workspaces(id,tenant_id,owner_user_id);
