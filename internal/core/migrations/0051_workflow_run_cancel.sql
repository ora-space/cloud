-- workflow_runs carry a cancellation request (circus#347; specs ADR
-- decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md D4/D6).
--
-- D4 makes cancellation a request plus a bounded terminal path: the writer records the instant and
-- answers, and the Cloud-side scheduler that already owns the run reacts to it on a later pass.
-- Keeping the request on the run row is what lets the reaction be recomputed from committed state
-- like every other scheduler decision — replay, restart and replica passes read the same fact — and
-- a request that arrives while the run is still `pending` survives the claim that would otherwise
-- start it.
--
-- Additive and forward-only. Every existing row carries no request, which is exactly "nobody asked
-- this run to stop"; there is nothing to backfill.

ALTER TABLE workflow_runs ADD COLUMN cancel_requested_at timestamptz;
