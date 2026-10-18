# Agent IssueRuns: an Agent Run from Start to Finish

English | [中文](issue-runs.md)

When a user `@`-mentions an Agent in an Issue comment and gives it a task, the result is an Agent
IssueRun (`executor_type = agent`). For every run Cloud creates a disposable isolated **run Workspace**
(sandbox → Node → clone → plugins) and, once it is ready, starts an Agent session inside it. The
session's record is called the **Thread**; it streams back to the Issue page in real time and the user
can keep talking in it. When the session ends, the Node saves the code changes and the session record
as a **Revision**; Cloud verifies and registers it, then deletes the run Workspace and releases the
sandbox and its data.

The authoritative state of the whole orchestration lives in Cloud. The Controller and the Node only
perform work Cloud has released, and Cloud advances the run in the same transaction that takes over the
evidence they report (see [business hooks in the same transaction](../core/persistence.en.md#business-hooks-in-the-same-transaction)).
This document is the overview; individual parts are covered in:

| Part | Document |
|---|---|
| Creation (@Agent, the Project gate, the input snapshot) | [Issue collaboration](../collaboration/interactions.en.md#task-mode-agent) |
| Thread entries, user turns, idleness and ending | [Threads](threads.en.md) |
| Startup waiting limits and settling unknown outcomes | [Bounded startup](startup-settlement.en.md) |
| Object storage, upload grants, Revision registration and resume | [Revisions](revisions.en.md) |
| Session lists, archives and model request statistics | [Session reads](session-reads.en.md) |

## Phases

`issue_runs.phase` is the Agent run's durable state machine; the public `status` keeps its original
meaning and is derived from the phase. Phases only move forward, and every transition is written by
Cloud in the transaction that takes over the previous phase's evidence.

| phase | Entered when | Work Cloud releases | `status` |
|---|---|---|---|
| (empty) | The comment transaction creates the run | None | `queued` |
| `provisioning` | The dispatch loop has created the run Workspace and its `create_workspace` operation | The run Workspace's creation operation | `dispatched` |
| `starting` | The creation operation succeeded, the Workspace is ready, and the pinned Agent plugin is installed at its pinned version | The Agent session execution (an `agent_session` work item) and the Thread's first user turn | `dispatched` |
| `running` | The session's first Node record has been taken over | Thread commands: user messages, end requests | `running` |
| `delivering` | The session execution has a terminal result, whatever the end reason | The Revision delivery execution (a `deliver_revision` work item) | `running` |
| `releasing` | A Revision is registered, or delivery was given up, or the run was cancelled before any session | The run Workspace's `delete_workspace` operation | See below |
| `done` | The delete operation succeeded, or the delete side exceeded its limit | None | Unchanged |

- **A Project allows only one lifecycle operation at a time**, and run Workspaces obey this too. While
  the Project is busy the run stays `queued` until the next dispatch; creation and deletion operations
  are short and the session itself occupies no operation, so several runs of one Project only queue up
  while their Workspaces are created and deleted.
- When the creation operation fails the run goes to `releasing` with `status = failed` and
  `failureReason = workspace_unavailable`, and the delete operation cleans up whatever was created; when
  the Workspace is ready but the pinned Agent plugin was not installed, the run likewise goes to
  `releasing`, with reason `agent_plugin_unavailable`.
- When the creation operation stays `blocked` beyond its limit, the run settles as `failed`, `done` and keeps its residual
  responsibility, while the original creation operation fails and releases the Project's operation slot (see
  [Bounded abandonment of a blocked creation](startup-settlement.en.md#bounded-abandonment-of-a-blocked-creation)).
- **`done` does not mean the run Workspace was deleted**; only a successful delete operation does (see
  [the delete-side limit](#limits)).

A run Workspace belongs to exactly this run (`workspaces.issue_run_id` and `issue_runs.workspace_id`
reference each other uniquely). Its creator and operation actor are the user who triggered the run, it
is cloned from the Project's default branch, it never appears in any public Workspace list or endpoint,
and it accepts no other run or interactive session.

## Background loops

Besides the synchronous transitions taken while evidence is taken over, the Cloud server process runs a
set of loops every 10 seconds for the progress and settlement that have no external event to follow:

| Loop | Purpose |
|---|---|
| `DispatchQueuedAgentRunsOnce` | Create the run Workspace and its creation operation for `queued` runs and move them to `provisioning`; a busy Project waits for the next pass |
| `StartQueuedAgentSessionsOnce` | Write the Thread's first turn and release the session work item for `starting` runs; also checks the startup waiting limits |
| `EndIdleAgentThreadsOnce` | Release an end command when a Thread has been idle beyond the window |
| `ReactToCancelledAgentRunsOnce` | Release an end command for runs with a recorded cancellation request |
| `GiveUpBlockedWorkspaceCreationsOnce` | Abandon a run Workspace creation that has stayed `blocked` beyond its limit |
| `GiveUpStaleDeliveriesOnce` | Give up delivery beyond its limits and move to `releasing` |
| `RedeclareRunWorkspaceDeletesOnce` | Re-declare the delete for a `releasing` run whose delete operation is no longer in flight, e.g. after a Node refused to stop |
| `GiveUpStaleWorkspaceReleasesOnce` | End a run as `done` when the delete side exceeds its limit |

They all have the same shape: one read-only scan outside the global lock (at most 100 runs per pass,
ordered by creation time and ID), then one short transaction per run that re-reads and transitions with
a compare-and-set. A repeated pass never produces a second command, a second operation or a second
transition; a run that is temporarily stuck simply rolls back and waits for the next pass, without
flooding the logs.

## Execution result and saved output are recorded separately

A run's final `status` only states the execution result. It is derived from the session's end reason,
written together with the phase on entering `releasing`, and accompanied by a timeline activity:

| Session end reason | `status` |
|---|---|
| `user_ended` (the user ended it), `idle_timeout` (the idle window expired) | `completed` |
| `cancelled` (the run was cancelled) | `cancelled`; a policy cancellation caused by a startup timeout is recorded as `failed` |
| `agent_failed` (the Agent failed), `interrupted` (the Node was interrupted or the outcome is unknown) | `failed` |

Whether the output was saved is stated by `result.deliveryState`: `saved` (a Revision with changes was
registered), `unchanged` (a Revision without changes was registered), `failed` (delivery was given up),
`skipped` (the deployment has no object storage, or the run was cancelled before any session). `failed`
and `cancelled` runs still try to save their output and can have a Revision too.

On the timeline an Agent run has one `run.enqueued` activity, and on entering `releasing` a
`run.completed`, `run.failed` or `run.cancelled` follows, carrying both the execution result and the
delivery state.

## Cancelling and ending are requests

When the user ends the session, the idle window expires or the run is cancelled, Cloud only records the
request and releases an `EndSession{reason}` command to the Node; session shutdown and delivery proceed
as usual, because destroying the Workspace immediately would lose the changes the Agent already made.

- User end: `POST .../runs/:rid/thread/end` (see [Threads](threads.en.md#ending)).
- Idle expiry: the Thread has been idle longer than `issue_runs.thread_idle_timeout` (15 minutes by
  default), judged by database time.
- Cancellation: the handling path is in place (`issue_runs.cancel_requested_at`), but there is no public
  cancel endpoint yet. A run cancelled before its session starts goes straight to `releasing` and ends as
  `cancelled` with delivery state `skipped`.

## Limits

| Stage | Rule | Default (setting) |
|---|---|---|
| Creation | After the run Workspace's creation operation first goes `blocked`, wait for its real result | 30 minutes (`agent_workspace_blocked_timeout`), see [Bounded startup](startup-settlement.en.md#bounded-abandonment-of-a-blocked-creation) |
| Startup | After the run Workspace is ready, wait for the session's first record; restoring a prior Revision uses a longer window; after an end request is released, wait for confirmation | 2 minutes, 6 minutes, 5 minutes (`agent_start_timeout`, `agent_restore_timeout`, `agent_end_confirm_timeout`), see [Bounded startup](startup-settlement.en.md) |
| Delivery retries | A failed delivery is retried with a new execution, waiting 30 seconds and doubling each time up to 10 minutes; the run stays `delivering` and the sandbox stays up | — |
| Giving up delivery | When delivery has failed continuously since the first delivery work item beyond the limit, or the run Workspace's Node has been in an unknown state beyond the limit, delivery is given up: `deliveryState = failed`, the run goes to `releasing`, and this round's unsaved changes are lost | 2 hours (`delivery_give_up_after`), 30 minutes (`delivery_unreachable_after`) |
| Giving up deletion | Conditioned only on unreachability: a live sandbox remains and its Node has been in an unknown state beyond the same unreachability window. A Node that explicitly refuses to stop is still reachable and does not trigger it | 30 minutes (shared with delivery) |

When deletion is given up, the delete operation ends with `node_unavailable`, the run goes to `done`,
`status` keeps the value written on entering `releasing`, and `failureReason = workspace_unavailable` is
recorded. The run Workspace row is **not** soft-deleted and still points at the run; its sandbox and data
are not reclaimed automatically and can only be handled by operators, and the public API always answers
`404` for deleting a run Workspace. All times are compared on the database clock.

## Inputs fixed before the run starts

A run's behavior is decided by snapshots fixed at creation and at start; later changes do not affect a
run already under way:

- **Agent plugin**: the plugin's identity and its then-desired version are written into the run input at
  creation, so a later plugin upgrade does not change this run.
- **Personal model**: a run of `official/ora-space.opencode` gets a run binding generated in the creating
  transaction from the triggering user's default model (see
  [Personal model connections](../models/model-access.en.md)); without a default model, a key or a usable
  connection the whole comment rolls back with `409 model_default_required`, `model_credential_required`
  or `model_connection_unavailable`.
- **Git commit identity**: the triggering user's Git author identity is fixed together with the model
  binding, and the Agent commits as that identity.
- **Prior Revision**: at session start the most recent resumable Revision of this Issue is chosen and fixed
  (see [Revisions](revisions.en.md#resuming-a-prior-revision)).

The triggering user must stay able to use the runtime throughout the run: once they are no longer a
current member or their account is disabled, later dispatches and Git access are refused.

## What is publicly visible

`GET .../issues/:iid/runs` and `.../runs/:rid` return the run resource. Control fields such as `phase`,
the run Workspace and the startup windows never appear in public documents; Agent runs carry two extra
projections:

- `preparation`: preparation progress, with `stage` one of `waiting`, `environment`, `clone`, `plugin`,
  `start`, `failed` and `cancelled`, plus the clone attempts used and their limit (3), the next retry time
  and a safe error code; no Workspace, Node, repository URL, credential or raw error is exposed.
- `revision`: metadata of the registered Revision (see [Revisions](revisions.en.md#public-reads)).

## Related decisions

- [An @Agent IssueRun on an Issue is orchestrated by Cloud](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
- [A new run on the same Issue first resumes its latest Revision, then starts the Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [An Agent run's conversation is recorded as a Thread](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/0-durable-agent-thread-with-user-turns.md)
- [Workspace creation and start gain a plugin step; Cloud creates and deletes IssueRun run Workspaces](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260928-plugin-step-and-run-workspace-release.md)
- [Users can configure a Git commit identity in Cloud](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260928-user-git-commit-identity.md)
