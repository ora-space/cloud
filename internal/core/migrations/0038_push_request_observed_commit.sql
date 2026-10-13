-- The push outcome needs a place for the observation it was drawn from. See specs decisions/node/
-- repository/20261012-push-execution-authority.md D7 and the core test case's four uncertainty rules.
--
-- 0037 recorded *what Cloud concluded* (`state`, `result_code`) and nothing about *what was seen*. For a
-- read that made no difference: a clone either produced a commit or it did not, and the commit goes into
-- the workspace. A write is different, because two of its three terminal states are only meaningful
-- beside the value they were decided from:
--
--   * `uncertain` / `push_attribution_unknown` means the ref already held the requested commit and this
--     push cannot be credited with putting it there. Without the observed commit a reader cannot tell
--     that from a push that failed and left the ref untouched.
--   * `uncertain` / `push_target_conflict` means the ref holds something nobody authorized. The
--     something is the whole content of the finding: it is what an operator needs in order to decide
--     whether a third party wrote, whether a lease was lost, or whether the reconciliation raced. A row
--     saying only "conflict" would send them to look it up by hand, and by then the ref may have moved
--     again.
--
-- So the column is the executor's own reading of the remote, stored as evidence next to the verdict
-- rather than folded into it. Cloud derives the verdict from this value; keeping the value is what lets
-- a later reader re-derive the verdict instead of trusting the label. The column is additive and the
-- migration is the forward kind: 0037 is already applied in any database that has it and its checksum is
-- verified, so this is a new file rather than an edit to that one.
ALTER TABLE push_requests
 ADD COLUMN observed_commit text;

-- Two conditions, both narrow. The value is a full commit id when it is present, because the only writer
-- is a reconciliation of a Git ref and a partial or abbreviated id would not be the thing that was read;
-- and it is absent when nothing was observed, which is the common case — a refusal before any write, an
-- authorization that could not be redeemed, and a reconciliation that could not reach the remote all
-- observe nothing, and NULL is how a row says "not observed" without inventing a value for it.
--
-- It is deliberately NOT tied to the state. A `succeeded` row may carry it (the remote's value after the
-- write), an `uncertain` row may carry it or not (a conflict carries one; an unreachable remote carries
-- none), and a `failed` row may carry one too (a lease rejection is decided from the value that was
-- seen). A CHECK that coupled the two would have to guess which combinations occur, and would refuse a
-- truthful row the first time the guess was wrong.
ALTER TABLE push_requests ADD CONSTRAINT push_request_observed_commit
 CHECK (observed_commit IS NULL OR observed_commit ~ '^[0-9a-f]{40}$');
