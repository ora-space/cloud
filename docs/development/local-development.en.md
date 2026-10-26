# Local Development

English | [中文](local-development.md)

This document lists the tools, database and steps needed to run Cloud, the Gateway and the frontend on one
development machine. The complete multi-repository environment (Controller, Sandbox Server, real Nodes and
sandboxes) is provided by the Compose setup in [ora-space/cluster](https://github.com/ora-space/cluster).

## Tools

| Tool | Purpose |
|---|---|
| Go (the version in `go.mod`) | Services and tests |
| Git | Real clones in tests and the simulator |
| PostgreSQL 17 | The only supported database; tests must use real PostgreSQL too |
| [Task](https://taskfile.dev) | The repository's task runner; `task --list` lists every task |
| `buf` ≥ 1.73 | Proto lint, breaking and generated-code checks (needed by `task check`) |
| Node 24, npm ≥ 11.19 | The frontend; only `task frontend:*` needs it |

## Database

Pick one:

- **Docker**: `docker compose up -d --wait` starts the repository's PostgreSQL 17 on `127.0.0.1:55432` with user `ora`,
  password `ora-local` and database `ora`, which are also the defaults in `config.toml.template`.
- **Windows**: `./scripts/postgres.ps1 start` installs and starts a pinned PostgreSQL 17 under the project's `.local/`,
  listening only on `127.0.0.1:55432` with local trust authentication and no system service;
  `./scripts/postgres.ps1 stop` stops it and keeps the data.
- **Your own server**: the role must own the database (migrations create tables and migration tests create schemas).
  Ordinary integration and core fixtures also require `CREATEDB` for their independent template copies.

## Configure and start

```sh
cp config.toml.template config.toml   # config.toml is ignored by Git and may hold a secret; never commit it
task setup                            # generates keys and secret files, writes .local/dev.env, runs migrations; rerunnable
task dev                              # Cloud :8080 + Gateway :8081 + Vite :5173
```

- `config.toml` holds only the values that differ per machine: the database DSN and an optional `[github]` or `[idaas]`
  section (at most one). With both empty the Gateway offers only the local development login, where any typed identity
  signs in. To test real GitHub login, create an OAuth App whose callback is
  `http://localhost:5173/auth/callback/github`.
- `task setup` ([`cmd/devsetup`](../../cmd/devsetup/README.en.md)) generates two purpose-separated Ed25519 key pairs and a PKCE
  key under `.local/gateway/` (keeping existing ones), writes the provider's client secret file and `.local/dev.env`, and runs
  the migrations. `.local/dev.env` overrides `configs/*.yaml` through `CLOUD_*` and `GATEWAY_*` environment variables and also
  provides `TEST_DATABASE_URL`; `Taskfile.yml` loads it for every task, and variables already set in the shell win.
  `configs/*.yaml` remain the authoritative configuration.
- The browser only ever visits `http://localhost:5173`: Vite proxies `/auth`, `/api` and `/healthz` to the Gateway, so
  `public.base_url` is that address and cookies, the same-origin check and OAuth callbacks are all judged against it. A user
  signing in for the first time has no tenant, and the frontend guides them to create their first space.

**Control listener certificates.** `task setup` generates a local CA (`ca.pem`/`ca.key`), the Cloud server certificate and
key (`server.pem`/`server.key`), and the Controller client certificate and key (`controller.pem`/`controller.key`) under
`.local/control/`. The server certificate covers `localhost`, `cloud`, `127.0.0.1` and `::1`. The client URI is
`spiffe://ora.local/controller/local-controller`, using the same identity shape as cluster with an independent CA.
`.local/dev.env` sets `CLOUD_CONTROL_CERTIFICATE_FILE`, `CLOUD_CONTROL_PRIVATE_KEY_FILE`, `CLOUD_CONTROL_CLIENT_CA_FILE` and
`CLOUD_CONTROL_CONTROLLER_IDENTITY` automatically. Existing complete bundles are kept; a partial bundle fails and must
be restored or removed as a whole before regeneration. Before reuse, setup validates key matching, CA chains, purposes,
SANs, Controller identity and validity. Invalid or expired bundles fail without rewriting files. The CA lasts ten years
and leaf certificates one year; repair or regeneration and updating the peer's materials are explicit.

For a local desktop Controller connecting to `https://localhost:8082` (`control.grpc_addr` in `configs/config.yaml`), use
`controller.pem`, `controller.key` and `ca.pem` with Controller ID `local-controller`. Keep the CA private key `ca.key` on
the development machine; do not give it to the Controller. Cloud still requires TLS 1.3 mutual authentication and refuses
startup without configuration. Generated local materials are for development only; cluster Compose continues to generate
and mount its materials through `management-setup`.

To start parts separately: `task run` (Cloud), `task run:gateway` (Gateway), `task frontend:dev` (Vite), `task migrate`
(migrations only).

## Demos without a Controller

- **The simulator**: `task simulate` (`go run ./cmd/simulator`) starts loopback doubles of Cloud and the Substrate in process,
  creates a test tenant, a simulated sandbox and Node, prepares a snapshot in the Git Server double (on-disk repositories laid
  out like the Git Server volume, recording the demo Project's snapshot in place of the external line), really clones that
  snapshot into the main Workspace's own data directory, and finally prints a ready Workspace. The Node double fetches only
  from the Git Server double and never reaches an external repository. Disk data lives in `.local/demo/`, database records are kept, and every
  run creates a new demo tenant. It uses temporary signing keys generated in process, holds no production credentials,
  starts no real Agent or plugin executor, and proves nothing about runtime exclusivity, management authentication or workload
  isolation.
- **Board demo**: `cmd/demo-issue-board-web` (`scripts/demo-issue-board-web.sh`) is a single-file HTML Issue board demo whose
  server signs the JWTs, meant only for manually checking the Issue endpoints; `frontend/` is the real frontend. `cmd/ora-web`
  is an early development edge server for the frontend; day-to-day development uses the Gateway.

These entry points are for development only; the development constructors they use, such as `router.NewDevelopment`, must never appear in a
production process.

## The frontend

The real frontend lives in [`frontend/`](../../frontend/README.en.md): React 19, TypeScript, Vite, Tailwind CSS 4 and TanStack
Query, with an API layer generated by orval from `api/openapi.json`. It has its own `AGENTS.md`; read it before changing the
frontend.

```sh
task frontend:install   # npm ci
task frontend:dev       # Vite :5173
task frontend:check     # the same frontend gate as CI
```

Pages not yet backed by the server get demo data from MSW's `/mock-api/*`.

## Next

- Running tests and gates: [Testing and gates](testing.en.md)
- Changing the code: [Changing the code](changing-the-code.en.md)
