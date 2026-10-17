# API Reference — for agents

Every route in the allowlist (`router.Routes()`). All public routes are tenant-scoped and require
service-JWT (gateway) + user-JWT + active membership. Error codes: see
[architecture.md](architecture.md) §Errors. Conventions (idempotency, version, pagination): see
[architecture.md](architecture.md) §Core conventions.

Legend: 🔑 = `Idempotency-Key` required · 🔢 = `version` precondition (428/409).

## Identity & tenancy

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/api/v1/me` | — | User |
| GET | `/api/v1/me/tenants` | — | `{items, nextCursor}` |
| GET | `/tenants/{tid}/members` | — | `{items, nextCursor}` |
| PUT | `/tenants/{tid}/members/{uid}` | `role`, `status`, 🔢`version` | membership (admin only) |

## Workspace members (`/spaces/{sid}/members`)

`SpaceMember`: `id, workspaceId, userId, role, status, version, displayName, joinedAt`.
`role ∈ owner|admin|member` (schema-level); `status ∈ active|disabled`.

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/spaces/{sid}/members` | — | `{items:[SpaceMember], nextCursor}` (workspace members) |
| POST 🔑 | `/spaces/{sid}/members` | `email`* | SpaceMember (admin or owner; fixed `member` role; unknown email → 404 `user_not_registered`; idempotent re-add) |
| PUT | `/spaces/{sid}/members/{uid}` | `role`, `status`, 🔢`version` | SpaceMember (**owner-only**) |
| DELETE 🔑 | `/spaces/{sid}/members/{uid}` | 🔢`version` | SpaceMember (**owner-only**; hard delete) |

### Access model (Workspace member management, Step 3A)

- **Change role — owner-only** (`PUT`): the actor must be the workspace **owner** (admin/member → 403
  `space_role_required`). Only `admin↔member` transitions are allowed: the **owner role is immutable**
  through this API — any write targeting an owner row, or any transition *to* owner (member→owner,
  admin→owner, owner self-demote) → **409 `ownership_transfer_not_supported`**. Ownership transfer is a
  separate, not-yet-designed feature; the last-owner invariant is guaranteed by owner-immutability (a
  workspace always has exactly one owner).
- **Remove member — owner-only** (`DELETE`): the actor must be the workspace **owner**; removing the
  owner (including self-removal) → **409 `cannot_remove_workspace_owner`**. Removal is a **hard delete of
  the workspace membership row only** — the user account, their tenant membership, and any resources they
  created (e.g. `projects.owner_user_id` keeps recording the creator) are untouched and remain in the
  workspace. The removed member's workspace access is revoked naturally: `listSpaces`/`spaceMember`/
  `workspaceRole` are all backed by the active membership row, so after removal the workspace, its
  projects, and their runtime workspaces become invisible (404) — with no creator backdoor, since
  space-scoped resource access first requires active workspace membership.
- **Add member — admin or owner** (`POST`): unchanged from the enrollment step; unknown email → 404
  `user_not_registered`; re-adding an existing member is idempotent (returns the existing row without
  mutating role/status/version). A disabled membership can be toggled back via `PUT` (`status`).
- Both `PUT` and `DELETE` need the current `version` (428/409 on conflict); `POST`/`DELETE` need an
  `Idempotency-Key`.

## Issues (board)

