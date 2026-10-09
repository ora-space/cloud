# issues: tenant issues

[中文](README.md) | [English](README.en.md)

## Responsibility

The tenant's issue board, list and detail pages, plus the Cloud Issue data layer they share (`/api/v1/tenants/:tid/issues*`). Not owned: the Agent session Thread (`thread/`) and MSW demo data.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Query hooks for issues, statuses, members, comments, runs, timeline, interactions and context refs; `useRuns` refreshes every 5 s while an agent run is unsettled (run settlement publishes no space event) |
| `types.ts` | Issue domain types mirroring the backend contract |
| `present.ts` | Pure display helpers: number, assignee, status columns |
| `issues-page.tsx` / `issues-board.tsx` / `issues-list.tsx` | The issues page, the board (drag → `move` anchors) and the list |
| `issue-detail-page.tsx` | Detail page: description and activity column, properties column (`components/issue-properties-panel.tsx`) and, when the issue has an agent run, the "Agent 会话" column (`thread/`) |
| `*.test.ts(x)` | Page and hook tests |

Subdirectories: `components/` (page sub-components), `thread/` (the Agent session panel).

## Dependencies and invariants

Depends on `src/api`, `lib`, `components`, `features/spaces`; used by `routes.tsx`. The `slug` prop is really the tenant id (forwarded by `CloudScope`); navigation links use the space slug from the route. The run list's query key is fixed as `['issue-runs', tid, issueId]`; space events invalidate it by that key.

## Testing

MSW stands in for the real Cloud API; under `onUnhandledRequest: 'error'` every test must serve every query the page mounts.
