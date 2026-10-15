-- One push approval can be attempted more than once, and the attempt that holds it has to be identifiable
-- and bounded. See specs decisions/node/repository/20261012-push-execution-authority.md D6 (the
-- pre-dispatch re-check), D7 (one authorization per attempt, and a retry is re-issued rather than
-- replayed) and D8 (the approval is durable before anything is dispatched).
--
-- 0037 gave the approval a state and 0038 the observation it was drawn from, and both describe a decision
-- made inside one request. Neither survives an attempt that never comes back. The dispatch used to run in
-- the caller's request lifetime: a canceled request, a killed process, or a settlement whose transaction
-- failed left the row `dispatching` with nothing anywhere reading that state — and no column recorded that
-- an attempt had been made at all, so nothing could tell a row whose dispatch was still running from one
-- whose dispatch had died, and nothing could bound how often a write was re-attempted.
--
-- Two columns close that, and they do different jobs.
--
-- `dispatch_generation` counts *claimed* attempts and fences their results. It is 0 until an attempt claims
-- the row, then 1, 2, ... after, and an attempt's settlement writes only while the row still holds that
-- attempt's generation. Without the fence a slow attempt — one that outlived its lease and was replaced —
-- would settle an approval another attempt is in the middle of, which is how a lost reply becomes a
-- recorded outcome nobody established. The count and the fence are one column on purpose: they move
-- together by construction, and a second counter would be a value that could disagree with this one.
--
-- `dispatch_lease_expires_at` is how long Cloud waits for that attempt before treating it as gone. It is
-- written with the database's clock, and a row becomes claimable again only once it has lapsed, so an
-- attempt still in flight is never replaced while its lease holds. It is the reconciliation clock rather
-- than the write fence: what stops two attempts from both writing is the executor's compare-and-swap on
-- the authorized commit pair, and what decides which attempt's answer counts is the generation above.
--
-- `authorization_id` (0037) now names the *current* attempt's authorization rather than the only one ever
-- minted: a re-attempt is a fresh authorization for the same binding, which D7 requires and this column
-- records. Its uniqueness is unchanged and still means what it always meant — no two approvals ever share
-- an authorization, so one authorization never authorizes two writes.
ALTER TABLE push_requests
 ADD COLUMN dispatch_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE push_requests
 ADD COLUMN dispatch_lease_expires_at timestamptz;

-- A generation counts claimed attempts and can never be negative. The default is the state every row
-- written before this migration is in, and it is exactly true of them: no attempt has claimed them, because
-- the dispatch ran inside the request that inserted the row and either settled it in that same request or
-- left it looking dispatched forever — which is the state this migration exists to end.
ALTER TABLE push_requests ADD CONSTRAINT push_request_dispatch_generation
 CHECK (dispatch_generation >= 0);

-- A live lease belongs to an attempt that is still being waited for, and only a `dispatching` row has one:
-- settling an approval ends every attempt it had. The CHECK states that as the pair's invariant, so a
-- settlement that forgot to release the lease fails loudly here instead of leaving a settled approval that
-- reads as one still being dispatched.
ALTER TABLE push_requests ADD CONSTRAINT push_request_dispatch_lease
 CHECK (state='dispatching' OR dispatch_lease_expires_at IS NULL);

-- The scan a pass makes is "which approvals are owed an attempt": `dispatching`, with a lapsed lease. The
-- partial index covers that filter and the `updated_at` ordering the pass walks, which neither existing
-- index can: `push_requests_workspace_sequence` is scoped to one Workspace's audit, and the identity key is
-- ordered by request id for the idempotency lookup rather than for reading. The filter is narrow enough
-- that the index covers only the rows a pass looks at, and a settled approval leaves the set for good.
CREATE INDEX push_requests_dispatch_pending ON push_requests(updated_at) WHERE state='dispatching';