`Issue`: `id, tenantId, creatorUserId, assigneeType, assigneeId?, assigneeUserId?, parentIssueId?,
projectRef?, title, description, status, priority, position, number, properties, labels[], version,
createdAt, updatedAt, deletedAt?`. Non-`user` `assigneeType`/`assigneeId` and `projectRef` are opaque
refs — shape-validated, not resolved this wave.

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/issues` | query `?q=` | `{items, nextCursor}` |
| POST 🔑 | `/issues` | `title`*, `description`, `status`, `priority`, `assigneeUserId`, `assigneeType`, `assigneeId`, `parentIssueId`, `projectRef`, `properties` | `{resource: Issue}` |
| GET | `/issues/{iid}` | — | Issue |
| PUT | `/issues/{iid}` | any create field + 🔢`version` | Issue |
| DELETE 🔑 | `/issues/{iid}` | 🔢`version` | Issue (soft-deleted) |
| POST 🔑 | `/issues/{iid}/move` | `status`, `beforeId`, `afterId`, 🔢`version` | Issue |
| POST 🔑 | `/issues/batch` | `ids`*[≤100], `status`, `priority`, `assigneeUserId` | `{items, nextCursor}` |
| GET | `/issue-groups` | query `?by=status\|priority\|assigneeUserId`, `?q=` | `{groups:[{key,items}]}` |

## Issue extensions (wave 2)

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/issue-statuses` | — | `{items:[IssueStatus]}` (auto-seeds 7) |
| POST 🔑 | `/issue-statuses` | `key`*, `name`*, `description`, `category`, `color`, `icon` | `{resource: IssueStatus}` |
| PUT | `/issue-statuses/{sid}` | `name`, `description`, `category`, `color`, `icon`, `position`, 🔢`version` | IssueStatus |
| DELETE 🔑 | `/issue-statuses/{sid}` | 🔢`version` | IssueStatus (409 if system) |
| GET | `/labels` | — | `{items:[Label]}` |
| POST 🔑 | `/labels` | `name`*, `color` | `{resource: Label}` (409 dup) |
| PUT | `/labels/{lid}` | `name`, `color`, 🔢`version` | Label |
| DELETE 🔑 | `/labels/{lid}` | 🔢`version` | Label |
| GET | `/issue-views` | — | `{items:[IssueView]}` (owner-scoped) |
| POST 🔑 | `/issue-views` | `name`*, `filter`(object) | `{resource: IssueView}` |
| PUT | `/issue-views/{vid}` | `name`, `filter`, 🔢`version` | IssueView |
| DELETE 🔑 | `/issue-views/{vid}` | 🔢`version` | IssueView |
| GET | `/issues/{iid}/comments` | — | `{items:[Comment]}` |
| POST 🔑 | `/issues/{iid}/comments` | `body`*, `parentId` (threading) | `{resource: Comment}` |
| PUT | `/issues/{iid}/comments/{cid}` | `body`, 🔢`version` | Comment |
| DELETE 🔑 | `/issues/{iid}/comments/{cid}` | 🔢`version` | Comment |
| GET | `/issues/{iid}/labels` | — | `{items:[Label]}` |
| POST 🔑 | `/issues/{iid}/labels` | `labelId`* | `{resource: Label}` |
| DELETE 🔑 | `/issues/{iid}/labels/{lid}` | — | Label |
| GET | `/issues/{iid}/subscribers` | — | `{items:[User]}` |
| POST 🔑 | `/issues/{iid}/subscribers` | `userId`* | `{resource: User}` |
| DELETE 🔑 | `/issues/{iid}/subscribers` | `userId`* | `{resource: User}` |

## Issue collaboration (Wave 3A)

`IssueRun`: `id, tenantId, issueId, version, executorType, executorId, status, externalExecutionId?,
executionContextRef?, workflowInvocationRef?, triggerEvidenceKind, triggerEvidenceRefId?, parentRunId?,
retryOfRunId?, rerunOfRunId?, delegatedFromRunId?, attempt, maxAttempts, input, result?, error?,
failureReason?, triggerSummary, queuedAt, dispatchedAt?, startedAt?, completedAt?, fireAt?,
leaseExpiresAt?, createdAt, updatedAt, deletedAt?`. `executorType ∈ agent|team|workflow` (opaque ref,
unresolved); `status` is an SQL-WHERE-guarded 7-state machine
(`queued/dispatched/running/completed/failed/cancelled/deferred`).

`ContextRef`: `id, tenantId, issueId, refType, refId, createdAt`.

Wave 3A also extended `Comment` with `authorType`/`authorId`/`parentId`/`seq` (uniform author ActorRef,
threading, shared per-issue timeline `seq`).

