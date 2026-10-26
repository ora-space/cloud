# Workflows

English | [中文](workflows.md)

A workflow is an editable directed-graph document within a tenant: nodes describe a multi-step Agent
collaboration and edges describe their order and branches. Cloud stores workflow documents and their
published versions, schedules runs on the Cloud side, and offers workflows as collaboration targets that can be
`@`-mentioned on Issues. This document covers documents and publishing, executor slots, runs and scheduling,
and importing workflows from plugins.

## Documents and publishing

| Endpoint | Purpose |
|---|---|
| `GET`, `POST /tenants/:tid/workflows` | List and create workflows |
| `GET`, `PUT`, `DELETE .../workflows/:wfid` | Read, save (versioned) and archive a workflow |
| `POST .../workflows/:wfid/publish` | Freeze the current document into the next published version |
| `GET .../workflows/:wfid/snapshots`, `.../snapshots/:snapshotId` | List and read published versions |
| `GET .../snapshots/:snapshotId/form` | The form projected from one published version |
| `PUT .../snapshots/:snapshotId/restore` | Replace the current document with a published version's graph |

- **Drafts and published versions are separate.** Saving only requires the body's graph to be a JSON object and
  checks nothing about completeness: a half-written draft is a legitimate document. Only **publishing** judges
  whether a graph can execute, returning problems together with the node or slot at fault so the editor can mark
  them. A frozen published version is a graph Cloud has agreed to execute.
- **Versions are immutable.** Published versions (`workflow_snapshots`) are stored under a per-workflow increasing
  version number and are never modified or soft-deleted, so the graph a run rendered never drifts with the document.
  Publishing locks the workflow row, so two concurrent publishes never get the same version number. A snapshot's
  name defaults to the workflow's.
- **Restore** replaces the current document with a version's graph and advances the document version like any other
  edit; it changes no published version.
- Workflow names are unique among a tenant's unarchived workflows (`409 workflow_conflict`). Archiving is a soft
  delete, and an archived workflow is hidden from reads together with all its versions and run history.

**The subset Cloud can execute.** Publishing accepts `aggregator`, `loop`, `loopExit`, `iteration`, `start`,
`agent`, `condition` and `output`. Other node kinds in the editor can still be edited and saved, but publishing
refuses them rather than freezing a graph whose nodes would be silently dropped. Agent node configuration fields
that Cloud does not execute are likewise refused by name, so an author never believes a setting is in effect when
no runner will apply it.

The publish dialog presents `workflow_graph_invalid` as a configuration check failure, listing the returned
nodes, fields or slots with corrective guidance rather than asking the author to keep retrying. For example,
Cloud does not execute structured output (`agentConfig.outputContract`, refused for any non-null value): the
editor explains this beside the switch and the publish dialog asks the author to turn off Structured output on
the rejected node. The draft retains its settings for the author to change; publishing never silently removes them.

**Reserved field.** `snapshotId` is the run-version key the platform owns in the `@` form, and an author cannot
declare it as a variable of their own. When a Start node's input variables or a global variable declare that name,
publishing and workflow asset import both refuse it, naming the node or field in the problem
(`reserved_field_name`), and freeze no published version; saving the draft is unaffected. Versions published before
this rule that declare the key themselves are neither rewritten nor deleted, but they no longer appear in the `@`
form's version selector. Until a live draft that still declares the key is republished, that author variable is not
projected into the `@` form either: whenever a selectable published version exists the form carries the
platform's version selector, and with none — never published, or every version withheld — it has no selector.
`snapshotId` on confirm is always the version cursor. Withdrawing the platform field through `launchFields` is a different request
and still works (see [Issue collaboration](../collaboration/interactions.en.md)).

