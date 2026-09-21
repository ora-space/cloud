# frontend: Ora Cloud Web Frontend

[中文](README.md) | [English](README.en.md)

React 19 + TypeScript + Vite 8 + Tailwind CSS 4 (shadcn/ui components). The API layer is not hand-written: `src/api/` is compiled by [orval](https://orval.dev) from the backend-generated [`api/openapi.json`](../api/openapi.json) into typed [TanStack Query](https://tanstack.com/query) hooks.

## Layout

| Path | Purpose |
| --- | --- |
| `src/api/<tag>/<tag>.ts` | **Generated.** One directory per OpenAPI tag (`me`, `projects`, `workspaces`, `internal`, …), with a `useXxx` / `getXxxQueryKey` / `getXxxQueryOptions` set per operation; never edit by hand |
| `src/api/generated.schemas.ts` | **Generated.** All request, response, and parameter TypeScript types; never edit by hand |
| `src/api/index.ts` | **Generated.** Re-exports every tag directory |
| `src/lib/api-client.ts` | The axios instance (`AXIOS_INSTANCE`) and mutator shared by every generated hook; the browser carries only its HttpOnly session cookie and stores or injects no token |
| `orval.config.ts` | Generator config: input `../api/openapi.json`, `client: 'react-query'`, `clean: true` |
| `vite.config.ts` | `@` → `src` alias; dev proxy for `/auth`, `/api`, and `/healthz` to Gateway `http://localhost:8081`; vitest and coverage thresholds |
| `scripts/` | Gate scripts that enforce module READMEs, tests and documented exports; see [`scripts/README.en.md`](scripts/README.en.md) |
| `AGENTS.md` | Engineering rules for this directory (cohesion, size limits, docs, tests); `CLAUDE.md` imports it |

Every directory with hand-written source is a module and carries its own `README.md` / `README.en.md`; start from [`src/README.en.md`](src/README.en.md).

## Commands

```sh
npm ci                  # install (same as CI); Node >= 24, see .node-version
npm run dev             # Vite dev server, http://localhost:5173 by default
npm run api:generate    # regenerate src/api from ../api/openapi.json
npm run format          # prettier --write (ts/tsx/css/html; Markdown and JSON are left alone)
npm run format:check    # prettier --check
npm run lint            # oxlint, type-aware, warnings are errors
npm run typecheck       # tsc -b (strict, noUncheckedIndexedAccess, exactOptionalPropertyTypes, …)
npm run test            # vitest with coverage thresholds; npm run test:watch for the watcher
npm run check:modules   # every module has README.md + README.en.md and tests
npm run check:modules -- --base origin/main   # changed modules also changed docs and tests
npm run check:docs      # every export has an informative JSDoc
npm run check:dead      # knip: unused files, exports, dependencies
npm run check:dup       # jscpd: duplicated code
npm run build           # tsc -b && vite build
npm run check           # everything above except the --base diff, in CI order
```

Task wrappers at the repository root:

- `task frontend:install`: `npm ci`.
- `task frontend:dev`: Vite only (start Cloud with `task run` and Gateway with `task run:gateway`).
- `task dev`: Cloud (:8080), authentication Gateway (:8081), and Vite (:5173) together; the single entry point for day-to-day development.
- `task frontend:generate`: runs `task openapi` (Go contract → `api/openapi.json`), then `npm run api:generate`. Run this after any backend API change and commit `api/openapi.json` together with `frontend/src/api`.
- `task frontend:format` / `task frontend:test`: `npm run format` / `npm run test`.
- `task frontend:check`: the same gate as the CI `frontend` job: regenerate and detect drift in `frontend/src/api`, then `npm run check`.
- `task frontend:check:diff` (optionally `BASE=<ref>`, default `origin/main`): the pull-request check that changed modules also updated their READMEs and tests.

## Invariants

- **One source for the contract**: `internal/contract` → `api/openapi.json` → `src/api`. Uncommitted drift at any link fails either the Go test (`TestPublishedOpenAPIIsValidAndCurrent`) or the CI `frontend` job.
- **No hand-written files inside the generated directory**: `clean: true` wipes `src/api/` before every run. Shared code belongs in `src/lib/`.
- **Generation is deterministic**: the same `openapi.json` yields byte-identical output on every run, which is what makes the drift check meaningful.
- **Docs and tests travel with code**: each module directory has bilingual READMEs and tests, every export has JSDoc, and pull requests that change a module's implementation must change its READMEs and tests too (or carry a reasoned `Docs-Unchanged:` / `Tests-Unchanged:` commit trailer). The full rule set is in [`AGENTS.md`](AGENTS.md).

## Local end-to-end

The backend needs a real PostgreSQL database and registered Gateway keys. Follow the root [README](../README.en.md) to start and migrate the database and configure `configs/gateway.yaml`. After `task dev`, open only `http://localhost:5173`; that origin proxies login and API requests, so `public.base_url` must name the same browser-visible origin. Production must likewise expose the frontend and Gateway as one public origin.
