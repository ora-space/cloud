-- The mirror preparation does not get its own step after all. 0034 added 'mirror' between 'node' and
-- 'clone' on the assumption that a new step name was the way to make the preparation visible to the
-- control loop. It is not: the step name crosses to the desktop Controller as a protobuf enum, and
-- that Controller is not ours to change.
--
-- The exact mechanism, because it is silent on the Cloud side:
--   * `internal/controlgrpc/operations.go` converts the stored step with `enum()`, which is a bare
--     map lookup (`values[prefix+strings.ToUpper(stored)]`) with no comma-ok form. An unrecognized
--     name therefore yields the zero value, not an error, so Cloud happily sends
--     OPERATION_STEP_UNSPECIFIED for step 'mirror'.
--   * `apps/ora-controller/src/cloud/operations.rs` treats that value as a failure:
--     `OperationStep::Done | OperationStep::Unspecified => Err(Error::Conflict)`.
--   * `drain()` aborts the round on that error and re-claims the operation forever, so no effect is
--     ever planned for the step. Cloud only advances when the round's effects are reconciled, so a
--     step the Controller cannot name is a step the operation cannot leave.
--
-- Nothing was deployed, so no row ever held 'mirror' and there is no history to preserve. The name is
-- removed rather than left as a latent trap: a vocabulary entry that no code can enter and no row
-- carries is a state the system must never be in, and a CHECK constraint that admits it invites it.
-- (Contrast the retired names kept by 0016: those stay because applied rows still spell them.)
--
-- The preparation itself is unchanged and still recorded in `mirror_preparations` (0034). It runs
-- inside the existing 'node' -> 'clone' transition, and Cloud refuses that transition until the
-- preparation is ready and its bindings re-verify; the preparation's own state carries the phase.
-- Making it drivable without a Controller change is the point; widening the step vocabulary was not.
ALTER TABLE operations DROP CONSTRAINT operations_step_check;
ALTER TABLE operations ADD CONSTRAINT operations_step_check
 CHECK (step IN ('storage','worktree','sandbox','node','clone','ready','quiesce','terminate','cleanup','storage_delete','plugin','done'));
