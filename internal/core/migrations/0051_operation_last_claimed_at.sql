-- Claim rotation (settle-deterministic-dispatch-refusals D3): last_claimed_at records only that
-- Cloud handed the operation to a Controller. It is not progress, a failure count, a lease, or a
-- timeout origin, so it cannot reuse updated_at, which unrelated writes refresh. Existing rows
-- stay NULL and rotate by created_at on their first claim without fabricating past claim times.
ALTER TABLE operations ADD COLUMN last_claimed_at timestamptz;
-- Serves the claim candidate order COALESCE(last_claimed_at, created_at), created_at, id over the
-- claimable states; due-ness of retry_wait rows is filtered from the same candidate set.
CREATE INDEX operations_claim_rotation ON operations(COALESCE(last_claimed_at, created_at), created_at, id)
 WHERE state IN ('queued','running','retry_wait');