**Interactive nodes are not supported in phase one.** Publishing rejects `human` nodes and `interactive: true` on
agent nodes, so a run never stops waiting for human input. The `workflow_runs.status` CHECK retains `awaiting_input`
as a reserved future value, but nothing writes it in phase one: it is absent from the public OpenAPI status enum
and never shown by the UI; cancellation still treats it as not cancellable, and IssueRun projection does not
advance on it. HITL is left to the later interactive-run-control decision. Publishing also rejects the interactive
deliverable declaration `agentConfig.completion` (any non-null value) and any `agentConfig.schemaVersion` other than
`3` — including `4`, the legacy `1`/`2`, and non-numbers: Cloud executes only version `3`, absent or `null` keeps the
old behavior, and the refusal names the node and field instead of silently dropping the declaration.

## Condition branches and named outputs

Condition cards expose a port for each case and the implicit ELSE branch. Connections persist the
case id in `sourceHandle`. Different branches may join the same node; duplicate endpoint-and-port
pairs are rejected. Deleting a case removes its connections, and undo restores both. Legacy
unlabeled condition connections retain ELSE semantics; case ordering never renumbers existing edges.

The Output inspector adds, names and removes `data.outputs` bindings, with sources selected from
the consumer's visible variable catalog. Execution resolves all bindings against one committed
snapshot and stores the named result on the output node. No bindings retain an empty object.
Explicit null, false, zero and empty strings remain assigned values. An unassigned binding or an
invalid path fails the node without publishing partial results. Publication rejects malformed
bindings, empty or duplicate names, invalid selectors and undeclared roots, with node/field locations.

**Upgrade compatibility:** strict bindings apply to every published snapshot, including new runs
from old snapshots and in-flight runs started before the upgrade and recovered afterward. There is
no execution-semantics version and historical snapshots are not rewritten. The former behavior of
always returning an empty object is not preserved: assigned bindings now produce named outputs,
missing values fail with `output_value_unset`, and invalid paths fail with `output_invalid_path`.
Old snapshots and recovered runs can therefore produce new failures or additional named outputs
after upgrading. No bindings still produce an empty object, and committed terminal results are not
recomputed. Publication admission cannot retroactively protect old snapshots; inspect their Output
bindings before upgrading.

## Executor slots

Agent nodes do not hard-code an Agent CLI or model ID in the graph; they reference an **executor slot**:

- `default` is a reserved slot every graph may reference directly, meaning the run initiator's personal default
  model; an Agent node that names no slot uses it. A graph may not redeclare `default`.
- A graph may declare named slots, with a description for the runner and the author's suggestion (`hint`). Slot
  names follow a fixed grammar, and descriptions are length-limited in characters.
- At run start every referenced slot is resolved against **the initiator's own** configuration, with no fallback to
  `default`, to the author's hint, or to anyone else's configuration. If any slot cannot be resolved the start is
  refused with `409 workflow_executor_unconfigured`, whose parameters list each slot, the nodes depending on it, the
  reason (such as `slot_not_configured`, `default_model_missing`, `credential_missing`) and the slot description, so
  the frontend can show everything that needs configuring at once.
- A named slot's mapping is each user's own configuration (`GET`, `PUT`, `DELETE /me/executor-slot-mappings/:slot`:
  slot name to agent CLI, model connection and model). `default` cannot be stored as a mapping — it is the personal
  default model itself, maintained separately in the personal model settings.

## Runs

Workflows have three start entries, all sharing one admission gate:

