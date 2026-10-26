# test: test scaffolding

[中文](README.md) | [English](README.en.md)

## Responsibility

Environment setup and test doubles shared by every test. The goal is that each test runs deterministically in jsdom without a network or a real backend, and that no state leaks between cases.

## Contents

| File | Description |
| --- | --- |
| `setup.ts` | vitest `setupFiles`: unmounts Testing Library trees after each case. |
| `http.ts` | `installFakeHttp(body, status)`: swaps the adapter of `AXIOS_INSTANCE`, records requests and answers with a fixed response; restored automatically when the test finishes. |
| `http.test.ts` | Verifies the fake adapter's own recording and error-status semantics, which other tests rely on. |
| `issue-fixtures.ts` | `makeIssue` / `makeStatus` / `makeProject` / `makeAgentTarget`: real-contract issue, status, project and Task Mode directory fixtures. |
| `issue-fixtures.test.ts` | Verifies the fixtures' defaults and override precedence, which other tests rely on. |
| `msw-server.ts` | MSW node server: the mock-domain handlers plus a baseline `GET /api/v1/me → 401` (every render starts signed out) and `GET /auth/providers → ['github']` (a production-shaped gateway; tests of the developer login override it). |
| `cloud-handlers.ts` | Shared MSW doubles for the session probe and the `cloud-dev` tenant space returned by `/me/spaces`, `projectFixture` / `installSpaceProjectHandlers` for that space's Projects, fixtures such as `TEST_USER`, and `chooseProject` for selecting the fixture Project in a run's start dialog. |
| `navigation.ts` | `installFakeNavigation()`: swaps external navigation and new-tab opening, recording `destinations` and `openedTabs`; restored when the test finishes. |
| `polling.ts` | `settleUntil(check)`: retries an assertion while `setInterval` is faked (a polling test advances its query that way) — Testing Library's `waitFor` polls on `setInterval` itself and cannot run then. |
| `polling.test.ts` | Verifies that it retries until the assertion holds and rethrows the last failure once the attempts run out, which other polling tests rely on. |
| `render.tsx` | `renderWithProviders` / `renderAtRoute` / `renderRoutes`: render entries that wire QueryClient, `SessionProvider`, Sidebar and (for the first two) `CurrentSpaceProvider`; `renderAtRoute` resolves the slug from the initial path with the app's `WORKSPACE_ROUTE_PATTERN` (`/w/:workspaceSlug`). |

## Dependency direction

Depends on `@/lib/api-client` (to swap its adapter), `@/lib/navigation` (to swap external navigation), `@/features/auth/session`, `@testing-library/react` and `vitest`. Production code **never** imports this directory.

## Conventions

- Doubles replace boundaries (the HTTP adapter, external navigation, network handlers), never internal modules; add a new file with its own test when another boundary needs a double.
- Tests that need a signed-in member call `installSignedInSession` (or `installCloudSpaceHandlers`) first; not calling it means signed out, the same default a browser without a cookie gets.
- Coverage excludes this directory (see `vite.config.ts`).
