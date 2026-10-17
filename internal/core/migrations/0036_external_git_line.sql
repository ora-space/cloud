-- External Git line (ADRs cloud/repo-executor root, identity-access Credential Store, cloud/project
-- sync and publish). This is the destructive switch the contract allows: no transition flag, no
-- data migration. It only adds columns to the foundation's tables; states stay the contract's.

-- Credential references now point into the independent Credential Store. The deployment-preset
-- secret reference is gone as a source of truth; pre-switch rows can never be redeemed, so they
-- become revoked and keep only their audit trail.
ALTER TABLE credential_refs ALTER COLUMN secret_ref DROP NOT NULL;
ALTER TABLE credential_refs ADD COLUMN name text CHECK(name IS NULL OR length(name) BETWEEN 1 AND 200);
ALTER TABLE credential_refs ADD COLUMN secret_state text NOT NULL DEFAULT 'revoked' CHECK(secret_state IN ('awaiting_secret','stored','revoked'));
ALTER TABLE credential_refs ADD COLUMN store_version bigint NOT NULL DEFAULT 0 CHECK(store_version>=0);
ALTER TABLE credential_refs ADD COLUMN requested_capabilities jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE credential_refs ADD COLUMN capability_evidence jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(capability_evidence)='object');
-- A browser submission that Cloud authorized but has not confirmed yet. Rotating a stored
-- credential records the pending version here and leaves the reference stored meanwhile.
ALTER TABLE credential_refs ADD COLUMN pending_submission_id uuid;
ALTER TABLE credential_refs ADD COLUMN pending_store_version bigint;
ALTER TABLE credential_refs ADD COLUMN pending_submission_expires_at timestamptz;
-- Revocation is two steps: the reference is revoked in Cloud first, then in the Store, retried
-- until the Store confirms.
ALTER TABLE credential_refs ADD COLUMN store_revocation_pending boolean NOT NULL DEFAULT false;
ALTER TABLE credential_refs ADD COLUMN revoked_at timestamptz;
UPDATE credential_refs SET capabilities='[]'::jsonb, revoked_at=now();
ALTER TABLE credential_refs ADD CONSTRAINT credential_capability_vocabulary CHECK(capabilities <@ '["read","publish"]'::jsonb AND requested_capabilities <@ '["read","publish"]'::jsonb);
ALTER TABLE credential_refs ADD CONSTRAINT credential_stored_version CHECK(secret_state<>'stored' OR store_version>0);
ALTER TABLE credential_refs ADD CONSTRAINT credential_pending_submission CHECK(num_nonnulls(pending_submission_id,pending_store_version,pending_submission_expires_at) IN (0,3) AND (pending_store_version IS NULL OR pending_store_version=store_version+1));
ALTER TABLE credential_refs ADD CONSTRAINT credential_revoked_shape CHECK((secret_state='revoked')=(revoked_at IS NOT NULL) AND (NOT store_revocation_pending OR secret_state='revoked') AND (secret_state<>'revoked' OR pending_submission_id IS NULL));
ALTER TABLE credential_refs ADD CONSTRAINT credential_new_scope CHECK(secret_state='revoked' OR repository_scope IS NOT NULL);

-- Records of push-capability checks (identity-access D4). A verification runs once, like an
-- Operation, so the probe authorization it carries is used at most once.
CREATE TABLE credential_verifications (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 credential_ref_id uuid NOT NULL,
 store_version bigint NOT NULL CHECK(store_version>0),
 repository text NOT NULL,
 requested_by uuid REFERENCES users(id),
 state text NOT NULL CHECK(state IN ('pending','leased','running','succeeded','failed')),
 result text CHECK(result IN ('verified','push_denied','verification_failed')),
 lease_generation bigint NOT NULL DEFAULT 0,
 lease_expires_at timestamptz,
 authorization_id uuid UNIQUE,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 finished_at timestamptz,
 FOREIGN KEY(credential_ref_id,tenant_id) REFERENCES credential_refs(id,tenant_id),
 CONSTRAINT credential_verification_finished CHECK((finished_at IS NULL)=(state IN ('pending','leased','running')) AND (result IS NULL)=(finished_at IS NULL))
);
CREATE UNIQUE INDEX credential_verification_unfinished ON credential_verifications(credential_ref_id,store_version) WHERE state IN ('pending','leased','running');

-- A Binding has exactly one credential reference; the continuity "effective reference" column
-- folds into it. version fences candidate-credential switches (sync ADR D1, D5).
UPDATE repository_bindings SET credential_ref_id=COALESCE(repository_credential_ref_id,credential_ref_id);
ALTER TABLE repository_bindings DROP COLUMN repository_credential_ref_id;
ALTER TABLE repository_bindings ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK(version>0);

