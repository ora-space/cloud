# Bounded Startup: Waiting Limits and Settling Unknown Outcomes

English | [中文](startup-settlement.md)

After a run Workspace is ready, the Agent session may fail to come up: the Controller never claimed
the work, the Node lacks a capability, or the session was dispatched but no record ever came back.
Without a limit the run would stay in `starting` forever and keep the sandbox occupied. This document
describes how Cloud bounds the wait for startup and how it settles on expiry — without ever treating
"no reply" as "nothing ran outside".

The central distinction:

- **The session work item and the first prompt are Cloud's own intent.** As long as the Controller has
  not registered their dispatch, Cloud knows nothing ran outside and can withdraw them atomically.
- **A dispatch registration means the Node may already be executing.** Silence after that cannot prove
  the absence of external side effects; Cloud can only request an end and keep the responsibility for
  reconciliation and residual resources.

## Durable windows

| Setting (`issue_runs.*`, environment prefix `CLOUD_ISSUE_RUNS_`) | Default | Starts at |
|---|---|---|
| `agent_start_timeout` | 2 minutes | The moment the run Workspace was created successfully and became ready |
| `agent_restore_timeout` | 6 minutes | The same ready moment; when the run resumes a prior Revision it **replaces** the normal window and covers both restore and startup |
| `agent_end_confirm_timeout` | 5 minutes | The first request to end the session during startup |

- The window is computed once, when the run enters `starting`, into an **absolute deadline** stored in the
  database (`startup_deadline_at`), together with whether a prior Revision will be resumed. Later
  declarations, user input, retries, Cloud restarts and configuration changes **never extend it**, and
  replicas with different configurations cannot change a deadline already fixed.
- The end-confirmation deadline is likewise fixed once, at the first end request
  (`startup_ending_deadline_at`), and never extended.
- Missing settings fall back to the defaults; an explicit zero, negative, non-duration or overflowing value
  makes the server refuse to start.
- Runs already in `starting` before migration `0034` get their window fixed exactly once in a serial scan:
  starting from the completion time of a successful creation operation when that evidence exists,
  otherwise explicitly from the time of the first observation. Missing evidence is never used to back-date
  a precise deadline for a historical run, and historical outcomes are never rewritten.

## Not yet registered

While the session work item has not been registered as dispatched, the startup scan **withdraws the intent
atomically** and settles the run in these cases:

| Case | Settlement |
|---|---|
| The user ended the session first, or the run was cancelled | `cancelled` |
| The current Node is initialized, connected, with a heartbeat within 30 seconds, and **definitively** lacks the model proxy capability while the run uses a personal model | `failed`, `failureReason = model_proxy_unavailable` |
| Such a Node **definitively** lacks the adapter capability the run's frozen Agent declaration needs (target, contract version or protocol differs) | `failed`, `failureReason = agent_adapter_unavailable` |
| The startup window expired | `failed`, `failureReason = agent_start_failed` |

Withdrawal and dispatch registration share the same transaction lock, so a registration arriving after the
withdrawal finds no work item. The run moves to `releasing` with delivery state `skipped`, queued user turns
become `discarded`, and the run Workspace's deletion is declared — all committed in one transaction.

Lack of capacity, queueing and a temporarily unreachable Node are not definitive refusals: the work item and
its input are kept unchanged and the existing claim scheduling defers them within the original window; Cloud
builds no queue or backoff of its own for this. An outdated Node capability record is never taken as
evidence of refusal either.

## Registered, but no startup record yet

If the window expires while the session has been registered as dispatched but no startup record has
arrived, Cloud **requests an end**: it releases an `EndSession{cancelled}` command, fixes the
end-confirmation window and records in the run result that startup is awaiting the Node; every execution and
resource fact is kept. An end reason the user gave earlier is preserved.

From there, two things can happen:

- **The real terminal state arrives in time**: it follows the normal delivery path (entering `delivering` and
  trying to save a Revision). A cancellation caused by the startup timeout is finally recorded as `failed`
  rather than `cancelled`.
- **The end-confirmation window also expires with the outcome still unknown**: one transaction writes the
  run as `failed` and `done`, the Thread as `ended` and the delivery state as `failed`, and inserts a row
  into the **residual ledger** `agent_run_residuals`. Cloud **never fabricates** a Node result, receipt or
  Revision.

When the end-confirmation window expires without a real terminal result, the same residual row
records `stop_evidence.stop_unconfirmed`: kind, source `cloud`, observation time and original execution
identity. Repeated scans neither duplicate nor renew it. It changes neither `reason`, `responsibility`
nor resource protection, and grants no termination, deletion or clearance authority. Node exposes its
own stop-confirmation bound and original Scope/Run timestamps through the existing execution query;
both sides correspond to the same execution/Workspace. Cloud's 2/6/5 windows retain their original
anchors and durations.

