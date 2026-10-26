-- Older incarnations have no proof of the declared-adapter startup contract.
ALTER TABLE node_instances ADD COLUMN model_adapter jsonb NOT NULL DEFAULT '{}'::jsonb
 CHECK(jsonb_typeof(model_adapter)='object');
