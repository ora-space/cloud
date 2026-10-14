# Bounded Agent startup and unknown-result business settlement

English | [中文](agent-startup-settlement.md)

The implementation follows approved bounded-agent-startup ADR D1–D4. Work and the first prompt are Cloud intent; registration means Node may have acted. Missing replies do not prove absence of effects. D5 blocked creation abandonment is a separate delivery.

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
