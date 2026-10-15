-- The preparation's attempt budget is counted, not fenced by the generation number.
--
-- 0034 and 0036 gave `generation` two jobs: the monotonic fence a late result is refused against, and
-- the count of attempts spent. The two agree while every attempt answers, and come apart when one does
-- not. An attempt whose process is killed between the claim and the result writes nothing at all: the
-- row stays `dispatched` at generation N until the lease lapses, and the candidate query's
-- `generation < bound` test then refuses the row forever. The Operation sits at `node`, the Workspace
-- sits behind the gate, and nothing works on the mirror — the wedge the attempt bound exists to
-- prevent, reached from the other side. Three attempts is not a hypothesis: an executor that is
-- restarted, or a Cloud process killed mid-dispatch, spends one without recording anything.
--
-- `attempts` counts the attempts that *concluded* — answered with a published mirror or with a
-- failure — because that is what a budget can honestly bound: a lost attempt established nothing, and
-- the lease is what bounds it instead. `generation` stays exactly what it was, the fence.
--
-- The backfill reads the only evidence the rows written before this column carry: a settled row at
-- generation N concluded N attempts, and a `dispatched` one at generation N concluded N-1, the Nth
-- being the attempt it is in or was left in. A `ready` row starts at zero because publishing a mirror
-- ends the streak the budget counts, and the code below clears the count on every success.
ALTER TABLE mirror_preparations ADD COLUMN attempts bigint NOT NULL DEFAULT 0 CHECK(attempts>=0);
UPDATE mirror_preparations SET attempts = CASE
 WHEN state='ready' THEN 0
 WHEN state='dispatched' THEN GREATEST(generation - 1, 0)
 ELSE generation END;
