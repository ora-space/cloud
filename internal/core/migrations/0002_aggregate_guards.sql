ALTER TABLE projects ADD UNIQUE(id,tenant_id);
ALTER TABLE operations ADD FOREIGN KEY(project_id,tenant_id) REFERENCES projects(id,tenant_id);
ALTER TABLE operations ADD UNIQUE(id,project_id);
ALTER TABLE external_effects ADD FOREIGN KEY(operation_id,project_id) REFERENCES operations(id,project_id);
ALTER TABLE external_effects ADD FOREIGN KEY(workspace_id,project_id) REFERENCES workspaces(id,project_id);
ALTER TABLE node_instances ADD COLUMN workspace_id uuid;
UPDATE node_instances n SET workspace_id=s.workspace_id FROM sandbox_instances s WHERE s.id=n.sandbox_instance_id;
ALTER TABLE node_instances ALTER COLUMN workspace_id SET NOT NULL;
ALTER TABLE sandbox_instances ADD UNIQUE(id,workspace_id);
ALTER TABLE node_instances ADD FOREIGN KEY(sandbox_instance_id,workspace_id) REFERENCES sandbox_instances(id,workspace_id);
ALTER TABLE node_instances ADD UNIQUE(id,workspace_id);
ALTER TABLE execution_tickets ADD FOREIGN KEY(node_instance_id,workspace_id) REFERENCES node_instances(id,workspace_id);
CREATE FUNCTION immutable_ownership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.id<>OLD.id OR NEW.tenant_id<>OLD.tenant_id OR NEW.owner_user_id<>OLD.owner_user_id THEN
 RAISE EXCEPTION 'resource ownership is immutable' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='workspaces' AND (to_jsonb(NEW)->>'project_id'<>to_jsonb(OLD)->>'project_id' OR to_jsonb(NEW)->>'kind'<>to_jsonb(OLD)->>'kind') THEN
 RAISE EXCEPTION 'workspace aggregate is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER immutable_project BEFORE UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION immutable_ownership();
CREATE TRIGGER immutable_workspace BEFORE UPDATE ON workspaces FOR EACH ROW EXECUTE FUNCTION immutable_ownership();
CREATE FUNCTION validate_task_kind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM workspaces WHERE id=NEW.workspace_id AND kind='isolated') THEN
 RAISE EXCEPTION 'task requires isolated workspace' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE TRIGGER task_kind BEFORE INSERT OR UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION validate_task_kind();
CREATE FUNCTION validate_effective_admins() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM 1 FROM tenants WHERE status='active' ORDER BY id FOR UPDATE;
 IF EXISTS(SELECT 1 FROM tenants t WHERE t.status='active' AND NOT EXISTS(
 SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=t.id AND m.role='admin' AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)) THEN
 RAISE EXCEPTION 'active tenant requires active administrator' USING ERRCODE='23514'; END IF; RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER tenant_admin AFTER INSERT OR UPDATE ON tenants DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_effective_admins();
CREATE CONSTRAINT TRIGGER user_admin AFTER UPDATE OR DELETE ON users DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_effective_admins();