**Still schema-ready / not API-implemented** (the DB accepts, the HTTP surface does not):

| Capability | State |
| --- | --- |
| `issue_comments.author_type = system` | CHECK allows `system`, but **no** code path writes a `system` comment yet (agent/team replies now go through the internal run-reply path, §Interaction). |
| `ConversationTarget` | **Not implemented** — left open by the Issue collaboration ADR. |

**Implemented by Wave 3B-1** (migration `0009` — formerly `0008`, renumbered in the workspace integration; semantics in the
[Issue collaboration ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)):

- `Comment.authorType` is no longer hard-coded `user`: mock agent/team runs post a reply comment with
  `author_type='agent'`/`'team'` through an **internal** path (no public impersonation — clients cannot
  set `authorType`).
- `issue_activities` is now readable: `GET /issues/{iid}/timeline` returns Comment + Activity merged in
  one shared per-issue `seq` order.
- `Comment.targets[]` is implemented on comment create (see the Interaction section below).
- `GET /collaboration/targets` exposes the `@`-picker projection (*not* an Agent/Team/Workflow API).

The integration ports are inventoried in that ADR's D1.

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/issues/{iid}/runs` | — | `{items:[IssueRun]}` |
| POST 🔑 | `/issues/{iid}/runs` | `executorType`*, `executorId`*, `input`(object) | `{resource: IssueRun}` (`status=queued`; pending-executor dup → 409 `pending_run_exists`) |
| GET | `/issues/{iid}/runs/{rid}` | — | IssueRun |
| GET | `/issues/{iid}/context-refs` | — | `{items:[ContextRef]}` |
| POST 🔑 | `/issues/{iid}/context-refs` | `refType`*, `refId`* | `{resource: ContextRef}` |
| DELETE 🔑 | `/issues/{iid}/context-refs/{crid}` | — | ContextRef (hard delete) |

There is **no** `DELETE /issues/{iid}/runs/{rid}` — terminal runs are history, not deletable.

## Issue collaboration interactions (Wave 3B-1)

The `@` collaboration spine. `POST /comments` now accepts an optional `targets[]` array of
`{type, id, task?}`; each target becomes one persisted `Interaction` row and, for `agent`/`team`,
drives a mock `IssueRun` via the ExecutionDispatcher port. `user` = **Mention Mode** (no run);
`agent`/`team` = **Task Mode** (`task` required → 400 `task_required`); `workflow` = **Form Mode**
(rejected with 409 `workflow_not_available` this wave). Clients **cannot** set `authorType`.

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/collaboration/targets` | query `?q=` | `{items:[CollaborationTargetSummary]}` (`{type,id,displayName,description,interactionDescriptor}`) |
| GET | `/issues/{iid}/timeline` | — | `{items:[TimelineEntry]}` (Comment + Activity merged by shared `seq`) |
| GET | `/issues/{iid}/interactions` | — | `{items:[Interaction]}` (`{id,commentId,targetType,targetId,mode,task,runId,input,createdAt}`) |
| POST 🔑 | `/issues/{iid}/comments` | `body`*, `parentId`?, `targets[]`? (`{type,id,task?}[]`) | `{resource: Comment}` (creates interactions + mock runs as a side effect) |

Consumers today: `/collaboration/targets` feeds the frontend `@` picker; `/timeline` feeds the
frontend Activity panel; `/interactions` has **no UI consumer yet** — its current callers are the
integration tests (they assert the comment→interaction→run cardinality) plus debugging. It is kept
because it is the read surface for the spine table; the frontend hook `useInteractions` exists but is
unused. Widening it later must stay additive.

### Workflow interaction (Wave 3B-2, migration `0010` — formerly `0009`, renumbered in the workspace integration)

