# auth: session and login boundary

[中文](README.md) | [English](README.en.md)

## Responsibility

The only module in the frontend that knows who the user is and whether they are signed in. It:

- completes login and logout through the gateway (`GET /auth/providers` to learn which logins exist, `POST /auth/login` → redirect to the provider; `POST /auth/logout`), the one hand-written HTTP surface outside OpenAPI;
- probes the session with `GET /api/v1/me` and exposes it as a `Session` (`loading` / `signed-out` / `disabled` / `unavailable` / `signed-in`; 403 is `disabled`, a valid gateway session Cloud refuses, which logging in again cannot fix);
- owns the 401 policy: any request answered with 401 ends the session, so screens redirect to login instead of failing query by query;
- provides the route gate `RequireSession` and the `LoginPage`.

It does not resolve tenants or spaces (`features/spaces`) and never holds a token: the session is the gateway's HttpOnly cookie, which this module cannot read either.

## Files

| File | Description |
| --- | --- |
| `api.ts` | `fetchLoginProviders` (the gateway's provider list, filtered to `huawei-idaas` / `github` / `dev`), `isExternalProvider`, `startLogin` (fetches `authorizationUrl`, then `navigateExternal`), `logoutSession`, `GITHUB_SIGN_OUT_URL` (GitHub's own sign-out page), `fetchSessionUser` (401 → signed out, 403 → disabled, anything else throws) |
| `providers.ts` | `useLoginProviders`: the provider list as a query cached for the tab; shared by the login page and the sidebar |
| `session.tsx` | `SessionProvider` (session query + `onUnauthorized` subscription), `useSession` with `signOut` and `signOutOfGitHub` (sign out, then GitHub's sign-out page in a new tab so this tab stays on Ora), the `Session` type, `SESSION_QUERY_KEY` |
| `require-session.tsx` | `RequireSession`: renders nothing while loading; redirects a signed-out tab to login, storing a private join link in the current tab so Gateway receives only a tokenless return path; reports a disabled account and an unreachable backend in place |
| `login-page.tsx` | With exactly one provider and it external (the production shape, e.g. `huawei-idaas`), starts that login by itself once and shows only a retry after a failure; otherwise one button per provider the gateway offers, plus a "sign out of GitHub first" link when GitHub login exists (GitHub otherwise reuses the browser's current account); a disabled account is told so and never sent to log in again: "sign in with GitHub" and, on a local gateway with `login.development_provider`, "developer login" (the gateway's own form where any typed identity signs in); `?returnTo=` is narrowed by `safeReturnTo`; a signed-in tab is redirected straight away |
| `auth.test.tsx` | Tests for all of the above |

## Dependency direction

Depends on: `src/api` (`getApiV1Me`), `src/lib/api-client` (`customInstance`, `onUnauthorized`, `isUnauthorizedError`), `src/lib/navigation`, `src/lib/paths`, TanStack Query, react-router.

May be consumed by: `main.tsx` (mounts `SessionProvider`), `routes.tsx`, layout components, `features/spaces` (gates its queries on `useSession`) and any page that shows the current user.

## Invariants

- `SessionProvider` is mounted exactly once, outside the router and inside the QueryClient.
- `fetchSessionUser` treats only 401 as "signed out" and only 403 as "disabled"; network errors and 5xx are `unavailable`, so `RequireSession` never mistakes them for a sign-out and never loses `returnTo` over them.
- Invitation and application tokens never enter Gateway's `returnTo`; the provider round trip stores only `/join/continue` while the full link stays in this browser tab.
- The automatic login start happens at most once per mount: a failed start shows an explicit retry, never a redirect loop.
- `signOut` calls the gateway first, then clears the cache: the session becomes null and every other query is removed, so the next member never sees the previous one's data.
- A 401 in a signed-in session also removes the prior member's queries, including private model connections. The initial anonymous probe preserves public login-provider queries.
- Ora cannot end a github.com session: the gateway never holds a GitHub token (it discards it right after reading the profile), so "sign out of GitHub" can only open GitHub's own sign-out page, always after revoking the Ora session, and in a new tab so the member returns to the login screen in this one. The URL is public github.com; a GitHub Enterprise Server deployment would need it made configurable.
- The login page never builds a provider URL or parses a callback; that is the gateway's job. It also never decides on its own whether the developer login exists: the button appears only when `/auth/providers` lists `dev`, which the gateway allows solely on loopback development origins.

## Testing

`auth.test.tsx` covers `/auth/*` and `/api/v1/me` with MSW and observes redirects through `installFakeNavigation`. The baseline MSW server answers `/api/v1/me` with 401, so signed-out scenarios need no extra handler.
