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

**The subset Cloud can execute.** Publishing accepts only the node kinds Cloud can currently execute: `start`,
`agent`, `condition` and `output`. Other node kinds in the editor can still be edited and saved, but publishing
refuses them rather than freezing a graph whose nodes would be silently dropped. Agent node configuration fields
that Cloud does not execute are likewise refused by name, so an author never believes a setting is in effect when
no runner will apply it.

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
- Personal mappings for named slots are not implemented yet, so any graph referencing a named slot is currently
  reported as unconfigured; graphs referencing only `default` are judged by the initiator's default model.

## Runs

Workflows have three start entries, all sharing one admission gate:

| Entry | Produces |
|---|---|
| `POST .../workflows/:wfid/runs` | A WorkflowRun (`workflow_runs`) |
| `POST .../issues/:iid/runs` with `executorType = workflow` | A workflow IssueRun |
| Confirming an @workflow form on an Issue | A workflow IssueRun (see [Issue collaboration](../collaboration/interactions.en.md#form-mode-workflow)) |

**Admission** is decided once, in the caller's transaction:

1. Resolve the published version to execute: the one the caller named explicitly, otherwise the newest published
   version. With no published version at all the answer is `400 workflow_no_published_snapshot`, because executing
   the draft would let later edits change what an already-created run "executed".
2. Validate the input against that version's graph: required Start variables must have values of matching types;
   keys the graph does not declare are not checked (a run's input also carries the platform's own material).
3. Check executor slots (see above).

If any check fails, no run, claimed interaction or activity is left behind. The executed version is frozen onto the
run, so the run still knows which version it executed after the workflow is edited or published again.

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
- **Agent nodes currently fail the run with an explicit reason.** An Agent node needs a run Workspace and a real Node
  session, and that execution path is not wired yet; until it is, a run that reaches an Agent node fails immediately
  with a reason that names the gap, instead of hanging in `running` or reporting success for work that never happened.

Development fixtures (`collaboration.development_fixtures`) install a deterministic run simulator that fills in the
execution trace at run creation and says in its output that nothing really ran; production never installs it.
Workflow IssueRuns currently have no executor in production and stay `queued` (see
[Issue collaboration](../collaboration/interactions.en.md#ports-issues-own-only-the-consuming-side-contract)).

## Plugin asset import

Plugins of `kind = workflow` ship workflow templates as assets. When an administrator installs such a plugin for a
space, Cloud downloads the package **itself** and imports every `assets/workflows/*.json` in it as an editable
workflow of the tenant, creating its first published version at the same time:

- Import is triggered by the space's desired state, independent of whether any Node installed the plugin: a space with
  no live runtime still receives the templates.
- A background loop runs every `plugins.asset_import_interval` (30 seconds by default); downloading and parsing happen
  outside any transaction, and only the results are written in one short transaction. Downloads use the same checked
  outbound client and public-address policy as model-gateway and never follow redirects (see
  [Outbound policy](../models/model-gateway.en.md#outbound-policy)).
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

## Related decisions

- [Workflows are executed by a Cloud-side scheduler: WorkflowRun is the execution aggregate](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md) (approved)
- [Workflow agent nodes reference executors by executor slot; graphs no longer store Agent CLIs or model IDs](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261015-executor-slots-replace-agent-model-ids.md) (approved)
- [Plugin asset import produces published snapshots: the asset lifecycle never rewrites snapshots, and runs freeze only the snapshotId](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261016-asset-import-and-published-snapshots.md)
- [Workflow plugin assets are fetched by Cloud and imported document by document as editable copies](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
- [Personal slot mappings resolve a workflow's executors; each run pins one model binding per slot](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md) (approved)
