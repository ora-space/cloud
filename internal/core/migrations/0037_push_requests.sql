-- Push requests: the Cloud-side approval record for one controlled remote write. See specs
-- decisions/cloud/identity-access/20261013-push-capability-vocabulary-extension.md (D2, D3, D8, D9)
-- and decisions/node/repository/20261012-push-execution-authority.md (D3, D7, D8).
--
-- Why this is its own table rather than a row on `operations`. A push is proposed by a person, not by
-- a Controller: nothing about it is work the control plane discovered, and the authorization it
-- carries names no operation, no execution and no Node — C runs on none (the execution-authority
-- decision's D2). Filing it under `operations` would also put it under `one_project_operation`
-- (0001), so one pending push approval would block the Project's provisioning operation and, worse, a
-- Project mid-provision would make a push unapprovable. The two are separate lifecycles and the
-- schema says so.
--
-- What this row IS: the durable decision that one identified person approved one exact write, written
-- before anything is dispatched (D8), and the single place that decision can be read back from (D9).
-- What it deliberately does NOT carry: the signed authorization itself. The bearer is never persisted
-- anywhere — not here, not in a log — because a stored bearer is a replayable one; what is stored is
-- the authorization's identifier, which is the name the Credential Service records consumption under
-- and the only value the audit needs.
--
-- `state` is the three-state vocabulary the execution authority's D7 fixes, plus the one Cloud-internal
-- state that precedes it. `dispatching` exists so the approval and its dispatch are distinguishable: a
-- row left there is a push whose outcome Cloud does not know, which is the honest reading and never a
-- success.
CREATE TABLE push_requests (
 id uuid PRIMARY KEY,
 tenant_id uuid NOT NULL,
 project_id uuid NOT NULL,
 workspace_id uuid NOT NULL,
 -- The person who approved this write. It is the approval's subject as well as its author: the audit
 -- answers "which authorized person", and the unique key below is scoped to it, so one user's repeat
 -- of their own request replays their own row and can never be read as another user's approval.
 actor_user_id uuid NOT NULL,
 -- The caller's own idempotency identity, in the scope of the Workspace and the approver.
 request_id text NOT NULL CHECK(length(request_id)>0 AND length(request_id)<=200),
 -- The canonical upstream the authorization binds, snapshotted here rather than joined from the
 -- Project at read time. A Project can change its `repository_url` after an approval; the approval
 -- must be readable as the write it authorized, not as whatever the Project points at now.
 repository_url text NOT NULL CHECK(length(repository_url)>0),
 -- The exact ref, in the qualified spelling the authorization signs. There is exactly one column for
 -- it and no wildcard is representable: `*`, `?`, `[`, a space and the `+` force prefix are all
 -- refused by `cloneauth.ValidateRef` before this insert, and the CHECK repeats the qualified-prefix
 -- half of that rule so a row written by anything else still cannot hold two refs or a pattern.
 pinned_ref text NOT NULL CHECK(pinned_ref ~ '^refs/heads/' AND length(pinned_ref) BETWEEN 12 AND 200),
 -- The commit the ref was observed at and the commit it is authorized to become. Both are required,
 -- and neither can be the null object id: "no commit" is how a delete or a create-from-nothing would
 -- be spelled, and both are refused outright (the execution authority's D3). `cloneauth.ValidateCommit`
 -- enforces the same rule before signing, so this CHECK is the durable half of one rule rather than a
 -- second, weaker one.
 old_commit text NOT NULL CHECK(old_commit ~ '^[0-9a-f]{40}$' AND old_commit <> repeat('0',40)),
 new_commit text NOT NULL CHECK(new_commit ~ '^[0-9a-f]{40}$' AND new_commit <> repeat('0',40)),
 -- The exact credential version this write was approved for. The version is what makes "which
 -- credential authorized this" answerable after a rotation, and it is pinned at approval time rather
 -- than resolved at dispatch time: a credential that rotates between the two must not silently widen
 -- what one approval covers.
 credential_ref_id uuid NOT NULL,
 credential_ref_version bigint NOT NULL CHECK(credential_ref_version>0),
 -- The single-use authorization identity Cloud minted for this approval. It is UNIQUE because one
 -- request producing a second write authorization is exactly the failure the idempotency rules exist
 -- to prevent, and a uniqueness constraint states that in the one place no code path can skip.
 authorization_id uuid NOT NULL UNIQUE,
 state text NOT NULL CHECK(state IN ('dispatching','succeeded','failed','uncertain')),
 -- The stable outcome code, required exactly when the row has settled. `dispatching` carries none,
 -- because an unsettled push has no outcome to report and a code there would be read as one.
 result_code text CHECK(result_code IS NULL OR length(result_code)>0),
 settled_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 -- The request's identity: one approver, in one Workspace, repeating one request id. Scoped to the
 -- Workspace rather than the tenant because that is the scope the approval is made in and the scope
 -- its effect belongs to; two Workspaces of one Project are two subjects.
 CONSTRAINT push_request_identity UNIQUE (workspace_id, actor_user_id, request_id),
 -- A row is settled or it is not, and the two columns that describe its end move together. Splitting
 -- them would allow a `failed` row with no time or a `dispatching` row carrying a code, and both
 -- would be read back by the audit as a fact that never happened.
 CONSTRAINT push_request_settlement CHECK((state='dispatching')=(settled_at IS NULL)),
 CONSTRAINT push_request_result CHECK((state='dispatching')=(result_code IS NULL)),
 FOREIGN KEY(workspace_id, project_id) REFERENCES workspaces(id, project_id),
 FOREIGN KEY(project_id, tenant_id) REFERENCES projects(id, tenant_id),
 FOREIGN KEY(credential_ref_id, tenant_id) REFERENCES credential_refs(id, tenant_id)
);

-- The audit lists one Workspace's approvals through the shared `page` cursor, which walks a uuid column
-- in ascending order, so the index matches that walk and the unique key above — ordered by request id,
-- for the idempotency identity rather than for reading — cannot serve it. The listing is therefore in
-- the same order as every other page cursor in this API rather than newest first: the cursor contract
-- is the platform's, and a second ordering would need a second cursor shape.
CREATE INDEX push_requests_workspace_sequence ON push_requests(workspace_id, id);
