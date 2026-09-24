# lib: React-free infrastructure

[中文](README.md) | [English](README.en.md)

## Responsibility

Utilities shared by generated code and components that contain neither React nor domain concepts. Every file here must be testable in plain Node.

## Contents

| File | Description |
| --- | --- |
| `api-client.ts` | Shared axios instance `AXIOS_INSTANCE` and the orval mutator `customInstance`. The single home for cross-cutting HTTP policy; also reconciles the two cancellation sources, react-query's `AbortSignal` and orval's `cancel()`. The request interceptor supplies `Idempotency-Key` for POST/DELETE; the response interceptor broadcasts every 401 to `onUnauthorized` subscribers (the session owner ends the session on it). Authentication is not a header: the browser holds only the gateway's HttpOnly session cookie, same-origin requests carry it automatically, and the frontend never touches a token; `faultCode` extracts the backend `Fault.code` (e.g. `not_found`, `capability_unavailable`) from a rejected request so feature modules can map errors to UX. |
| `api-client.test.ts` | Verifies body unwrapping, both cancellation paths, the idempotency-key policy, that no credential header is attached, that 401 listeners fire and can be removed, and `faultCode` error-code extraction. |
| `navigation.ts` | The only contact with other origins: `navigateExternal` (the login redirect to the provider) and `openExternalTab` (a provider page in a new `noopener` tab, this tab stays); `replaceExternalNavigation` / `replaceExternalTabOpener` let the test scaffolding swap them, since jsdom does not allow spying on `location.assign` and has no `window.open`. |
| `navigation.test.ts` | Verifies the replace-and-restore semantics. |
| `paths.test.ts` | Verifies `safeReturnTo`'s rejections, `loginPath` encoding and the `/w/` prefix in every workspace route. |
| `paths.ts` | The reserved workspace prefix `WORKSPACE_ROUTE_PREFIX` (`/w`) with its router pattern `WORKSPACE_ROUTE_PATTERN`, the workspace route builder `workspacePaths` (issues, projects, squads, agents, skills, plugins, runtimes, settings and every other workspace route), the login route `loginPath`, narrowing of an untrusted `returnTo` (`safeReturnTo`: single leading slash, no `//`, no backslash or control character, at most 2048 chars) and `workspaceUrlPrefix` (`host/w/`, the fixed part shown in front of the slug input). |
| `mock-api-client.ts` | Axios client for the MSW-mocked domain (`/mock-api/*`), kept separate from the real-backend generated client. It rewrites the real space slug to the seeded demo workspace so pages without a backend keep showing demo data in any space until they gain real API counterparts. The mock domain has no authentication. |
| `utils.ts` | Re-exports `cn` (Tailwind-aware class merging); shadcn components import it via `@/lib/utils`. |

## Dependency direction

Third-party libraries and sibling modules in this directory only. **Never** import `react`, `@/components` or `@/api` (`@/api` depends on this module; the reverse would be a cycle).

## Invariants

- The first parameter of `customInstance` must accept orval's `signal: AbortSignal | undefined` (an explicit `undefined` under `exactOptionalPropertyTypes`). Run `npm run typecheck` after touching the signature to confirm the generated client still compiles.
- The request interceptor must stay synchronous (`synchronous: true`); otherwise axios delays dispatch until the interceptor settles and `AbortSignal` can lose the race.
- No file in this directory may hold, read or generate credentials: the session is the gateway's HttpOnly cookie, which frontend code cannot and must not touch.
- `safeReturnTo` accepts only paths with a single leading `/` whose second character is neither `/` nor `\`, matching the gateway's `returnTo` rule.
