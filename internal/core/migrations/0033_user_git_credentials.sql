-- User-managed personal HTTPS Git credentials (identity-access
-- 20261010-user-managed-git-credentials D1/D2/D5).
--
-- `credential_refs` stays the single business master table (D1): attribution, availability,
-- repository scope, capabilities and the freeze triggers already live here, so this change adds the
-- two columns a user-facing credential needs and nothing else. A parallel Git credential table would
-- fork the freeze semantics, the owner check and the project foreign key into two sets.
--
-- `name` is the label the owner sees in the credential list. It is a display value and authorizes
-- nothing: the credential's authority comes from `tenant_id` + `owner_user_id` + `repository_scope`
-- + `capabilities`, never from what it is called. The default is the empty string, and the product
-- path always writes a 1..120 character name, so the empty default exists only for
-- `ConfigureCredential`, the deployment CLI path, which knows no name. The product list filters on
-- `attribution='personal'`, which that path never writes, so a nameless row cannot reach a user.
--
-- `revoked_at` records when the owner withdrew the credential. Revocation itself is expressed
-- through `availability='invalid'` per D5 — `deleted_at` must not take on deletion semantics before
-- those are defined (D10) — so this column is the audit timestamp beside that state, not a second
-- source of truth about whether the credential is usable.
--
-- Both columns are additive and nullable-or-defaulted, so the change is safe to retry and requires
-- no backfill: existing rows keep their current meaning untouched.

ALTER TABLE credential_refs ADD COLUMN name text NOT NULL DEFAULT '' CHECK (char_length(name) <= 120);
ALTER TABLE credential_refs ADD COLUMN revoked_at timestamptz;
