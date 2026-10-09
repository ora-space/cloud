-- User git commit identity (identity-access 20260928-user-git-commit-identity D1).
--
-- A user may state the name and email their Agent runs commit as. It is a self-described commit
-- signature, never an authentication identity, and it grants nothing. It lives in its own table
-- rather than as columns on `users` because `/me` and every user read return the `users` row
-- whole: a git email there would travel to every reader, while here only its owner's
-- `/me/git-identity` and the session-start spec ever read it. No row means the user has not stated
-- one and the default identity applies; `version` is this row's own optimistic-concurrency
-- counter.
--
-- The checks mirror the API's validation so a writer that bypassed it still cannot store a value
-- Git or the Node would reject: a name of 1..200 characters with no line break or angle bracket, an
-- email of at most 254 bytes shaped local@domain with no whitespace or angle bracket.
--
-- Purely additive over 0031: a new table, no existing table altered.

CREATE TABLE user_git_identities (
  user_id uuid PRIMARY KEY REFERENCES users(id),
  name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200 AND name !~ '[\r\n<>]'),
  email text NOT NULL CHECK (octet_length(email) <= 254 AND email ~ '^[^[:space:]<>@]+@[^[:space:]<>@]+$'),
  version bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
  updated_at timestamptz NOT NULL DEFAULT now()
);
