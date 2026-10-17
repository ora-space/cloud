-- Workspaces clone from Git Server snapshots (ADRs cloud/operation "clone step consumes the Binding
-- snapshot" D5 and cloud/controller-integration "snapshot clone input and execution ref grants").
-- This is a breaking switch: no data is migrated, development data is rebuilt.
--
-- Tenant-level clone requests are retired. Their executions and receipts go with them, so
-- clone_executions only ever belongs to a Workspace operation's clone step.
DELETE FROM clone_event_receipts r USING clone_executions e WHERE e.execution_id = r.execution_id AND e.workspace_id IS NULL;
DELETE FROM clone_executions WHERE workspace_id IS NULL;
DROP INDEX IF EXISTS one_request_execution;
ALTER TABLE clone_executions DROP COLUMN IF EXISTS clone_request_id;
ALTER TABLE clone_executions ALTER COLUMN workspace_id SET NOT NULL;
DROP TABLE IF EXISTS clone_requests;
