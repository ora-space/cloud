# src: application source root

[中文](README.md) | [English](README.en.md)

## Responsibility

Browser entry point and composition root. This layer only wires things together: mount React, install global providers, render the root screen. Business logic, HTTP details and styling primitives live elsewhere.

## Contents

| File | Description |
| --- | --- |
| `main.tsx` | Browser entry: starts MSW (for `/mock-api/*` only), creates the `QueryClient`, mounts `SessionProvider` and the router. No exports; not unit-tested. |
| `routes.tsx` | Route table: `/login`, and `/onboarding` plus `/w/:workspaceSlug/*` (under the reserved prefix, so workspace slugs never collide with top-level routes) behind `RequireSession`; `/` lands on `/onboarding`. |
| `index.css` | Tailwind entry and design tokens (colors, radius). Global theme variables only; component styles live with components. |
| `session-routes.test.tsx` | Verifies the session list/detail routes resolve the authenticated tenant and expose the sidebar entry. |

## Submodules

| Directory | Description |
| --- | --- |
| `api/` | **Generated** by orval; never hand-edited, see [`../README.en.md`](../README.en.md). |
| `components/ui/` | Presentational primitives (shadcn/ui) with no business meaning. |
| `features/auth/` | Session and login boundary: gateway login/logout, the `/api/v1/me` probe, the 401 policy, the route gate. |
| `features/onboarding/` | First-workspace creation screen. |
| `features/model-connections/` | Private model connections, credential writes and defaults for new sessions at `settings/model-connections`. |
| `features/spaces/` | Collaboration space adapter: tenant/space resolution, space APIs, SSE subscription. |
| `lib/` | React-free infrastructure: HTTP client, external navigation, paths. |
| `features/sessions/` | Agent IssueRun session screens, authorized reads and pagination. |
| `features/session-data/` | Framework-free normalization of archive and Thread observations. |
| `features/session-timeline/` | Three-lane timeline, tool statistics and raw event inspection. |
| `features/session-metrics/` | Content-free model-request summaries, trends and request pages. |
| `mocks/` | MSW demo data: `/mock-api/*` handlers and seeds for pages without a backend yet. |
| `test/` | Test scaffolding: jsdom cleanup, the fake HTTP adapter, session and space fixtures. |

## Dependency direction

`main.tsx → routes.tsx → (features, components)`; `features → (api, lib)`; `api → lib`. Lower layers never import higher ones.

## Invariants

- `main.tsx` is the only top-level module with side effects (mounting the DOM).
- All real-backend HTTP goes through `AXIOS_INSTANCE` in `lib/api-client.ts`; authentication is the gateway's HttpOnly cookie, and the frontend holds no token.
