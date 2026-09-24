# features/plugins: workspace plugin screen

[English](README.en.md) | [中文](README.md)

The workspace sidebar's "Plugins" screen and its data layer: the **selected plugins** list
(desired/observed state badges + removal) on top, the **marketplace** catalog grid (search, kind
filter, install) below. It expresses only the cloud-authoritative selection state; activation,
stop, configuration and logs belong to the Node runtime plane and stay out of this module.

## Files

- `api.ts`: cloud-backed hooks — `usePluginCatalog` (5-minute staleTime matching the server sync
  cadence), `useSpacePlugins`, `useInstallPlugin` (pins the catalog version), `useRemovePlugin`
  (optimistic version; refetches after a 409).
- `plugins-page.tsx`: the page and its presentational sub-components (state badges, kind labels,
  install/remove interactions).
- `*.test.tsx`: functional tests for the page and hooks over MSW contract fixtures.

## Dependencies and callers

- Depends on: `src/api` (orval-generated client), `features/spaces` (current-space, idempotency
  keys, SSE invalidation), `components/ui`.
- Called by: `routes.tsx` (`/w/:slug/plugins`), `components/layout/app-sidebar.tsx` (nav entry).
- Tests depend on: `mocks/handlers/plugins.ts`, `test/cloud-handlers.ts`.

## Invariants

- Catalog data comes only from the PostgreSQL snapshot (staleTime aligns with the 5-minute sync
  cadence); the screen never reaches the marketplace network itself.
- State badges render exactly one of the six `observedState` values; rows with
  `desiredState=removed` never enter the selected list.
- `pack` entries are not installable in v1: the button stays disabled and reads "暂不支持".
- Every async update stays inside query/mutation act boundaries (clean-stderr gate).

## Tests

- Page behavior runs over MSW fixtures on the real API paths: a POST install moves the row to
  待安装 and disables the button; removal needs confirmation, carries the optimistic version,
  and a 409 shows the inline notice plus a refetch.
- SSE invalidation branches live in `features/spaces/spaces.test.tsx`; the nav entry lives in the
  app-sidebar tests.
