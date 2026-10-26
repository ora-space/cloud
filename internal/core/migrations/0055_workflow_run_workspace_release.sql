-- WorkflowRun Workspace release (circus#75 S5; specs decisions/cloud/workflow
-- 0-cloud-side-scheduler-owns-workflow-execution.md D3/D7).
--
-- A WorkflowRun's isolated Workspace is a real resource with a real owner: Cloud provisions it, and
-- once the run is terminal it must either be deleted under the operation contract or be recorded as
-- abandoned with a reason. Before this migration that fact was implicit — the scheduler declared the
-- delete and nothing recorded how it ended — so a terminal run could not say whether its Workspace
-- was gone, and a delete operation stuck behind an unreachable Node was re-declared forever.
--
-- The state machine is forward-only and has one writer per transition:
--
--   pending    -> releasing      terminal cleanup declared the Workspace delete
--   pending    -> not_required   the run never provisioned a Workspace
--   releasing  -> released       the delete_workspace operation succeeded, in the same transaction
--   releasing  -> abandoned      the Workspace's Node stayed unreachable past the give-up window
--
-- `released` is the only state that claims the Workspace is gone, and the abort path never writes it:
-- `abandoned` carries the reason, keeps the Workspace row (the record of a residual resource) and
-- leaves `workspace_released_at` NULL. `not_required` is the honest answer for a run whose graph
-- never needed a Workspace. A run never moves out of a terminal release state: a late delete success
-- after a give-up replays an operation the give-up already failed, and must not rewrite the fact.
--
-- Additive; edits no applied migration.

ALTER TABLE workflow_runs
  ADD COLUMN workspace_release_state text NOT NULL DEFAULT 'pending'
    CHECK (workspace_release_state IN ('pending','releasing','released','abandoned','not_required')),
  ADD COLUMN workspace_release_error text NOT NULL DEFAULT '',
  ADD COLUMN workspace_released_at timestamptz,
  ADD CONSTRAINT workflow_run_release_released_at
    CHECK (workspace_release_state <> 'released' OR workspace_released_at IS NOT NULL),
  ADD CONSTRAINT workflow_run_release_abandoned_error
    CHECK (workspace_release_state <> 'abandoned' OR btrim(workspace_release_error) <> '');

-- Backfill existing rows from the evidence already authoritative in the database. A terminal run
-- with no Workspace row never provisioned one; one whose Workspace row is soft-deleted had it
-- released by the very delete operation that removed it in the same commit; one with a live
-- Workspace either waits for its delete or has one in flight, and the scheduler's reconciliation
-- pass finishes that job. Non-terminal runs stay `pending`: their release has not started.
--
-- `workspace_released_at` for a backfilled release is the Workspace's own deletion instant, which is
-- the commit in which the delete operation settled — no row is stamped with the migration's clock.
UPDATE workflow_runs r
SET workspace_release_state = CASE
      WHEN r.status NOT IN ('succeeded','failed','cancelled') THEN 'pending'
      WHEN w.id IS NULL THEN 'not_required'
      WHEN w.deleted_at IS NOT NULL THEN 'released'
      ELSE 'releasing'
    END,
    workspace_released_at = CASE
      WHEN r.status IN ('succeeded','failed','cancelled') AND w.deleted_at IS NOT NULL THEN w.deleted_at
    END
FROM workflow_runs current
LEFT JOIN workspaces w ON w.workflow_run_id = current.id
WHERE r.id = current.id;
