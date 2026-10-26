# features/plugins: workspace plugin screen

[English](README.en.md) | [中文](README.md)

The workspace sidebar's "Plugins" screen and its data layer: the **selected plugins** list
(desired/observed state badges + removal) on top, the **workflow asset** import results (one row
per document, separate from install state) below it, and the **marketplace** catalog grid
(search, kind filter, install) last. Selected Agents delegate a personal entry to `agent-configurations`. It expresses the cloud-authoritative selection state;
activation, stop and logs belong to the Node runtime plane and stay out of this
module.

## Files

- `api.ts`: cloud-backed hooks — `usePluginCatalog` (5-minute staleTime matching the server sync
  cadence), `useSpacePlugins`, `useWorkflowAssetImports` (independent read; polls every 10
  seconds while a row is non-terminal), `useInstallPlugin` (pins the catalog version),
  `useRemovePlugin` (optimistic version; refetches after a 409).
- `plugins-page.tsx`: the page and its presentational sub-components (state badges, kind labels,
  install/remove interactions, per-document asset results and bounded failure-code labels).
- `*.test.tsx`: functional tests for the page and hooks over MSW contract fixtures.

## Dependencies and callers

- Depends on: `src/api` (orval-generated client), `features/spaces` (current-space, idempotency
  keys, SSE invalidation), `components/ui`, `features/agent-configurations`.
- Called by: `routes.tsx` (`/w/:slug/plugins`), `components/layout/app-sidebar.tsx` (nav entry).
- Tests depend on: `mocks/handlers/plugins.ts`, `test/cloud-handlers.ts`.

## Invariants

- Catalog data comes only from the PostgreSQL snapshot (staleTime aligns with the 5-minute sync
  cadence); the screen never reaches the marketplace network itself.
- State badges render exactly one of the six `observedState` values; rows with
  `desiredState=removed` never enter the selected list.
- Workflow asset imports are a separate query and never merge into the install badge: completed
  install plus failed document import is a valid combination. Document failure codes are the
  stable contract; the frontend only maps labels and shows unknown codes verbatim.
- Import rows poll only while `pending`/`importing`; once terminal the interval stops and SSE or
  the next mount refreshes stale data.
- `pack` entries are not installable in v1: the button stays disabled and reads "暂不支持".
- My Configuration uses the selected release's `kind`, not the latest catalog's type; members can open it, and personal data is read only then.
- Every async update stays inside query/mutation act boundaries (clean-stderr gate).

## Tests

- Page behavior runs over MSW fixtures on the real API paths: a POST install moves the row to
  待安装 and disables the button; removal needs confirmation, carries the optimistic version,
  and a 409 shows the inline notice plus a refetch.
- Asset imports are covered through the separate `/plugins/assets` fixture: per-document
  imported/failed results render apart from the install badge, and a fake-timer test advances to
  the terminal state and proves no further request fires.
- SSE invalidation branches live in `features/spaces/spaces.test.tsx`; the nav entry lives in the
  app-sidebar tests.

## Runtime control

Current tenant administrators change the space selection; members read it. Mutations carry the resource version (zero for a new row). Each runtime waits for idle maintenance ownership. Counts distinguish affected, completed, occupied, stopped, unavailable and failed targets. The production plugin executor is unavailable: accepting a selection does not prove installation or start a stopped runtime. Server authorization is independent of button state.
