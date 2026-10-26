-- Task-level user messages and handoff order. Existing Threads and IssueRuns keep their identity.
-- New rows are created only by explicit conversation sends. Historical comments and Thread entries
-- are read through their original resources rather than backfilled with guessed identities.
CREATE TABLE task_conversations (
  issue_id uuid PRIMARY KEY REFERENCES issues(id),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  last_seq bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
  acknowledged_run_id uuid REFERENCES issue_runs(id),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_conversation_messages (
  id uuid PRIMARY KEY,
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  issue_id uuid NOT NULL REFERENCES issues(id),
  seq bigint NOT NULL CHECK (seq > 0),
  user_id uuid NOT NULL REFERENCES users(id),
  agent_id uuid NOT NULL,
  body text NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 65536),
  state text NOT NULL CHECK (state IN ('waiting','submitted','cancelled','failed')),
  error_code text CHECK (error_code IS NULL OR error_code IN ('agent_unavailable','project_unavailable','member_unavailable','model_default_required','model_connection_unavailable','model_credential_required')),
  run_id uuid REFERENCES issue_runs(id),
  thread_seq bigint CHECK (thread_seq IS NULL OR thread_seq > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (issue_id, seq),
  CHECK ((state = 'waiting' AND run_id IS NULL AND thread_seq IS NULL)
      OR (state = 'submitted' AND run_id IS NOT NULL)
      OR (state IN ('cancelled','failed') AND run_id IS NULL AND thread_seq IS NULL)),
  CHECK ((state = 'failed') = (error_code IS NOT NULL))
);

CREATE INDEX task_conversation_waiting ON task_conversation_messages(issue_id, seq)
  WHERE state = 'waiting';
CREATE INDEX task_conversation_waiting_order ON task_conversation_messages(created_at, id)
  WHERE state = 'waiting';
CREATE INDEX task_conversation_run ON task_conversation_messages(run_id)
  WHERE run_id IS NOT NULL;

-- A durable scan cursor prevents an old group of paused tasks from permanently occupying the
-- bounded dispatch window. Advancing it under Cloud's short global transaction lock is safe across
-- replicas; wrapping after the tail eventually revisits a newly unblocked old task.
CREATE TABLE task_conversation_scan_state (
  singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
  last_created_at timestamptz,
  last_id uuid,
  CHECK ((last_created_at IS NULL) = (last_id IS NULL))
);
INSERT INTO task_conversation_scan_state(singleton) VALUES(true);

-- An optional Node-owned plain-text projection for future cross-run prompts. Existing Thread
-- records stay byte-identical and older rows simply have no projection to contribute.
ALTER TABLE thread_entries
  ADD COLUMN visible_text text CHECK (visible_text IS NULL OR octet_length(visible_text) <= 65536),
  ADD COLUMN visible_text_truncated boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT thread_visible_text_truncation CHECK (NOT visible_text_truncated OR visible_text IS NOT NULL);
