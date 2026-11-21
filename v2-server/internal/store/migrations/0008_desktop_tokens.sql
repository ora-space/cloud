-- A desktop token is the credential Ora desktop holds after a browser-approved loopback login
-- (A6 D6). It is a row of sessions like a web session, so both resolve to the same account and the
-- same tenant authorization, and disabling or deleting the account ends both at the next request.
-- kind keeps the two apart where their rules differ: a web session is carried only by the cookie
-- and expires a fixed time after login; a desktop token is carried only by an Authorization header
-- and expires after 30 days without use, which last_used_at records (refreshed at most hourly).
-- Existing rows are web sessions.
ALTER TABLE sessions
	ADD COLUMN kind text NOT NULL DEFAULT 'web' CHECK (kind IN ('web', 'desktop')),
	ADD COLUMN last_used_at timestamptz NOT NULL DEFAULT now();
