# spaces: cloud collaboration space adapter

## Responsibility

Wraps the generated orval client into domain hooks and owns the "current space" context and the SSE subscription. It:

- resolves the tenant via `GET /api/v1/me/tenants` and loads the `GET /spaces` list after the layout verifies the Gateway session;
- resolves the route's `:workspaceSlug` against real spaces (an unjoined slug resolves to nothing);
- queries and mutates space members and space-scoped projects (create / rename / archive / member upsert), invalidating the affected queries on success;
- subscribes to the `/spaces/:sid/events` SSE stream and only invalidates queries on events (events never carry business state).

It does not render pages, define routes, sign credentials (the Gateway owns that), or serve mock data.

## Files

| File | Purpose |
|---|---|
| `api.ts` | Domain hooks (useSpaces / useSpaceMembers / useSpaceProjects / useCreateSpace / useUpdateSpace / useArchiveSpace / useUpdateSpaceMember) |
| `current-space.tsx` | `CurrentSpaceProvider` + `useCurrentSpace`: resolves the active authenticated Cloud space |
| `use-space-events.ts` | `parseSSEFrames` (pure) + `useSpaceEvents` (fetch stream subscription and invalidation) |
| `create-space-dialog.tsx` | Create-space dialog: slug normalized to lowercase, reports the slug for navigation |
| `spaces.test.tsx` | Tests for the behaviors above |

## Dependencies and consumers

Depends on `src/api` (generated client) and TanStack Query. Browser credentials stay in the same-origin Gateway's HttpOnly cookie and are unreadable here.

May be consumed by: pages and layout components (`projects`, `members`, `settings`, `dashboard-layout`, `app-sidebar`).

## Invariants

- Every tenant-scoped query is gated on `tenantId` (`enabled: !!tenantId`); no request fires without a tenant;
- the module never reads or assembles authentication tokens; normal requests and SSE rely on the browser's same-origin HttpOnly cookie;
- SSE events only trigger invalidation; authoritative state always comes from REST refetches;
- `parseSSEFrames` is pure: malformed frames are skipped, never thrown.

## Testing

`spaces.test.tsx` mocks the real API with dynamic MSW handlers and verifies tenant/space resolution without JavaScript tokens. Tests make no network calls.
