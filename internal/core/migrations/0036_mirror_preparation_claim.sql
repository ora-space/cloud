-- The mirror preparation needs a claim protocol, and 0034 gave it only half of one.
--
-- 0034 established the row's identity (one per Operation), its state vocabulary and `generation` as the
-- monotonic attempt fence. What it did not have is anything that lets several Cloud processes, or one
-- Cloud process after a restart, agree on *who is preparing right now* and *when to try again*. Without
-- that, a worker would either dispatch the same preparation twice or never retry a lost one.
--
-- The protocol these columns serve, in three short transactions with nothing held across the slow part:
--   1. Claim: pick an Operation that needs a preparation (or whose attempt has lapsed), write the
--      binding it was authorized for, bump `generation`, set `lease_expires_at`, commit. The `generation`
--      value returned is the attempt's fence.
--   2. Dispatch: outside any transaction, sign the authorization and call C. This is the minutes-long
--      part, and the reason none of it is inside the claim.
--   3. Settle: accept the result only if `generation` still equals the fence and the lease still belongs
--      to this attempt; otherwise the answer belongs to an attempt that is over and is refused rather
--      than applied.
--
-- A lease rather than a lock, because an attempt that dies leaves no transaction to roll back: the
-- lease simply lapses and the next pass may claim the row. That is also why the lease has to be written
-- with the database's clock — a lease timed by a worker's own clock would let a worker whose clock runs
-- fast steal an attempt from a slower one that is still working.
ALTER TABLE mirror_preparations
 ADD COLUMN lease_expires_at timestamptz,
 ADD COLUMN retry_at timestamptz;

-- A dispatched preparation is one whose authorization exists and which C was asked to fetch. It must
-- carry a lease, or an attempt that died between signing and dispatching would hold the row forever
-- with nothing to expire. `admitting` is the state before the authorization exists, so it may hold no
-- lease yet.
ALTER TABLE mirror_preparations ADD CONSTRAINT mirror_preparation_dispatch_has_lease
 CHECK (state <> 'dispatched' OR lease_expires_at IS NOT NULL);

-- Settled states carry no lease and no backoff: leaving one behind would make a finished preparation
-- look claimable to the next pass.
ALTER TABLE mirror_preparations ADD CONSTRAINT mirror_preparation_settled_has_no_lease
 CHECK (state IN ('admitting','dispatched') OR (lease_expires_at IS NULL AND retry_at IS NULL));

-- The claim query reads "work that is unclaimed, or whose attempt has lapsed". Both halves are the
-- state and the lease, and this index is what keeps the pass from scanning every operation.
CREATE INDEX mirror_preparations_claimable ON mirror_preparations (state, lease_expires_at)
 WHERE state IN ('admitting','dispatched');
