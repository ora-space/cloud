# Projects, Workspaces and Lifecycle Operations

English | [中文](workspaces.md)

A Project is a Git repository's development environment in Cloud; a Workspace is a **runtime** under a
Project: its own persistent data, its own clone of the code, and the sandbox and Node that run with
that data mounted. This document describes the shape of these aggregates and how the lifecycle
actions — create, start, stop, restart, delete — are carried out as durable operations by Cloud and
the Controller together.

Who may use a runtime, how a page holds exclusive control, and how force-stop works are in
[Runtime control](runtime-control.en.md); the Controller-side RPC contract is in
[Controller integration](controller-integration.en.md).

## Participants

```text
Cloud (authoritative state; decides the next step)
  ▲  gRPC: claim operations, record effect results, advance
  │
Controller (dials out only; performs external actions)
  ├──▶ Substrate (cluster's Sandbox Server): create and terminate sandboxes, delete Workspace data
  └──▶ Node (desktop's ora-node in the sandbox): clone, install plugins, prove idleness, Agent sessions
```

Cloud never calls the Substrate or a Node directly. It only stores intent and evidence and decides
from the evidence whether an operation may take its next step; external actions are performed by the
lease-holding Controller and count only once their results are reported back to Cloud. A Node holds
no Cloud credential and never talks to Cloud; its connection, heartbeat, end and idleness are reported
by the Controller on its behalf.

## Aggregates

**Project** (`projects`, merged with `repository_bindings` into one document when read)

- Belongs to a tenant and its sole Space. `owner_user_id` is the creator, kept as durable ownership
  and as a credential foreign key; it is **not** an additional access grant — every current member of
  the tenant can see and use the Project.
- The external repository is recorded on its single repository Binding: URL, default branch and an
  optional credential reference. Only `https` or `ssh` URLs are accepted, without query, fragment or
  embedded password. The default branch must be a concrete branch name, not `HEAD`, because Cloud
  never reads the remote repository and the Node only clones literal branches.
- `lifecycle`: `provisioning` (being created) → `active` → `deleting` → `deleted`. When the creating
  operation reaches a terminal state, success or failure, the Project becomes `active`: what failed is
  its main Workspace, not the Project. A deletion returns to `active` when a Node refuses to stop.

**Workspace** (`workspaces`)

| kind | Meaning |
|---|---|
| `main` | Exactly one per Project, created with the Project, never deleted on its own |
| `isolated` | A runtime a user creates for one task, one-to-one with `tasks` (the task title) |

- Each Workspace has its own persistent data, mounted as the Node's home by every generation of its
  sandbox. Stopping keeps the data; only deletion removes it. Workspaces share no volume, and there is
  no shared Project repository or linked worktree.
- `requested_ref` is the branch to clone; `base_commit_id` is the commit the clone produced. When
  creating an isolated Workspace, `baseRef` set to `HEAD` means the Project's default branch.
- `creator_user_id` is the user who actually requested the creation. Use of the runtime's content is
  judged by it, not by the Project owner (see [Runtime control](runtime-control.en.md)).
- State is described by three groups of fields:
  - `desired_state`: `running`, `stopped`, `deleted` — what the user wants;
  - `observed_state`: `provisioning`, `starting`, `ready`, `stopping`, `stopped`, `deleting`, `deleted`, `unavailable` — what Cloud has confirmed from evidence;
  - `admission_open` and `admission_epoch`: whether new executions are accepted; every closing of
    admission increments the epoch and invalidates older idleness evidence.
- `runtime_generation` is the runtime generation. It increments every time a sandbox creation is
  planned, and every Node report must belong to the current generation.

**Sandbox and Node instances**

- `sandbox_instances`: one generation of sandbox, whose `generation` matches the Workspace's runtime
  generation; a Workspace has at most one unterminated sandbox.
- `node_instances`: one Node process instance (`node_id` plus `node_incarnation_id`) on a sandbox; a
  sandbox has at most one unended instance.

**Run Workspaces.** Agent IssueRuns also use isolated Workspaces (`issue_run_id` set). Cloud creates
and deletes them under the identity of the user who triggered the run, through the same operations,
but they never appear in any public list or endpoint (see [Agent IssueRuns](../agent-runs/issue-runs.en.md)).

## Durable operations

Every lifecycle action is written as one `operations` row. The public endpoint returns `202` with that
row immediately, and the Controller carries it forward:

