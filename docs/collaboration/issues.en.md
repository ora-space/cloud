# The Issue Board

English | [中文](issues.md)

Issues are task cards within a tenant, and the Issue board is Cloud's first purely business resource:
it depends on no runtime and only reads and writes PostgreSQL. Starting collaboration on an Issue by
`@`-mentioning people, Agents or workflows is covered in [Issue collaboration](interactions.en.md),
and Agent runs themselves in [Agent IssueRuns](../agent-runs/issue-runs.en.md).

The board is shared with every current member of the tenant: any member can read and write the
tenant's Issues, status columns, labels and comments; only saved views are personal.

## Resources

| Resource | Table | Description |
|---|---|---|
| Issue | `issues` | Title, description, status, priority, assignee, parent Issue, related Project, board position, per-tenant number, custom properties |
| Status column | `issue_statuses` | Board columns; tenants can define their own |
| Label | `labels`, `issue_labels` | Tenant-level labels, many-to-many with Issues |
| Subscriber | `issue_subscribers` | Members following an Issue |
| Comment | `issue_comments` | Supports replies (`parentId`); the author is an ActorRef |
| Activity | `issue_activities` | An append-only timeline projection with no public update or delete |
| Context reference | `issue_context_refs` | Opaque references to external resources; recorded, never copied |
| View | `issue_views` | Personal saved filters, visible only to their creator |

## Issues

- **Number**: a per-tenant `number` starting at 1, allocated in a transaction under the global lock.
- **Priority**: `urgent`, `high`, `medium`, `low`, `none`; defaults to `none`.
- **Status**: must be a live column in the tenant's status catalog; defaults to `todo`.
- **Assignee** is a polymorphic ActorRef: `assigneeType` of `user`, `agent` or `team` plus
  `assigneeId`. A user assignee must be an existing active user; Agent and team assignees are only
  checked for UUID shape and not resolved. For older clients a lone `assigneeUserId` is still
  accepted. Assigning only changes who is responsible and **triggers no execution**.
- **Parent Issue**: an existing Issue in the same tenant, never the Issue itself. Deleting an Issue
  leaves its children without a parent.
- **Related Project** (`projectRef`): only checked for UUID shape, not for existence; there is no
  foreign key, so a deleted Project leaves a reference that no longer resolves rather than an error.
  Running an @Agent on an Issue requires it to name an existing Project in this Space (see
  [Issue collaboration](interactions.en.md)).
- **Description** is at most 20,000 characters and the title at most 200; `properties` is an arbitrary
  JSON object.

The list `GET /tenants/:tid/issues` returns the whole board at once (no paging), ordered by status
column position, card position and ID. `?q=` performs a case-insensitive substring match over title
and description, treating `%` and `_` in user input literally.
`GET /tenants/:tid/issue-groups?groupBy=` returns the same content grouped by status, priority or
assignee.

## Board position and moving

Each card has a floating-point `position`, and cards in a column are ordered by it:

- New cards and cards that change status go to the top of the target column (the current minimum
  position minus one).
- `POST .../issues/:iid/move` can change status and position together: with both `beforeId` and
  `afterId` the card takes their midpoint, with only one it goes right next to it, and with neither but a
  changed status it goes to the top of the new column. When the anchors leave no room the answer is
  `409 position_conflict`.
- `POST .../issues/batch` atomically applies one status, priority or assignee change to up to 100
  Issues.

## Status columns

The first time a tenant uses the board, the seven **system columns** are filled in automatically:

| key | Name | Category |
|---|---|---|
| `backlog` | Backlog | `unstarted` |
| `todo` | Todo | `unstarted` |
| `in_progress` | In Progress | `started` |
| `in_review` | In Review | `started` |
| `blocked` | Blocked | `started` |
| `done` | Done | `done` |
| `cancelled` | Cancelled | `closed` |

Tenants can add custom columns (a `key` of lowercase letters, digits and underscores, at most 32
characters, with a category of `unstarted`, `started`, `done` or `closed`) and change names,
descriptions, categories, colors, icons and positions. System columns cannot be deleted
(`409 system_status_required`); deleting a custom column is a soft delete.

## Comments, activities and the timeline

- A **comment**'s author is an ActorRef: comments posted by users are `user`; replies from Agent or team
  runs are written as `agent` or `team`; `system` is allowed by the schema but nothing writes it today.
  A reply's parent must belong to the same Issue and not be deleted. Bodies are at most 20,000
  characters and can be edited and soft-deleted. A comment may carry `targets`, the collaboration
  targets selected with `@` (see [Issue collaboration](interactions.en.md)).
- **Activities** are appended by the server when business state changes — a run being queued, started,
  completed, failed or cancelled, for example; a workflow's output also appears as an activity by
  `system`. Activities have no public update or delete, and business state is never rebuilt from them.
- The **timeline** `GET .../issues/:iid/timeline` merges comments and activities into one list ordered
  by **one shared per-Issue sequence `seq`**, not by creation time. Comments and activities share this
  sequence space, so numbers never repeat within an Issue. The timeline is a high-level projection —
  neither an execution log nor an event source; the full record of an Agent session is in its
  [Thread](../agent-runs/threads.en.md).

## Context references

`.../issues/:iid/context-refs` records the external resources this Issue relates to: `refType` is one of
`parent_issue`, `run`, `timeline_message`, `pull_request`, `project`, `workspace` and
`acceptance_criteria`, and `refId` is a UUID. References store only a pointer and never copy or expand
content; the same reference cannot be added twice (`409 context_ref_exists`), and deletion is a hard
delete. When a run is created, these references go into the context snapshot in the run's input.

## Labels, subscriptions and views

- **Labels** belong to the tenant, with names unique among live labels; adding a label to or removing it
  from an Issue is a hard insert or delete on the join table.
- **Subscriptions**: the request body's `userId` names a current member to add to or remove from the
  Issue's followers; adding again is idempotent.
- **Views** store a name and a `filter` object and belong only to their creator: other members can
  neither see nor change them, and accessing someone else's view returns `404`.

## Related decisions

- [Issue collaboration: `@` only selects typed targets; execution comes only from explicit intent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)
- [The collaboration Space is the sharing, visibility and authorization boundary within a tenant](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/0-collab-space-boundary.md)
