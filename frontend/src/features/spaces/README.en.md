# spaces: collaboration-space adapter

## Responsibility

Wraps the generated Cloud client, loads every space joined across tenants, selects the space by route slug, and exposes that space's `tenantId`. A collaboration space is the visible information for one tenant; creating one calls `POST /api/v1/tenants`. Also owns space renaming, space project lists, and SSE invalidation.

It does not own membership (`features/members`), joining (`features/onboarding`), authentication, or project business content.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Paginated complete joined-space list, tenant creation, rename, and project hooks |
| `current-space.tsx` | Derives the active space and its tenant ID from the route slug |
| `slug.ts` | Backend-compatible slug validation and name derivation |
| `create-space-dialog.tsx` | Dialog creating a new tenant and its sole space |
| `use-space-events.ts` | SSE parsing, reconnection, and Query cache invalidation (including issue-run Thread events) |
| `*.test.tsx` | Tests for listing, creation, switching, and events |

## Dependencies and invariants

Depends on the generated client, `features/auth/session`, and TanStack Query. Layout, onboarding, projects, and settings consume this module.

- Request `/me/spaces` only after confirmed sign-in; read all pages so the switcher includes every tenant.
- Derive `tenantId` from the route-matched space; an unknown slug never borrows the first tenant's privileges.
- Roles are `admin` and `member`; treat an unknown role as an ordinary member.
- SSE events only trigger authoritative REST refetches; disconnections reconnect with capped backoff. A 403/404 response refreshes joined spaces and stops the subscription so the UI leaves a space the user can no longer access.

## Testing

MSW replaces the generated client's network boundary; pure tests cover slugs, SSE frames, and reconnect delays.

Creation uses shared `CreateFormSubmit` error and pending feedback. SSE handles tenant membership revocation, plugin selection and catalog updates by refreshing the corresponding queries. `issue_run.thread_appended` / `issue_run.thread_changed` invalidate that run's Thread query (the generated client's `getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey`, without params) and `['issue-runs', tid, issueId]`; `lastSeq` is only a hint, and only the Thread GET moves a cursor. A Thread event without `issueId` is ignored, and an event whose `issueId`/`runId` is not a string is dropped at the boundary.
