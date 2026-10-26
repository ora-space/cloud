# issues: tenant issues

[中文](README.md) | [English](README.en.md)

## Responsibility

The tenant's issue board, list and detail pages, plus the Cloud Issue data layer they share (`/api/v1/tenants/:tid/issues*`). Not owned: the Agent session Thread (`thread/`) and MSW demo data.

## Files

| File                                                       | Purpose                                                                                                                                                                                                     |
| ---------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `api.ts`                                                   | Query hooks for issues, statuses, members, comments, runs, timeline, interactions and context refs; `useRuns` refreshes every 5 s while an agent run is unsettled (run settlement publishes no space event) |
| `types.ts`                                                 | Issue domain types mirroring the backend contract; preparation uses generated `RunPreparation` without duplicating its contract                                                                             |
| `present.ts`                                               | Pure display helpers: number, assignee, status columns                                                                                                                                                      |
| `issues-page.tsx` / `issues-board.tsx` / `issues-list.tsx` | The issues page, the board (drag → `move` anchors) and the list                                                                                                                                             |
| `issue-detail-page.tsx`                                    | Wide conversation area, Markdown description and collapsible properties; versioned Project updates with recovery reload, composing conversation/execution/activity navigation                               |
| `*.test.ts(x)`                                             | Page and hook tests                                                                                                                                                                                         |

Subdirectories: `components/` (page sub-components), `thread/` (one Agent session), `conversation/` (task-level continuing conversation and composer).

## Dependencies and invariants

Depends on `src/api`, `lib`, `components`, `features/spaces`; used by `routes.tsx`. The `slug` prop is really the tenant id (forwarded by `CloudScope`); navigation links use the space slug from the route. The run list's query key is fixed as `['issue-runs', tid, issueId]`; space events invalidate it by that key.

The detail page passes member display names to the Thread's initiator summary; Thread permissions remain server-authoritative.

Issue creation supports the existing optional `projectRef` contract. Tests paste a complete title and verify the real API request and refreshed list.

## Testing

MSW stands in for the real Cloud API; under `onUnhandledRequest: 'error'` every test must serve every query the page mounts.

Recovery reloads and property saves are mutually exclusive: properties and the reload button remain locked until the original save error has been handled. Failed reloads retain the error; a later save uses the loaded version and reports its own failure independently.

Detail tests use SessionProvider and CurrentSpaceProvider to cover versioned association/removal, retained values and GET reload after conflicts, delayed GET/PUT exclusion and error visibility, ordinary comments creating no Run, and the Thread of a Run created through Task Mode. Component evidence never substitutes for real-provider multi-turn, terminal-state and resource-release acceptance.

`IssueRun.workflowInvocationRef` retains the server's WorkflowRun reference for activity status and navigation; the API test checks that list reads preserve it. Workflow confirmation exclusion, retry and detail-link interaction tests live in `components/`.

`useWorkflowSnapshotForm` reuses the generated snapshot-form GET with tenant/workflow/snapshot/issue-scoped query keys. The API test varies every scope so versions and Issues cannot share the wrong field defaults.

`useWorkflowPublishedSnapshotIds` caches all published ids by tenant/workflow, following the generated list endpoint's cursors with a page limit of 100 and the caller's abort signal; it stores no graph copies. The activity form uses those ids to identify published-version options. API tests cover the second-page cursor, complete id list and tenant/workflow isolation.

Issue details use a bounded workspace: heading, views, one transcript viewport and an in-flow composer. The details rail becomes a drawer based on available width; version conflicts retain the existing update path, and denied reads hide stale content.

Initial reads show a skeleton only while pending. `not_found` displays an unavailable-task message with a list link; other failures offer a safe message and retry, with no duplicate retry while loading. A 401/403 still hides cached content, as does a 404 for a removed task. Transient reload failures for an already loaded task retain the existing property-save recovery flow, loaded version and original save error.

Opening a workflow interaction paginates to the exact commentId returned by comment creation, never the latest record for the target. The form supplies a stable comment retry key. Confirm treats interaction_already_confirmed as existing execution and refreshes interactions, runs and timeline without recreating it. API tests cover concurrent same-target records across pages.