| Entry | Produces |
|---|---|
| `POST .../workflows/:wfid/runs` | A WorkflowRun (`workflow_runs`) |
| `POST .../issues/:iid/runs` with `executorType = workflow` | A WorkflowRun and the workflow IssueRun linked to it |
| Confirming an @workflow form on an Issue | A WorkflowRun and the workflow IssueRun linked to it (see [Issue collaboration](../collaboration/interactions.en.md#form-mode-workflow)) |

**Admission** is decided once, in the caller's transaction:

1. Resolve the published version to execute: the one the caller named explicitly, otherwise the newest published
   version. With no published version at all the answer is `400 workflow_no_published_snapshot`, because executing
   the draft would let later edits change what an already-created run "executed".
2. Validate the input against that version's graph: required Start variables must have values of matching types;
   keys the graph does not declare are not checked (a run's input also carries the platform's own material).
3. Check executor slots (see above).

If any check fails, no run, claimed interaction or activity is left behind. The executed version is frozen onto the
run, so the run still knows which version it executed after the workflow is edited or published again.

**A run's identity is frozen at creation.** All three entries share one creation function: a direct
start takes the Project from the request body's `projectId`, while Confirm/manual take it from the
Issue's `project_ref`. A missing Project is `409 workflow_run_project_required` for every entry;
an unusable, archived or foreign-space Project is refused as `404`, and one being
deleted is `409 resource_unavailable`. The run writes the initiating user (`created_by`) and that Project
(`project_id`) into its row and never changes them — a workflow document belongs to a tenant, not to a Project, so
there is nothing to default to, and the run's Workspace, its clone and its whole checkout land in the Project that
was named. A run Workspace belongs to the run that created it: `workspaces.workflow_run_id` is mutually exclusive
with an IssueRun's `issue_run_id`, and the `maintenance` reserved for it in runtime control is recorded against the
same run identity (see [Runtime control](../runtime/runtime-control.en.md)).

**Cancellation is a request, not a transition.** `POST .../workflows/:wfid/runs/:rid/cancel` records the request on
the run row and answers `202` with the instant it was recorded; repeating the request (even under a fresh idempotency
key) returns the same instant. The request writes only `workflow_runs.cancel_requested_at` — never the run's status,
node states, session or Workspace; the scheduler settles it on its next pass (see below). Runs that are `pending` or
`running` accept the request; a terminal run answers `409 run_not_cancellable`. Authorization follows the executor
bindings frozen on the run: on a bound run only the initiator and the tenant's administrators may cancel, and any
other member answers `403 workflow_run_cancel_forbidden`; an unbound run keeps the Issue Thread's member rule.

**The resume endpoint is registered but unavailable in this phase.** `POST .../workflows/:wfid/runs/:rid/resume`
validates the request against the final contract: `mode` is optional and is one of `keep` (the default), `node_files`
and `checkpoint`; an unknown field is `400 unknown_field` and an invalid value is `400 invalid_field_value`.
Authorization is cancellation's: on a bound run only the initiator and the tenant's administrators may call it, and
any other member answers `403 workflow_run_resume_forbidden`. Every valid request answers
`501 workflow_run_resume_unavailable` and writes nothing — no run state and no idempotency record — so a repeat is
the same refusal rather than a replay of a recorded success. A failed run's Workspace is already declared deleted,
leaving `keep` nothing to resume onto, so the frontend offers no resume entry point.

## Scheduling

WorkflowRuns are carried forward by a scheduling loop in the Cloud server (`AdvanceWorkflowRunsOnce`, every 10
seconds):

- Each pass recomputes the executable nodes from **committed state** only: the node states persisted on the run row
  plus the frozen published graph. A process crashing mid-pass loses nothing, and a repeated pass does nothing. Each
  run advances in its own short transaction and node results commit as a whole-row compare-and-set on the version, so
  a run has at most one advancer, late or duplicate advances have no effect, and a node without a committed result is
  never recorded as succeeded.
- `start`, `condition` and `output` are pure functions of committed state and are computed and committed inline in the
  scheduling transaction. `condition` activates only the outgoing edge for the chosen branch, and an outgoing edge
  with no branch label counts as `else`; nodes on branches not taken are marked inactive and never run. A run's graph
  is a directed acyclic graph with no loops or iterations.
- **An Agent node runs in a real session.** The first time a run schedules an Agent node, Cloud creates the run's
  Workspace (isolated, owned by this run) in the Project the run froze, and once that Workspace has a connected Node
  it takes the model binding frozen at creation for the slot the node references and hands the session work item to
  the Controller; the session's terminal state is the node's terminal state. One session runs at a time per run —
  a node on the next edge starts only after the previous one settles — and the wait before a session starts is
  bounded. The run Workspace's `plugin` step installs the Agent plugin release frozen at run creation, not the Space's
  current selection: an in-flight upgrade after creation does not change what the run executes, and a plugin removed
  after creation is not reinstalled — the start gate below fails the run as `agent_plugin_unavailable`. A Workspace
  that cannot be created fails the node as `workspace_unavailable`; a session that finds its
  frozen plugin version or slot binding missing fails as `agent_plugin_version_unfrozen` or
  `workflow_executor_unconfigured`. A ready Workspace whose run-frozen Agent plugin has no usable instance — absent,
  failed to install, or installed at another version — fails the node as `agent_plugin_unavailable` before any session
  is handed over, and the run reaches a terminal state too. None of these hang in `running` or report success for work
  that never happened.
  **When a session has started but its Node can no longer report**, the node settles failed as `node_unavailable`
  and the run reaches a terminal state: the judgement is the Node's reachability, not how long the session has been
  running — a heartbeat older than `issue_runs.delivery_unreachable_after` (30 minutes by default), or a sandbox gone
  together with the Node on it (exactly what a force stop leaves behind), both mean the Node is not there. That
  judgement and its window are the ones the IssueRun delivery and release paths already use
  (`runWorkspaceNodeUnknown`), so the two run kinds cannot answer "is the Node still there?" differently; an
  unconfigured window does not degrade into a zero-length one, and the session is held rather than failed on the
  spot. `node_unavailable` is Cloud's own diagnosis — it is also the published operation failure code for an
  unreachable Node — and not a session end reason a Node reported. **When a session has started and its Node is still
  reachable, but there is neither a committed result nor a successful `turnEnded` receipt**, the node fails as
  `session_turn_deadline_exceeded` once its execution row's `created_at` is older than `workflow.session_turn_timeout`
  (2 hours by default), and the run reaches a terminal state. That too is Cloud's own diagnosis, timed per execution row
  (a retry's new row re-times), using only the database clock — replay, restart and replica passes never reset it.
  Results win: a committed result settles by its own reason, an unreachable Node is still `node_unavailable`, and only
  when neither holds does the window apply. A result that arrives after the window already settled the run is kept on the
  execution row and never rewrites the terminal run. Once the run reaches a terminal state, Cloud declares the deletion
  of its Workspace. In the first phase this session has no Git Server
  access (reads and writes get `403`), and its Workspace is not listed in an IssueRun session's Workspace directory;
  see [Git Server](../repositories/git-server.en.md).
- **Agent-node retry is decided by the `agentConfig.retry` frozen on the node.** A session retries only when it ends
  as `agent_failed`, the policy is enabled and the budget is not spent: the budget is `maxRetries + 1` attempts
  (`maxRetries` ranges 0–5 and defaults to 2), and the next attempt starts after `initialDelaySeconds × 2^(n-1)`
  (`initialDelaySeconds` ranges 0–300 and defaults to 10, with a 600-second cap per wait). While it waits, the node
  commits `retrying`: the run stays `running`, the node keeps the failed attempt's `startedAt`, `error` and
  `output.reason`, records `retryAt` from the database clock and clears `finishedAt`; it keeps the run's single
  session slot until `retryAt` arrives. The new attempt has its own execution identity (`workflow_node_attempt`), so
  the previous attempt's result and end request are never reused. A spent budget, a disabled policy or a reason
  outside `agent_failed` fails the node with the last reason, and the run reaches a terminal state and releases its
  Workspace. At publish, `agentConfig.retry` must be a complete object; absent or `null` means the default policy,
  and unknown keys are ignored. A frozen historical snapshot is never re-gated, and a policy that cannot be parsed
  runs as the default.
- **A workflow session is single-turn, and Cloud decides when it ends.** Once the initial turn's `turnEnded`
  receipt arrives (with a `stop_reason` other than `cancelled`), the scheduler ends the session through the existing
  `EndSession` command relay; the session result the Node reports remains the node's only terminal evidence. If the
  session has no terminal state within the confirmation window after the end request was released, the node fails
  as `session_end_deadline_exceeded`, the run reaches a terminal state and its Workspace is released — no run stays
  `running` forever.
- **A cancellation request is settled by the scheduler, within a bound.** Once the request is recorded, a `pending`
  run no longer starts — it stamps no `started_at`, validates no graph, creates no Workspace and settles `cancelled`
  directly — and a `running` run dispatches no further node. An in-flight session keeps a committed result as the
  node's own terminal state (that result is what actually ran) while the run still settles `cancelled`; with no result
  yet the scheduler reuses that node's existing end request, or releases `end_session{cancelled}` when there is none,
  and the wait stays bounded by `issue_runs.agent_end_confirm_timeout`, after which the node and the run settle
  `cancelled`. A Node that can no longer report is a cancellation too, not `node_unavailable`. Every terminal path
  declares the run Workspace's deletion, and a result arriving after the run is terminal is kept on the execution row
  without rewriting the run.
- **A Workspace release is an independent fact after the terminal state, and it is bounded.** In the same transaction
  that settles the run `succeeded`/`failed`/`cancelled`, Cloud records the release state on the run row and declares the
  run Workspace's deletion: a run that never had a Workspace records `not_required`, every other run records
  `releasing`. The transaction that writes the delete operation `succeeded` and soft-deletes the Workspace records
  `released`, with that instant as the release fact; a Workspace deleted by another path — a project deletion, for
  example — is healed to `released` from the Workspace's own `deleted_at` instead of staying `releasing`. When the
  Workspace still holds a live sandbox whose Node has sent no heartbeat for longer than
  `issue_runs.delivery_unreachable_after` (30 minutes by default; an unconfigured window never gives up), the scheduler
  gives up the release: the unfinished delete operation fails as `node_unavailable`, the run records `abandoned` with
  its reason, the Workspace row stays readable, and no release instant is written. **Only `released` means Cloud
  observed the Workspace deleted**; `abandoned` is never reported as a successful release and is never re-declared or
  retried by a later pass. Replayed delete completions and repeated reconciliation passes are no-ops.

Development fixtures (`collaboration.development_fixtures`) install a deterministic run simulator that fills in the
execution trace at run creation and says in its output that nothing really ran; production never installs it.
A workflow IssueRun is linked through `workflow_invocation_ref` to the WorkflowRun created in the same transaction,
and the scheduling loop above advances that run. After a WorkflowRun status update affects a row, its state,
timestamps and errors are projected onto the linked IssueRun in that same transaction. An update affecting no rows
does not project; contract conflicts roll back the transaction. `awaiting_input` leaves the IssueRun unchanged,
and direct runs without a linked IssueRun create no collaboration records. Output summaries are `system`
`run.message` activities, bounded to 20000 bytes of valid UTF-8, never workflow-authored comments (see
[Issue collaboration](../collaboration/interactions.en.md#ports-issues-own-only-the-consuming-side-contract)).

## Plugin asset import

Plugins of `kind = workflow` ship workflow templates as assets. When an administrator installs such a plugin for a
space, Cloud downloads the package **itself** and imports every `assets/workflows/*.json` in it as an editable
workflow of the tenant, creating its first published version at the same time:

- Import is triggered by the space's desired state, independent of whether any Node installed the plugin: a space with
  no live runtime still receives the templates.
- A background loop runs every `plugins.asset_import_interval` (30 seconds by default); downloading and parsing happen
  outside any transaction, and only the results are written in one short transaction. Downloads use the same checked
  outbound client and the same deployment egress policy as model-gateway and never follow redirects (see
  [Shared deployment egress](../operations/deployment-egress.en.md)).
- The package location comes from the selected release in the catalog snapshot and is resolved at each claim rather
  than stored. Imports are fenced by **the claimed revision**: if the administrator reinstalls during the download, the
  stale attempt writes nothing and the next pass resolves the newer selection.
- **Per-document results**: a successfully imported path becomes a skip key and is never imported again; a failed path
  (unparseable, failing the publish gate, a name clash with an existing workflow `name_conflict`, JSON PostgreSQL
  cannot store verbatim, and so on) records a bounded failure code and is retried on the administrator's next trigger.
  A package-level download or extraction failure records only the package failure and writes no document results.
- **Administrators trigger retries**: installing the same version again retries failed documents, and installing a new
  version retries every path not yet imported successfully; copies already imported are never changed. Plugins
  installed before this feature existed are not backfilled; reinstalling is the administrator's explicit trigger.
- Import results are read separately through `GET .../spaces/:spaceId/plugins/assets`: installation results and asset
  import results are different facts.

Once imported, a workflow is an ordinary tenant workflow; the asset's lifecycle (plugin upgrade or removal) never
rewrites imported documents or published versions.

## Composite scopes and capacity

Publication resolves effective membership from non-empty `data.containerId`, falling back to `parentId`.
Body consumers read only region predecessors, the container's outer ancestors, globals / sys and current
carried or item / index inputs. Loop feedback, until and outputs read the completed body snapshot.
Iteration accepts at most 100 items.

Every composite state commit measures compact UTF-8 JSON, including every duplicated value.
Iteration inputs, collect values and member archives, and loop carried-plus-member archives and final
outputs, each have a 1 MiB limit. Complete `node_states` have an 8 MiB limit, with 64 KiB reserved for
bounded failure records. Overflow discards the entire candidate and retains the last committed ledger
and members, reporting `iteration_state_size_exceeded`, `loop_state_size_exceeded` or
`workflow_state_size_exceeded`; `continue` cannot absorb these failures.
Both iteration strategies record failed rounds. Terminal states retain the last started round and its
member projection. Inconsistent persisted progress fails as `iteration_invalid_round` without resetting
or replaying earlier rounds. Body agent execution attempts stay unique across rounds while retry budgets
count attempts within the current round.
The reserve opens only for newly failed/cancelled nodes, settlement of a new failed iteration ledger
entry, or a run transitioning to failed/cancelled. Historical failed members and ledger entries do not
enlarge ordinary writes. Re-reading identical committed state does not reject it against the ordinary
budget again; the local 1 MiB and total 8 MiB hard limits still apply. A body Agent turn timeout keeps the current
attempt running, persists its timeout diagnosis and requests `end_session{cancelled}`. Only that
attempt's terminal result permits failed-round settlement and iteration continuation; a late result
cannot replace the timeout diagnosis. If the end-confirmation window expires without terminal
evidence, the node reason becomes `session_end_deadline_exceeded` and the run error is
`session end deadline exceeded`. The run retains the failed round's ledger and archive, terminates and releases its Workspace
instead of starting another round while the old session may still be alive.

## Related decisions


- [Workflows are executed by a Cloud-side scheduler: WorkflowRun is the execution aggregate](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md) (approved)
- [Workflow agent nodes reference executors by executor slot; graphs no longer store Agent CLIs or model IDs](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261015-executor-slots-replace-agent-model-ids.md) (approved)
- [A workflow session ends after its initial turn by the Cloud scheduler, with a bounded wait on the end request](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261019-workflow-session-end-after-initial-turn.md)
- [Workflow agent nodes retry under a retry policy; the resume endpoint is registered against its final contract but unavailable in the first phase](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261021-node-retry-and-resume-contract.md) (approved)
- [Plugin asset import produces published snapshots: the asset lifecycle never rewrites snapshots, and runs freeze only the snapshotId](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261016-asset-import-and-published-snapshots.md)
- [Workflow plugin assets are fetched by Cloud and imported document by document as editable copies](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
- [Personal slot mappings resolve a workflow's executors; each run pins one model binding per slot](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md) (approved)
- [`snapshotId` is a reserved field: the publish gate refuses author declarations and stock versions are not selectable](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/20261021-snapshot-id-is-a-reserved-field.md) (approved)

- [variable-aggregator-execution](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261025-variable-aggregator-execution.md) (approved)
- [loop-and-loop-exit-execution](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261025-loop-and-loop-exit-execution.md) (approved)
- [iteration-foreach-execution](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261025-iteration-foreach-execution.md) (approved)