## The residual ledger

`agent_run_residuals` records a run that has ended as business but whose external execution and resources
may still exist:

- A ledger row records the tenant, Project, Workspace and runtime generation, plus the identities of every
  related execution, sandbox, Node, operation, effect and clone at that moment. It holds only resource
  identities and unfinished responsibilities — no credentials and no business input.
- The transaction that writes the row closes new dispatches, user input and model grants: Workspace
  admission is closed, runtime control goes to `reconciling`, and the run's model binding and any issued
  model grants are revoked. The Workspace is kept, and ordinary deletion and takeover are refused
  (`409 resource_in_use`).
- **Late facts only add evidence.** Real records and terminal states that arrive later are taken over and
  acknowledged under the original execution identity, so the Node stops replaying, but they never revive the
  run, never settle it again, and are never taken as proof the resources were deleted.
- **Reconnecting is not reclaiming.** A Node that reconnects can restore connection and readiness facts, but
  as long as the ledger row is not cleared the Workspace's admission stays closed; the same path restores
  admission as usual for normal runs without residuals.
- Authorized operators can query the ledger by `tenant_id` and `run_id` through a controlled database
  session. `session_ended_at` records a late real terminal state; it is evidence, not proof of deletion.
  `cleared_at` is reserved for a later, approved reclamation process and is never set automatically today.

A business `done` does not mean the physical resources were released. Automatic reclamation will only open
once a later reclamation decision defines authorization, real termination, fencing of late creations and the
order of deletion.

## Bounded abandonment of a blocked creation

A run can also wait forever before its Workspace is ready: the result of its `create_workspace` operation is unknown and the
operation stays `blocked`. A run Workspace is invisible to the public API, so nobody can retry it by hand, while the `blocked`
operation keeps holding the Project's only operation slot; so the wait can only end through Cloud's scan
(`GiveUpBlockedWorkspaceCreationsOnce`, every 10 seconds).

| Setting (`issue_runs.*`) | Default | Starts at |
|---|---|---|
| `agent_workspace_blocked_timeout` | 30 minutes | The transaction that first persists `blocked` for the creation operation |

- The deadline is frozen in the same transaction that first writes `blocked` (`workspace_blocked_at`, `workspace_deadline_at`,
  source `blocked_write`). Deferring, retrying, re-claiming, a Cloud restart or a configuration change never renews it; a temporary
  return to `running` or `retry_wait` does not erase it; a creation still queued without the Project's operation slot does not
  start it. The setting is validated like the startup windows.
- Creations already `blocked` before migration `0038` carry no immutable first evidence and are not backfilled; the first scan grants
  one default window from the time it observes them, recorded as `first_observed`, without claiming the original wait was being timed.
- When the deadline passes and the creation still has no real terminal result, one transaction does all of the following: it writes the
  residual ledger row (`reason = workspace_unavailable`, with the effect plans and the clone and plugin execution identities not yet
  reported); closes the Workspace's admission and bumps its epoch; revokes the model binding and grants; discards queued user turns;
  writes the run as `failed`, `done`, with `failureReason = workspace_unavailable` and delivery state `skipped`; and writes the original
  creation operation as `failed` (`provisioning_result_unknown`), which releases the Project's operation slot so other Workspaces of the
  same Project can proceed. A failure of any write rolls the whole transaction back; the slot is never released first.
- No delete is declared: deleting the residual Workspace, and Project deletes that involve it, are refused by the existing residual checks.
  Cancellation does not reset the window: a creation that really settles first follows the original cancel and release path, and one whose
  deadline passes first with the result still unknown settles as `failed`, `done`.
- **Late facts are recorded through `RecordProvisioningFact`.** After the abandonment, the Controller submits late real facts (an effect's
  Substrate result, the outcome of a clone or plugin execution, the creation's own result) under its current service identity and lease.
  Cloud checks them against the original operation, effect and execution identities and writes them idempotently into the original records
  and the ledger through the live paths' own validators before acknowledging: an identical fact replays as a no-op; a non-terminal
  observation such as `running`, `absent` or `pending` can be taken over by a later real fact, keeping the earlier observation as prior
  evidence; a recorded terminal accepts only its replay, and contradicting facts are refused. It never reopens admission, returns to
  `starting`, settles again or creates an execution; the operation's own actions (claim, defer, advance, plan, effect result) stay closed for
  an abandoned creation.

Physical termination, fencing of late ensures, authorization recycling and deletion order remain with a later recovery decision.

## Related decisions

- [Unknown creation outcomes and Agent startup both have waiting limits; business settlement keeps residual responsibility](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261012-bounded-agent-startup-and-undispatched-withdrawal.md)
- [An @Agent IssueRun on an Issue is orchestrated by Cloud](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
