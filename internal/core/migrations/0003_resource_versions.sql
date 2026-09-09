ALTER TABLE workspace_worktrees ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE sandbox_instances ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE external_effects ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE execution_tickets ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);
CREATE FUNCTION increment_resource_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN NEW.version:=OLD.version+1; RETURN NEW; END $$;
CREATE TRIGGER worktree_version BEFORE UPDATE ON workspace_worktrees FOR EACH ROW EXECUTE FUNCTION increment_resource_version();
CREATE TRIGGER sandbox_version BEFORE UPDATE ON sandbox_instances FOR EACH ROW EXECUTE FUNCTION increment_resource_version();
CREATE TRIGGER effect_version BEFORE UPDATE ON external_effects FOR EACH ROW EXECUTE FUNCTION increment_resource_version();
CREATE TRIGGER ticket_version BEFORE UPDATE ON execution_tickets FOR EACH ROW EXECUTE FUNCTION increment_resource_version();

CREATE FUNCTION check_isolated_task() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE wid uuid;
BEGIN
 IF TG_TABLE_NAME='workspaces' THEN wid:=COALESCE(NEW.id,OLD.id);
 ELSE
   IF TG_OP='UPDATE' AND NEW.workspace_id<>OLD.workspace_id THEN
     RAISE EXCEPTION 'task workspace is immutable' USING ERRCODE='23514';
   END IF;
   wid:=COALESCE(NEW.workspace_id,OLD.workspace_id);
 END IF;
 IF EXISTS(SELECT 1 FROM workspaces WHERE id=wid AND kind='isolated' AND deleted_at IS NULL) AND
 (SELECT count(*) FROM tasks WHERE workspace_id=wid AND deleted_at IS NULL)<>1 THEN
 RAISE EXCEPTION 'live isolated workspace requires one task identity' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER isolated_task AFTER INSERT OR UPDATE OR DELETE ON workspaces DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_isolated_task();
CREATE CONSTRAINT TRIGGER task_identity AFTER INSERT OR UPDATE OR DELETE ON tasks DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_isolated_task();
