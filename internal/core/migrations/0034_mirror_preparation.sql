-- Mirror preparation: the pre-clone step that fills the internal Git Server. See specs
-- decisions/cloud/git-server/20261012-workspace-content-transport-over-the-internal-git-server.md
-- (D6, D7) and the 2026-10-13 ruling that approved a Cloud-authoritative mirror_prepare pre-operation.
--
-- Why this is a STEP and not a new operation kind. `one_project_operation` (0001) admits at most one
-- active operation per Project, so a mirror-preparation operation would serialize against the very
-- `create_workspace` operation whose clone needs the mirror: the two could never overlap, and a
-- Workspace could never be created on a Project whose mirror does not exist yet. The preparation
-- therefore runs inside the operation it belongs to, entered from the existing `node` -> `clone`
-- transition, which is also where the audit trail already lives.

-- The step vocabulary is a CHECK constraint, so a new step is a rebuild of it (precedent: 0015, 0016,
-- 0020). `mirror` is placed before `clone` because that is the order the operation now runs in. The
-- retired names stay in the list: 0016 keeps them valid for history, and `reject_retired_lifecycle`
-- is what stops a new row from entering one.
ALTER TABLE operations DROP CONSTRAINT operations_step_check;
ALTER TABLE operations ADD CONSTRAINT operations_step_check
 CHECK (step IN ('storage','worktree','sandbox','node','mirror','clone','ready','quiesce','terminate','cleanup','storage_delete','plugin','done'));

-- One row per operation's mirror step. The operation is the identity rather than a fresh row per
-- attempt, and that is what makes "a late result cannot advance the state" checkable: a retry
-- re-enters the same step, `generation` advances in place, and a result naming an older generation is
-- refused instead of applied to a preparation that has moved on.
--
-- The upstream identity is snapshotted here rather than read through a join at result time. A Project
-- can change its `repository_url` while a preparation is in flight; the preparation must be settled
-- against the repository it was authorized for, not against whatever the Project points at now.
CREATE TABLE mirror_preparations (
 operation_id uuid PRIMARY KEY REFERENCES operations(id),
 tenant_id uuid NOT NULL,
 project_id uuid NOT NULL,
 workspace_id uuid NOT NULL,
 -- The real upstream repository the fetch is authorized against. This is the identity the credential
 -- scope and the authorization bind; it is never the internal mirror URL, because the mirror URL is
 -- what the fetch is supposed to produce.
 repository_url text NOT NULL CHECK(length(repository_url)>0),
 -- The exact ref, in the qualified spelling the authorization signs. A short branch name is refused
 -- here as well as in `cloneauth.ValidateRef`: the two spellings of one branch are not
 -- interchangeable, and the database is the cheaper place to keep them from both existing.
 pinned_ref text NOT NULL CHECK(pinned_ref ~ '^refs/heads/' AND length(pinned_ref) BETWEEN 12 AND 200),
 -- The fencing values captured when the preparation was admitted. They are what a late result is
 -- compared against: a result produced under an older runtime generation or control epoch must not
 -- settle a preparation admitted under the current one.
 runtime_generation bigint NOT NULL CHECK(runtime_generation>0),
 control_epoch bigint NOT NULL CHECK(control_epoch>=0),
 -- Incremented on every attempt. A retry does not create a second row; it moves this one forward, so
 -- an answer that arrives after a retry is identifiable as belonging to the attempt that is over.
 generation bigint NOT NULL DEFAULT 1 CHECK(generation>0),
 state text NOT NULL CHECK(state IN ('admitting','dispatched','ready','failed','superseded')),
 credential_ref_id uuid REFERENCES credential_refs(id),
 credential_ref_version bigint,
 -- The single-use authorization Cloud signed for this attempt. It is recorded so the preparation can
 -- be audited against the exact authorization that authorized its fetch, and so a result naming an
 -- authorization other than the current attempt's is refusable rather than merely unexpected.
 authorization_id uuid,
 -- The commit the pinned ref resolved to, as C reported it, and the internal mirror generation that
 -- publication produced. Both are required once the state is `ready`: a ready preparation with no
 -- commit would name no immutable content, which is exactly what the key exists to prevent.
 commit_id text CHECK(commit_id IS NULL OR commit_id ~ '^([0-9a-f]{40}|[0-9a-f]{64})$'),
 mirror_generation bigint CHECK(mirror_generation IS NULL OR mirror_generation>0),
 error_code text,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CONSTRAINT mirror_preparation_credential_binding CHECK((credential_ref_id IS NULL)=(credential_ref_version IS NULL)),
 CONSTRAINT mirror_preparation_ready_has_evidence CHECK(state<>'ready' OR (commit_id IS NOT NULL AND mirror_generation IS NOT NULL)),
 FOREIGN KEY(workspace_id, project_id) REFERENCES workspaces(id, project_id),
 FOREIGN KEY(project_id, tenant_id) REFERENCES projects(id, tenant_id)
);