Form Mode. `POST /comments` with `targets:[{type:"workflow", id}]` now records a `mode='form'`,
`runId=null` interaction and **creates no run**; only an explicit confirm does. `409
workflow_not_available` (3B-1) is **SUPERSEDED**.

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/collaboration/forms/{formRef}` | query `?issueId=` | `FormDescriptor` (`{formRef,title?,description?,fields[]}`) |
| GET | `/workflows/{wfid}/snapshots/{snapshotId}/form` | query `?issueId=` | `WorkflowSnapshotForm` (`{snapshotId,formRef,title?,description?,fields[]}`) |
| POST 🔑 | `/issues/{iid}/collaboration/assist` | `targetId`*, `values`(object) | `{suggestedValues, suggestedContextRefs, explanations?}` — **stateless, suggest only, no side effects** |
| POST 🔑 | `/issues/{iid}/interactions/{ixid}/confirm` | `values`(object)*, `contextRefs`(array of `{refType,refId}`) | `{resource: IssueRun}` (the initial run, `status=queued`; dispatch is post-commit) |

- `formRef` is an **opaque** provider token (not a UUID) and appears in a path segment.
- `?issueId=` is **optional** and additive. Omitted (or empty) ⇒ the descriptor is exactly the one this
  route served before the platform fields existed. Present ⇒ the descriptor is tailored to that issue
  and the platform fields are **prepended**, in this order:

  | key | label | type | required | default | injected when |
  | --- | --- | --- | --- | --- | --- |
  | `repository` | 仓库地址 | `text` | ✅ | issue's `project_ref → projects.repository_url` | always |
  | `branch` | 分支 | `text` | ✅ | the same project's `default_branch` | always |
  | `snapshotId` | 运行版本 | `select` | — | newest `workflow_snapshots` row | the workflow has ≥1 published snapshot |
  | `prompt` | 提示词 | `textarea` | — | Start node's 初始提示词; blank means "use the workflow's own prompt" | always |
  | `context_refs` | 补充上下文引用 | `multi_select` | — | none (empty = add nothing) | the issue has a project or a parent |

  `snapshotId` offers snapshot **ids** (a run is created from an id, and the id keeps pointing at the
  same frozen document); the version number rides in the label as `v3 · <snapshot name>`. The key was
  called `version` until it was renamed to say what its value is; a submission still carrying the old
  key is read as the selection it was (Issue collaboration ADR D8). An unpublished
  workflow has nothing to offer and the field is **dropped** rather than injected empty — the contract
  has no way to express a select with no choices. `context_refs` is **additive**: the issue's own
  persisted references are already on every run it starts, so its options are the references the issue
  can name for itself (`project:<id>`, `parent_issue:<id>`) and it opens on nothing selected.
  A workflow that already declares one of these keys keeps its own label/type/requiredness and only
  gains a default when that default validates against its control. Unknown or foreign issue ⇒ 404.

  That table is the platform's **catalogue**, not the form: the workflow's author declares which of
  those fields their workflow's form asks for, and which of them it insists on (Issue collaboration ADR D8). The
  declaration rides in the graph envelope as `launchFields`, a sibling of `globalVariables`, and is
  read **per key** — `[{"key":"snapshotId","enabled":false},{"key":"prompt","required":true}]` turns off
  the version choice and makes the prompt mandatory, and every key it does not mention keeps the
  catalogue's own answer. That is what keeps a workflow declaring nothing rendering exactly the table
  above, and a field added to the catalogue later appears for every workflow without re-saving one.
  `enabled: false` removes the field from the descriptor (`repository` and `branch` included), after
  which a value for that key is an **unknown key** on both `assist` and `confirm` — they re-resolve
  the same declaration — and AI Assist can no longer suggest it. `required` overrides the catalogue's
  answer in both directions and is enforced server-side by `confirm` (400 `required_field_missing`).
  Two rules outrank the declaration: a field with **nothing to offer** is dropped whatever the author
  asked for (`snapshotId` with no published snapshot, `context_refs` for an issue that names no
  reference), and a key the author declared as a **Start variable of their own** is theirs — the
  declaration is inert for that key, control and requiredness alike. Edited in the workflow editor's
  `@ 表单字段` dialog, which writes all five keys with both answers spelled out; it takes effect on the
  next descriptor read, because the projection reads the live graph rather than a published snapshot.
  A snapshot published before the rename still declares `version`; that entry is read as the
  `snapshotId` entry its author wrote, so a field they turned off stays off rather than coming back.
- The graph's `globalVariables` are projected **after** the author's Start variables, one optional field
  each, keyed and labelled by the variable name, defaulted from its declared `value`. `number`/`integer`
  → `number`, `boolean` → `boolean`, `string`/`secret` → `text`, everything else (arrays, objects, files)
  → `textarea` holding JSON. A name without a `.` is not a global and is dropped.
- The three routes must agree: `assist` and `confirm` re-resolve the descriptor with the **same** issue
  context, so a client that fetches with `?issueId=` must confirm against that same issue (a client
  rendering the un-injected form gets 400 `required_field_missing` on confirm). They agree on the
  *context*, not on which document they project: GET and `assist` read the **live** graph, while
  `confirm` reads the **version** it is about to execute — the two describe the same fields only while
  the draft has not moved on from that version, which is what the divergence check below is for.
  `prompt`, `repository`, `branch`, `snapshotId` and `context_refs` are **reserved keys** — an executor
  that validates run input against Start variables must map them explicitly. `context_refs` is reserved
  in a second sense: the surface reads its value back out (a list of `refType:refId` strings) and turns
  it into the run's `contextRefs`, in addition to whatever AI Assist applied. `snapshotId` carries a second
  reservation: it is the platform's version cursor only while the author's own Start node declares no
  variable under that name — a workflow that declares one keeps the key, gets no version selector
  injected, and the value the user typed is that variable's rather than a snapshot choice. The cursor
  also reads the pre-rename `version` key, under the mirror of that rule: it selects the version only
  while the author's Start node declares no variable named `version`.
- A caller that picks the version **before** rendering it fetches
  `GET /workflows/{wfid}/snapshots/{snapshotId}/form` instead: the same projection, made from that
  version's frozen graph, with `snapshotId` echoed in the response so the form says which version it
  describes (`formRef` stays the workflow's own id). The echo is what a version-first caller submits
  rather than a label: the injected selector keeps its own default, the newest published version, so
  rendering an older version's projection without selecting the echoed id would confirm the newest one.
  It is the projection `confirm` validates against, so once the selection matches the projection, what
  the form renders and what the run executes cannot drift. The workflow editor's manual-run entry — the
  caller that picks a version before rendering — is what this read is being built for. Scope is the
  direct-run entry's: another tenant's or another workflow's snapshot, an
  unknown or malformed snapshot id, and an archived workflow are all 404 `not_found`. It is a **read,
  not a start** — publish state, archiving and per-slot executor resolution are still answered by
  `POST …/runs`, so a readable form can still be a refused start. `assist` and the live
  `GET /collaboration/forms/{formRef}` are unchanged and still project the live graph, because a caller
  that has not chosen a version has no version to name.
- `values` is a plain `{fieldKey: value}` map, re-validated server-side against the descriptor of the
  **version being confirmed** rather than the live document: unknown key / wrong type / value outside
  `options` → 400 `invalid_field_value`; missing required → 400 `required_field_missing`. The version is
  the `snapshotId` field's value (a snapshot id) or the newest published one when that field is unset; a
  version that is not this workflow's is 404. A submission that answers under the pre-rename `version`
  key instead is read the same way and stored under `snapshotId`, so a form rendered before the rename
  and submitted after it still runs the version its user picked. A workflow with nothing published
  cannot be confirmed:
  with no version selected it is 400 `workflow_no_published_snapshot`, because there is no frozen
  document to execute and running the live draft is what the draft/published split forbids. Confirm
  goes through that same gate as every other workflow start, and so also refuses — before the values
  are looked at, because nothing would run — a start the confirming user cannot staff: a graph
  referencing an executor slot they have no model for is 409 `workflow_executor_unconfigured`, with
  one entry per gap in `params.slots` (`slot`, `nodeIds`, `reason`).
- **The version's schema is the submission's schema only while it still describes the form the client
  rendered**, and the form is rendered from the live document. When an author edits between publishing a
  version and confirming against it, the two can disagree about which fields exist, which are required,
  and which values a select offers — in either direction: a field the version rejects as unknown, or one
  it insists on that the form never showed. `confirm` refuses that with 409
  `workflow_version_fields_changed` and `params.fields` naming the keys, instead of blaming a value the
  user was legitimately given; the answer is to publish again or select another version. Only the fields
  that judge a submission are compared, so a changed label, description, placeholder or default is not a
  divergence.
- Confirm claims the interaction with a compare-and-set on `run_id IS NULL`: a second confirm (different
  key) → 409 `interaction_already_confirmed`; the same key + same body replays the stored response.
- Confirm on a non-`form` interaction → 409 `interaction_not_confirmable`.
- **Assist is stateless and takes a `targetId`, not an interaction**: the form is a *draft* until the
  user confirms it, so assist must work before any interaction exists and must write nothing. It
  re-validates `values` against the live graph's descriptor, and reads the pre-rename `version` key
  the same way `confirm` does — a form rendered before the rename is accepted here rather than refused
  as an unknown key, because the two entries validate one submission and cannot disagree about its
  keys. An unknown/non-workflow target → 404 `target_not_found`.
- Capability states: `503 form_descriptor_unavailable` / `503 assist_unavailable` (port not wired);
  `404 form_descriptor_not_found` (unknown `formRef`); `500 invalid_form_descriptor` (provider bug).
- The confirmed values land in `issue_interactions.input`; the effective execution snapshot in
  `issue_runs.input` as `workflowSnapshotId`, which is server-owned — the caller cannot choose it, and
  every entry that starts a workflow run (this one, a direct `WorkflowRun`, a manual workflow IssueRun)
  resolves it through the same admission gate. A workflow run's message is a `system` **activity**,
  never a `workflow` comment.

## Projects / workspaces / operations

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/projects` | — | `{items, nextCursor}` (tenant-level, **owner-filtered** — see access note) |
| POST 🔑 | `/projects` | `name`*, `repositoryUrl`*, `defaultBranch`* (a branch name valid in a ref, else `400 invalid_ref`; it is the Binding branch), `credentialRefId` | 202 `{resource, workspace, operation}` |
| GET | `/projects/{pid}` | — | Project |
| PATCH | `/projects/{pid}` | `name`, 🔢`version` | Project |
| DELETE 🔑 | `/projects/{pid}` | 🔢`version` | 202 `{resource, operation}` |
| GET | `/projects/{pid}/workspaces` | — | `{items, nextCursor}` |
| POST 🔑 | `/projects/{pid}/workspaces` | `title`*, `baseRef` (omitted, empty, `HEAD` or the Project's `defaultBranch`; anything else `400 base_ref_not_synced`) | 202 `{resource, operation}` |
| GET | `/workspaces/{wid}` | — | Workspace |
| POST 🔑 | `/workspaces/{wid}/start` | 🔢`version` | 202 `{resource, operation}` |
| POST 🔑 | `/workspaces/{wid}/stop` | 🔢`version` | 202 `{resource, operation}` |
| DELETE 🔑 | `/workspaces/{wid}` | 🔢`version` | 202 `{resource, operation}` |
| GET | `/operations/{oid}` | — | Operation |
| POST 🔑 | `/operations/{oid}/retry` | 🔢`version` | `{operation}` |
| GET | `/resource-status` | — | `{items}` (admin projection) |
| POST 🔑 | `/workspaces/{wid}/administrative-stop` | 🔢`version` | 202 `{resource, operation}` (admin) |
| GET | `/spaces/{sid}/plugins/catalog` | — | `{items, syncedAt}` (catalog snapshot, never the network) |
| GET | `/spaces/{sid}/plugins` | — | `{items: SpacePlugin[]}` (desired/observed states) |
| POST 🔑 | `/spaces/{sid}/plugins` | `identifier`* (canonical `ns/name`), `pluginVersion` (defaults to catalog version; pinned) | `{resource: SpacePlugin}` |
| DELETE 🔑 | `/spaces/{sid}/plugins` | `identifier`*, 🔢`version` | `{resource: SpacePlugin}` |

### Access model (Project Workspace Sharing, Step 3)

Projects are **workspace-shared**: `CanAccessProject(user, P) = P.space_id = W AND user is an active
member of W`. Concretely:

- **Space-scoped project** (`space_id` set): `GET /projects/{pid}`, `PATCH`, `DELETE`, the runtime
  workspace `GET /workspaces/{wid}`, and `GET /projects/{pid}/workspaces` are reachable by **any active
  workspace member**, regardless of creator. Deleting requires the **project creator OR a workspace
  owner/admin** (unified rule `CanDeleteWorkspaceResource`): an ordinary member who can read but not
  delete gets **403 `space_role_required`**; a non-member never reaches the gate (`project()` hides the
  resource with 404, no existence leak).
- **Legacy unscoped project** (`space_id` NULL): keeps **owner-only** access end to end (detail, runtime
  workspaces, delete); no auto-backfill, no scope widening.
- **Lists**: the space-scoped list `GET /spaces/{sid}/projects` returns every active project in the
  workspace (membership-gated); the **tenant-level `GET /projects` stays owner-filtered** (the space view
  is the sharing surface; the frontend does not use the tenant-level list).
- **Runtime Workspaces inherit the parent Project's access**: `GET /workspaces/{wid}` resolves the parent
  project's `space_id` and applies the workspace-membership rule; unscoped parents stay owner-only. No
  per-runtime member table.
- **No per-project membership**: `project_members` is **NOT used**; `owner_user_id` continues to record
  the creator.
- **Frontend delete button (Step 3A)**: the detail page's delete button gate now mirrors the backend rule
  (`creator OR workspace owner/admin`), so a member who created a project sees their own delete button;
  the backend delete authorization is unchanged.

## Internal / control API (`/internal/v1`)

Service-only (no user token except where noted). `epoch` = controller fencing. See
[../../core-contract.md](../../core-contract.md) + [../../execution-contract.md](../../execution-contract.md).

| Action | Path | Notes |
| --- | --- | --- |
| access | `/internal/v1/access` | +user token; `tenantId, workspaceId, action, epoch` |
| admit | `/internal/v1/admissions` | +user token; `tenantId, workspaceId, action, ticketId, kind, epoch` |
| lease_acquire / renew / release | `/internal/v1/controller-lease/*` | controller leadership |
| claim | `/internal/v1/operations/claim` | poll next operation |
| snapshot | `/internal/v1/operations/{oid}/snapshot` | restricted aggregate |
| plan | `/internal/v1/operations/{oid}/effects` | persist external-effect plan |
| effect_result | `/internal/v1/operations/{oid}/effects/{eid}/result` | report substrate outcome |
| advance / defer | `/internal/v1/operations/{oid}/advance` `/defer` | step machine |
| node_register / status / idle | `/internal/v1/nodes/*` | node lifecycle |
| node_finish | `/internal/v1/nodes/tickets/{ticket}/finish` | execution ticket |

## Field validation (`validField`)

| Field(s) | Accepted JSON type |
| --- | --- |
| default (incl. `title`, `status`, `name`, `body`, `key`, …) | string |
| `version`, `epoch`, `position`, `retrySeconds`, … | integer (json.Number, Int64) |
| `ids` | array of strings |
| `filter`, `properties`, `input` | object |
| `idle`, `initialized` | bool |

Unknown body key → 400 `unknown_field`; wrong type → 400 `invalid_field_type`; `null` body on
non-GET → 400 `invalid_json` (send `{}` for an empty body, e.g. label detach).