-- Sync and publish Operations get their lease, credential, result and publish columns. A row is
-- executed at most once: it enters running once and carries one authorization id.
ALTER TABLE repository_operations
 ADD COLUMN requested_by uuid REFERENCES users(id),
 ADD COLUMN source_operation_id uuid REFERENCES repository_operations(id),
 ADD COLUMN replay_count integer NOT NULL DEFAULT 0 CHECK(replay_count BETWEEN 0 AND 3),
 ADD COLUMN credential_ref_id uuid,
 ADD COLUMN credential_version bigint,
 ADD COLUMN binding_version bigint,
 ADD COLUMN expected_tracking_oid text CHECK(expected_tracking_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
 ADD COLUMN lease_generation bigint NOT NULL DEFAULT 0 CHECK(lease_generation>=0),
 ADD COLUMN lease_owner text CHECK(length(lease_owner) BETWEEN 1 AND 200),
 ADD COLUMN lease_expires_at timestamptz,
 ADD COLUMN authorization_id uuid UNIQUE,
 ADD COLUMN started_at timestamptz,
 ADD COLUMN task_deadline timestamptz,
 ADD COLUMN result jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(result)='object'),
 ADD COLUMN source_ref text,
 ADD COLUMN source_commit text CHECK(source_commit ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
 ADD COLUMN target_branch text CHECK(length(target_branch) BETWEEN 1 AND 200),
 ADD COLUMN expected_old_oid text CHECK(expected_old_oid ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
 ADD COLUMN revision_id uuid,
 ADD COLUMN approver_kind text CHECK(approver_kind IN ('credential_owner','publisher')),
 ADD COLUMN approved_by uuid REFERENCES users(id),
 ADD COLUMN approved_at timestamptz,
 ADD COLUMN approved_store_version bigint,
 ADD COLUMN approval_expires_at timestamptz,
 ADD COLUMN check_attempts integer NOT NULL DEFAULT 0 CHECK(check_attempts BETWEEN 0 AND 20),
 ADD COLUMN next_check_at timestamptz,
 ADD CONSTRAINT repository_operation_credential FOREIGN KEY(credential_ref_id,tenant_id) REFERENCES credential_refs(id,tenant_id),
 ADD CONSTRAINT repository_operation_credential_version CHECK((credential_ref_id IS NULL)=(credential_version IS NULL)),
 ADD CONSTRAINT repository_operation_lease CHECK(state NOT IN ('leased','running') OR lease_expires_at IS NOT NULL AND lease_owner IS NOT NULL),
 ADD CONSTRAINT repository_operation_started CHECK((started_at IS NULL)=(authorization_id IS NULL) AND (state NOT IN ('running','uncertain') OR started_at IS NOT NULL)),
 ADD CONSTRAINT repository_operation_publish_shape CHECK(
  type='repository_sync' AND num_nonnulls(source_ref,source_commit,target_branch,expected_old_oid,approver_kind,approved_by,approved_at,approved_store_version,approval_expires_at,revision_id)=0
  OR type='repository_publish' AND num_nonnulls(source_ref,source_commit,target_branch,expected_old_oid,approver_kind,approval_expires_at,credential_ref_id,requested_by)=8 AND expected_tracking_oid IS NULL
   AND num_nonnulls(approved_by,approved_at,approved_store_version) IN (0,3)
   AND (state IN ('awaiting_approval','cancelled') OR approved_at IS NOT NULL));
-- Contract invariant: one publish at most is touching an external branch at a time.
CREATE UNIQUE INDEX repository_publish_in_flight ON repository_operations(binding_id,target_branch) WHERE type='repository_publish' AND state IN ('leased','running');
CREATE INDEX repository_operation_work ON repository_operations(state,lease_expires_at) WHERE state IN ('pending','leased','running','uncertain');

-- Append-only audit of publish Operations (publish ADR D7) and sync settlements.
CREATE TABLE repository_operation_events (
 id bigserial PRIMARY KEY,
 operation_id uuid NOT NULL REFERENCES repository_operations(id),
 tenant_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind ~ '^[a-z][a-z_]{0,63}$'),
 actor_user_id uuid REFERENCES users(id),
 detail jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(detail)='object' AND octet_length(detail::text) <= 8192),
 at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX repository_operation_event_operation ON repository_operation_events(operation_id,id);
CREATE FUNCTION repository_operation_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'repository_operation_events is append-only';
END $$;
CREATE TRIGGER repository_operation_events_immutable BEFORE UPDATE OR DELETE ON repository_operation_events FOR EACH ROW EXECUTE FUNCTION repository_operation_events_append_only();
