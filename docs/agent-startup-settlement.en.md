# Bounded Agent startup and unknown-result business settlement

English | [中文](agent-startup-settlement.md)

The implementation follows the approved bounded-agent-startup ADR. Phase 1 (D1–D4) is described above; phase 2 (D5, bounded abandonment of a blocked creation) is delivered in this repository — see "Bounded abandonment of a blocked creation" below. Work and the first prompt are Cloud intent; registration means Node may have acted. Missing replies do not prove absence of effects.

## Durable windows

| Setting (environment prefix CLOUD_ISSUE_RUNS_) | Default | Anchor |
| --- | --- | --- |
| agent_start_timeout | 2m | Run Workspace creation succeeds/ready |
| agent_restore_timeout | 6m | Same ready anchor, replacing the ordinary window and including restore/start |
| agent_end_confirm_timeout | 5m | First ending of an unresolved registered startup |

Omitted values use defaults; explicit zero, negative, malformed or overflowing durations refuse startup. Migration 0034 adds durable ready/deadline and resume selection. Declaration, input, retry, restart and configuration changes never renew the absolute deadline. Resume candidates keep the existing Issue/Project/repository/refusal rules; ready freezes the choice to select the window, and declaration reuses it.

Legacy starting runs initialize once under the serialized scan: a completed create_workspace anchors ready, otherwise the first policy observation is explicit. Existing declarations retain their work's frozen restore input. Missing historical evidence cannot retroactively establish exact deadlines. Historical D8 release-abandonment outcomes are unchanged.

## Before and after registration

Before registration, only a current initialized connected Node with a heartbeat within 30 seconds can prove model_proxy_unavailable. Capacity, queueing and temporary unreachability retain the same work/input; the existing scheduler owns deferral within the original budget, not a second queue or backoff policy. Expiry atomically withdraws intent and settles agent_start_failed/releasing; an earlier user end settles cancelled. Registration and withdrawal share the transaction lock; terminal state, discarded queued input and owned-delete declaration commit together.

An unresolved registered startup requests ending at expiry, preserving existing user intent and execution/resource facts. Timely real terminal evidence follows the original delivery path. Confirmation expiry atomically commits failed/done/ended, deliveryState=failed and agent_run_residuals without inventing Node results, receipts or a Revision. New dispatch/input/model authorization closes; the Workspace remains protected from ordinary deletion or handoff. Late true records and terminal events are receipted against original identities without resurrection or another settlement.

## Operator query and recovery boundary

A Node reconnect can restore connection and readiness evidence, but an uncleared residual keeps Workspace admission closed. Connectivity proves neither permission to accept new work nor physical cleanup; the same path still restores admission for ordinary runs without residuals.

Authorized deployment operators query agent_run_residuals by tenant_id/run_id through controlled database access. Rows contain resource identities and outstanding responsibility, not credentials or business input. session_ended_at proves a late session terminal, never physical deletion. cleared_at is reserved for a separately approved cleanup workflow and is not automatically set here.

Tenant/project/workspace/generation and existing operation/effect/clone/Node execution identities remain retained. Hidden Workspace public endpoints stay unavailable. Automatic cleanup needs the later recovery ADR's authorization, real termination/late-ensure fencing and deletion order. Business done does not mean resources are physically released.

Direct regressions cover frozen windows, invalid configuration, changed settings across two Stores, deferred work, repeated scans, ledger rollback, migration and real HTTP/gRPC late facts. Full Controller deferral and permanent-disconnection deployment qualification still require coordination with scheduling and deployment acceptance.

## Bounded abandonment of a blocked creation (D5)

A run Workspace is invisible to the public API (no manual retry can reach it) and a blocked creation holds the Project's one_project_operation, so Cloud's persistent scan is the only way the wait ends. The first transaction persisting `blocked` freezes an absolute deadline in that same transaction — issue_runs.workspace_blocked_at / workspace_deadline_at / workspace_blocked_source — with a configurable default of 30m (`agent_workspace_blocked_timeout`; omission selects the default, an explicit nonpositive or invalid value rejects startup). Defer, manual retry, re-claiming, restarts and configuration changes never renew it; a temporary return to running/retry_wait does not erase it; a queued creation that never got the Project slot does not open it. Legacy blocked creations have no immutable first-blocked evidence: nothing is backfilled, and the first policy scan grants one default window recorded as `first_observed`, explicitly not claiming the original window was already in force.

When the deadline expires with no real creation terminal, one transaction commits atomically: the agent_run_residuals row (reason `workspace_unavailable`, including not-yet-reported effect plans and clone/plugin execution identities), Workspace admission closed with a bumped epoch, model bindings and grants revoked, queued user turns discarded, the Run `failed/done` (`failure_reason=workspace_unavailable`, `deliveryState=skipped`, result naming Cloud as the abandoning source) and the creation operation `failed` / `provisioning_result_unknown` — leaving the unfinished states is what releases one_project_operation, so another Workspace of the same Project can proceed. A ledger or protection write failure rolls the whole transaction back; the slot is never released without residual ownership. No delete is queued: the residual Workspace's deletion and Project-level deletes involving it are refused by the existing residual checks. A cancellation never resets the window: a creation that really settles first follows the original cancel/release path, while a deadline that expires first with the result still unknown settles as failed/done.

After the business terminal, facts keep flowing: a Controller submits late evidence — an effect's real Substrate result, a clone or plugin execution's real outcome, or the creation's own real outcome — under its current service identity and lease through `RecordProvisioningFact`. Cloud validates it against the original operation/effect/execution identities, stores it idempotently through the live paths' own validators, receipts it onto the residual ledger and only then acknowledges. The ledger records under stable identities: an identical fact replays as a no-op; a provisional observation (running, absent, pending) is taken over by the later real fact, which keeps the superseded observation as prior evidence; a recorded terminal accepts only its replay and refuses contradicting facts. A submission's fingerprint binds its operation/effect target, so replaying a submission id against a different target is a conflict. No admission reopens, no starting, no second settlement, no new execution. The operation's own actions (claim, defer, advance, plan, effect result) stay closed to an abandoned creation. Physical termination, late-ensure fencing, authorization recycling and deletion order remain the later recovery ADR's responsibility.
