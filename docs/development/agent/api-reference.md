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
| `ConversationTarget` | **Not implemented** — out of scope this wave, left open in §37.17. |

**Implemented by Wave 3B-1** (migration `0009` — formerly `0008`, renumbered in the workspace integration; semantics frozen in
[12-collaboration-architecture.md §37](../../migrations/multica-issue-board/12-collaboration-architecture.md#37-wave-3b-0--collaboration-interaction-model-frozen)):

- `Comment.authorType` is no longer hard-coded `user`: mock agent/team runs post a reply comment with
  `author_type='agent'`/`'team'` through an **internal** path (no public impersonation — clients cannot
  set `authorType`).
- `issue_activities` is now readable: `GET /issues/{iid}/timeline` returns Comment + Activity merged in
  one shared per-issue `seq` order.
- `Comment.targets[]` is implemented on comment create (see the Interaction section below).
- `GET /collaboration/targets` exposes the `@`-picker projection (*not* an Agent/Team/Workflow API).

The 14 integration ports are inventoried in that doc's §6.4.

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
| GET | `/collaboration/forms/{formRef}` | — | `FormDescriptor` (`{formRef,title?,description?,fields[]}`) |
| POST 🔑 | `/issues/{iid}/collaboration/assist` | `targetId`*, `values`(object) | `{suggestedValues, suggestedContextRefs, explanations?}` — **stateless, suggest only, no side effects** |
| POST 🔑 | `/issues/{iid}/interactions/{ixid}/confirm` | `values`(object)*, `contextRefs`(array of `{refType,refId}`) | `{resource: IssueRun}` (the initial run, `status=queued`; dispatch is post-commit) |

- `formRef` is an **opaque** provider token (not a UUID) and appears in a path segment.
- `values` is a plain `{fieldKey: value}` map, re-validated server-side against the **current**
  descriptor: unknown key / wrong type / value outside `options` → 400 `invalid_field_value`; missing
  required → 400 `required_field_missing`.
- Confirm claims the interaction with a compare-and-set on `run_id IS NULL`: a second confirm (different
  key) → 409 `interaction_already_confirmed`; the same key + same body replays the stored response.
- Confirm on a non-`form` interaction → 409 `interaction_not_confirmable`.
- **Assist is stateless and takes a `targetId`, not an interaction**: the form is a *draft* until the
  user confirms it, so assist must work before any interaction exists and must write nothing. An
  unknown/non-workflow target → 404 `target_not_found`.
- Capability states: `503 form_descriptor_unavailable` / `503 assist_unavailable` (port not wired);
  `404 form_descriptor_not_found` (unknown `formRef`); `500 invalid_form_descriptor` (provider bug).
- The confirmed values land in `issue_interactions.input`; the effective execution snapshot in
  `issue_runs.input`; a workflow run's message is a `system` **activity**, never a `workflow` comment.

## Projects / workspaces / operations

| Method | Path | Body fields | Response |
| --- | --- | --- | --- |
| GET | `/projects` | — | `{items, nextCursor}` (tenant-level, **owner-filtered** — see access note) |
| POST 🔑 | `/projects` | `name`*, `repositoryUrl`*, `defaultBranch`, `credentialRefId` | 202 `{resource, workspace, operation}` |
| GET | `/projects/{pid}` | — | Project |
| PATCH | `/projects/{pid}` | `name`, 🔢`version` | Project |
| DELETE 🔑 | `/projects/{pid}` | 🔢`version` | 202 `{resource, operation}` |
| GET | `/projects/{pid}/workspaces` | — | `{items, nextCursor}` |
| POST 🔑 | `/projects/{pid}/workspaces` | `title`*, `baseRef`* | 202 `{resource, operation}` |
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
