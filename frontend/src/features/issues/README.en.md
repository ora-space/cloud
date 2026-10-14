# issues: tenant issues

[中文](README.md) | [English](README.en.md)

## Responsibility

The tenant's issue board, list and detail pages, plus the Cloud Issue data layer they share (`/api/v1/tenants/:tid/issues*`). Not owned: the Agent session Thread (`thread/`) and MSW demo data.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Query hooks for issues, statuses, members, comments, runs, timeline, interactions and context refs; `useRuns` refreshes every 5 s while an agent run is unsettled (run settlement publishes no space event) |
| `types.ts` | Issue domain types mirroring the backend contract; preparation uses generated `RunPreparation` without duplicating its contract |
| `present.ts` | Pure display helpers: number, assignee, status columns |
| `issues-page.tsx` / `issues-board.tsx` / `issues-list.tsx` | The issues page, the board (drag → `move` anchors) and the list |
| `issue-detail-page.tsx` | Activity, versioned Project association with failure reload, real Agent entry and the existing Thread; supplies member names |
| `*.test.ts(x)` | Page and hook tests |

Subdirectories: `components/` (page sub-components), `thread/` (the Agent session panel).

## Dependencies and invariants

Depends on `src/api`, `lib`, `components`, `features/spaces`; used by `routes.tsx`. The `slug` prop is really the tenant id (forwarded by `CloudScope`); navigation links use the space slug from the route. The run list's query key is fixed as `['issue-runs', tid, issueId]`; space events invalidate it by that key.

The detail page passes member display names to the Thread's initiator summary; Thread permissions remain server-authoritative.

Issue creation supports the existing optional `projectRef` contract. Tests paste a complete title and verify the real API request and refreshed list.

## Testing

MSW stands in for the real Cloud API; under `onUnhandledRequest: 'error'` every test must serve every query the page mounts.

Recovery reloads and property saves are mutually exclusive: properties and the reload button remain locked until the original save error has been handled. Failed reloads retain the error; a later save uses the loaded version and reports its own failure independently.

Detail tests use SessionProvider and CurrentSpaceProvider to cover versioned association/removal, retained values and GET reload after conflicts, delayed GET/PUT exclusion and error visibility, ordinary comments creating no Run, and the Thread of a Run created through Task Mode. Component evidence never substitutes for real-provider multi-turn, terminal-state and resource-release acceptance.