| `state` | Meaning |
|---|---|
| `queued` | Waiting for the Controller to claim it |
| `running` | Claimed and at some step |
| `retry_wait` | Paused; claimable again once `retry_at` passes |
| `blocked` | Paused; continues only when a user explicitly retries it (`POST /operations/:oid/retry`); Cloud never retries it automatically; a run Workspace creation is the exception and is abandoned by Cloud beyond its limit (see [Bounded startup](../agent-runs/startup-settlement.en.md#bounded-abandonment-of-a-blocked-creation)) |
| `succeeded`, `failed` | Terminal |

**A Project has at most one unfinished operation at a time** (`queued`, `running`, `retry_wait`,
`blocked`). New lifecycle requests return `409 operation_in_progress` meanwhile. This keeps a
Project's lifecycle serial; it does not provide conflict merging for several people editing the same
directory at once.

The steps each operation goes through:

| `kind` | Steps |
|---|---|
| `create_project`, `create_workspace` | `sandbox` → `node` → `clone` → `plugin` → `done` |
| `start` | `sandbox` → `node` → `plugin` → `done` |
| `restart` | `quiesce` → `terminate` → `sandbox` → `node` → `plugin` → `done` |
| `stop` | `quiesce` → `terminate` → `done` |
| `delete_workspace`, `delete_project` | `quiesce` → `terminate` → `cleanup` → `done` |
| `install_plugin`, `remove_plugin` | `plugin` → `done` |

The `plugin` step is skipped when there are no plugins to handle. What lets each step advance:

| Step | What the Controller does | Cloud advances when |
|---|---|---|
| `sandbox` | Plans and performs a `sandbox_ensure` effect to create this generation's sandbox | The effect succeeded and its result carries the sandbox ID and the `nodeId` the Node must present |
| `node` | Completes the handshake with the sandbox's Node and reports it registered | The current generation's unterminated sandbox has an initialized, connected Node with a heartbeat within 30 seconds, and its `nodeId` matches the ensure result |
| `clone` | Registers and dispatches one clone execution and takes over the Node's result | The latest clone succeeded on the current Node with a real 40- or 64-character commit, which is written to `base_commit_id` |
| `plugin` | Registers and dispatches the plugin install or removal execution | Every item in the frozen plugin set has a result. Item failures are recorded on the instances and do not keep the Workspace from becoming ready |
| `quiesce` | Asks the Node to check its own activity atomically and report idleness | No execution is unfinished, and when a sandbox is alive the Node has proven idleness for the current `admission_epoch` |
| `terminate` | Plans and performs `sandbox_terminate` | The effect succeeded with `terminated=true`, i.e. the old process is confirmed never to touch the storage again |
| `cleanup` | Plans and performs `workspace_data_delete` for each Workspace being deleted | Each effect succeeded with `removed=true`; it can only be planned once that Workspace has no unterminated sandbox |

"Workspace ready" (`observed_state=ready`, `admission_open=true`) commits in the same transaction as
the operation's success, and the Node is checked once more right before, so admission never opens on a
Node that has already gone away.

**Key rules.**

- **Clone retries are bounded.** After a Node reports a failed clone, the Controller defers with
  `clone_failed` and Cloud schedules a retry after 5 and 10 seconds for the first and second failure;
  every retry is a new clone execution. On the third failure the operation fails and the Workspace
  becomes `unavailable`. A clone whose outcome is unknown (`clone_result_unknown`) is never retried
  automatically and cannot be registered a second time.
- **Stopping requires proven idleness.** `stop`, `restart` and deletions first check in one transaction
  that no execution is unfinished, close admission and increment `admission_epoch`, and then ask the
  Node to report idleness. When the Node reports "not idle", the operation fails with
  `resource_in_use` and every Workspace returns to its state before the operation; a refused Project
  deletion also returns the Project to `active`, and no work is cancelled. No Node, a stale heartbeat or
  an unknown state never advances the step.
- **Generations fence old instances.** Before a new sandbox is planned, the old one must be confirmed
  terminated. Node registrations and reports are bound to the current generation and the live sandbox;
  late reports from an older generation get `409 stale_node` or `stale_sandbox`. The database can
  refuse late writes but cannot stop a process that is already running, so "confirmed terminated" is
  real external evidence; when it cannot be confirmed the operation stays `blocked` and no new
  generation is allocated.
- **A disconnected Node closes admission.** When the Controller reports a Node disconnected, a ready
  Workspace becomes `unavailable` with admission closed. When the same initialized Node reconnects the
  Workspace becomes available again, but a page control that was withdrawn does not come back with it.
- **Deleting a Project closes all its Workspaces at once.** Only administrators may delete. If any
  Workspace of the Project has activity or is held by a page, the whole deletion is refused; there is no
  partial deletion. Deletion starts only after syncs or publishes already running on the Project
  repository have ended (see [Git Server](../repositories/git-server.en.md)).

## Effects: durable intent for external side effects

Every action the Controller takes against the Substrate is first planned in Cloud as one
`external_effects` row and only then performed. The effect ID is the external idempotency key (the
`sandbox_ensure` effect ID is also the pre-allocated sandbox instance ID).

| kind | Purpose | Success evidence |
|---|---|---|
| `sandbox_ensure` | Create this generation's sandbox for the Workspace | Sandbox instance ID equal to the effect ID, plus `nodeId` |
| `sandbox_terminate` | Terminate the current sandbox | `terminated=true` |
| `workspace_data_delete` | Delete the Workspace's persistent data | `removed=true` |

- **Results cannot be rewritten.** Once registered an external ID cannot change, and a successful
  result cannot be replaced by another (`409 external_state_conflict`, `external_binding_conflict`).
- **Nothing is inferred from timeouts.** A failure or timeout only lets the Controller defer the
  operation to `retry_wait` or `blocked` with a finite error code; the effect's plan, external
  reference and current step are all kept. "The call timed out" is never taken to mean "nothing
  happened outside".
- **A new Controller reconciles first.** Each effect records the lease epoch that reconciled it. After
  the lease changes hands, effects from the old epoch must first be looked up at the Substrate by their
  stable ID and their observation re-recorded before planning and advancing continue (otherwise
  `409 reconcile_required`).
- The Substrate must support "look up by ID without creating", so the real object can be found after
  "succeeded but the response was lost". Termination must come with evidence that the old process will
  never touch the storage again; when unsure it does not report success.

## Public Workspace endpoints

| Endpoint | Description |
|---|---|
| `POST /tenants/:tid/projects` (or `/spaces/:spaceId/projects`) | Create a Project and its main Workspace; returns the `create_project` operation |
| `PATCH`, `DELETE /tenants/:tid/projects/:pid` | Rename; delete (administrators only) |
| `POST /tenants/:tid/projects/:pid/workspaces` | Create an isolated Workspace with a task title and `baseRef` |
| `GET /tenants/:tid/projects/:pid/workspaces` | List runtimes: those who may use one see its full content, other members see only a safe overview (state, generation, creator and so on) |
| `POST .../workspaces/:wid/start`, `/stop`, `/restart`, `DELETE .../workspaces/:wid` | Lifecycle actions; require the current page control. Only isolated Workspaces can be deleted |
| `GET`, `POST .../operations/:oid`, `.../retry` | Read an operation; retry a `blocked` or `retry_wait` operation |
| `GET /tenants/:tid/resource-status` | Administrators read a safe tenant-wide overview of runtimes, with no repository URLs, credentials or request results |

Starting requires a Workspace that completed initialization (has a `base_commit_id`) and is stopped.
Neither start nor restart clones again; both mount the existing data.

**Repository credentials.** A Project can reference one of the creator's own Git credential
references (`credential_refs`, which hold infrastructure references, never secret values) at creation.
Production has no executor that can resolve a credential by reference yet, so any new remote Git
operation that needs a credential is refused explicitly with
`409 repository_credential_executor_unavailable`; it never degrades to anonymous access or a global
credential. Anonymous public repositories work as usual.

The tenant-level `/tenants/:tid/clones` is an early, standalone clone request API. In production new
requests return `410 runtime_scope_required` and only historical reads remain; Workspace clones always
go through the `clone` step above.

## Related decisions

- [The runtime Workspace lifecycle follows the desktop Node](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/0-workspace-runtime-follows-desktop-node.md)
- [A Project must name a concrete default branch; Cloud never resolves HEAD from the remote](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260926-require-concrete-default-branch.md)
- [Ordinary stop protects activity; administrative force-stop records independent intent and waits for real termination](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260927-safe-stop-and-administrative-force-stop.md)
- [Workspace creation and start gain a plugin step executed by the Node](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260928-plugin-step-and-run-workspace-release.md)
- [Internal Control gRPC adds Workspace operations and Controller-reported Nodes](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md)
- [Projects belong to a Space: membership replaces personal ownership as the visibility and authorization boundary](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/0-project-space-scoping.md)
- [Repository credentials are managed as team, personal or unknown attribution](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20260927-repository-credential-continuity.md) (approved)
