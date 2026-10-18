# Deployment and Operations

English | [中文](deployment.md)

This document describes which processes make up Cloud, how they are packaged and deployed, which keys and
certificates each needs, and what to watch for in health checks, shutdown, upgrades and backups. The complete
multi-repository environment (Controller, Sandbox Server, Node) is provided by the Compose setup in
[ora-space/cluster](https://github.com/ora-space/cluster); this document covers only the Cloud side. Every setting
is described in the [configuration reference](configuration.en.md).

## Components

| Component | Entry point | Responsibility |
|---|---|---|
| Cloud server | `/app/server` | The public HTTP API, SSE, the Controller's gRPC control listener, and the background loops for IssueRuns, plugins and workflows |
| Gateway | `/app/gateway` | External login, browser sessions, the `/api/v1` proxy, and optionally the frontend |
| model-gateway | `/app/model-gateway` | Encryption of personal keys and the model request proxy |
| git-server | `/app/git-server` | Smart HTTP and ref queries for Project repositories |
| cloudctl | `/app/cloudctl` | Migrations, first-tenant bootstrap, credential reference registration; run once, not a service |
| PostgreSQL 17 | — | The only business authority |
| S3-compatible object storage | — | Revision objects (bundles, session JSONL); optional |

Outside Cloud, cluster also deploys the Controller (desktop's `ora-controller`), the Sandbox Server (cluster's own
Substrate implementation) and the Node image (desktop's `ora-node`).

## Images

`scripts/Dockerfile` produces two images:

- **The runtime image** (default target): the five statically built binaries `server`, `gateway`, `model-gateway`,
  `git-server` and `cloudctl`, the sample `configs/config.yaml` and `configs/gateway.yaml`, and the frontend build at
  `/app/web`; it includes `git` and `git-daemon` (git-server needs `git http-backend`). The default entry point is the
  server, other processes are selected with `--entrypoint`, and everything runs as a non-root user (UID 10001).
- **The devsetup image** (`devsetup` target): `devsetup` (local key generation and migrations) and `git-server-preset`
  (an acceptance-only Git Server preset tool), used by cluster's Compose and **never** part of the service image.

Deployments mount their own configuration files, public keys, certificates and key files. With
`GATEWAY_WEB_DIST_DIR=/app/web` the Gateway serves the frontend on the same origin as `/auth` and `/api`.

## Huawei IDaaS site configuration

Authentication URLs and the Client ID come from configuration outside the image. The Client Secret is supplied by a
read-only mounted file named by `GATEWAY_IDAAS_CLIENT_SECRET_FILE`. See the
[configuration reference](configuration.en.md#gateway) for settings and endpoint validation.

1. In production, disable `GATEWAY_PUBLIC_DEVELOPMENT` and `GATEWAY_LOGIN_DEVELOPMENT_PROVIDER`, set an HTTPS public
   origin, and register exactly `<public origin>/auth/callback/huawei-idaas` with IDaaS.
2. After changing endpoints or the Client ID, recreate the Gateway containers with the updated configuration; no image
   rebuild is required. All replicas use the same client configuration.
3. After atomically replacing the Client Secret file, recreate every Gateway container to refresh the file mount and
   read the new secret; replacing the file alone does not update a running process.

## Networks

cluster's Compose places services on three networks, and production deployments should keep the same isolation:

| Network | Members | Purpose |
|---|---|---|
| Default network | Cloud, Gateway, model-gateway, git-server (ref queries), Controller, Sandbox Server, PostgreSQL, object storage | Internal service-to-service traffic |
| Sandbox network | Sandboxes, Controller, Sandbox Server, model-gateway (`ora-model-gateway`), the git-server repository listener (`ora-git-server`), the object storage public address | Everything a sandbox may reach |
| Git service network | The git-server service repository listener (`ora-git-service`) and internal services that need it (such as the Repo Executor) | An internal network sandboxes never join, with no external egress |

Sandboxes can reach only the model proxy, the Git Server repository listener and the object storage public address;
they cannot reach Cloud, the Gateway or the database directly.

## Deployment order

1. **Migrate**: using the same runtime image with `--entrypoint /app/cloudctl`, run `-command migrate` under a migration
   identity allowed to create tables. Migrations run in file-name order under an advisory lock, and running them again
   is safe.
2. **First tenant (optional)**: `cloudctl -command bootstrap -name … -source … -subject … -display-name …` atomically
   creates a tenant, its sole space and its first administrator; on Huawei IDaaS deployments `-subject` must be the
   employee's IDaaS `uuid`. Users can also create tenants themselves afterwards.
3. **Start the services**: the server, Gateway, model-gateway and git-server each verify the migration state at startup:
   every migration in the binary must be applied with a matching checksum and the database must contain no migration the
   binary does not know, otherwise startup fails. They never run DDL.

For **upgrades**, run the new version's migrations first, then start the new binaries. Migrations only append and
applied files cannot change; once the database contains a new migration, an old binary refuses to start on restart
because of the unknown migration, so a rollback has to deal with the database as well.

## Keys and certificates

| Process | Materials needed |
|---|---|
| Cloud server | The Gateway's two verification public keys (`auth.keys`); the control listener's service certificate, private key and Controller client CA; object storage credential files; the directory machine key on corporate deployments |
| Gateway | The service and user signing private keys; the PKCE key; the chosen provider's client secret; model-gateway's public CA |
| model-gateway | Its service certificate and key; the CA verifying Node model client certificates; the encryption master key; the Gateway's verification public keys (through Cloud's configuration) |
| git-server | Its service certificate and key; the CA of sandbox git-access certificates; the CA of the Repo Executor certificate; Cloud's ref query client certificate |
| Controller | A client certificate accepted by the control listener, whose URI is the configured SPIFFE identity |

- Every secret is mounted as a read-only file and kept only on management volumes, never in code directories or runtime
  Workspaces.
- Certificates only prove identity; the three kinds of sandbox certificate (WSS server, `git-access`, `model-access`) may
  chain to the same CA and are told apart by the purpose segment of their URI, so none can stand in for another.
- To rotate the Gateway's signing keys, configure the old and new `kid` together in Cloud's `auth.keys`, restart, and
  remove the old entry once old credentials have expired.

## Database accounts

| Account | Rights |
|---|---|
| Migration account | Creating and altering tables; used only by `cloudctl migrate` |
| Cloud server, model-gateway, git-server | DML on business tables |
| Gateway | `SELECT/INSERT/UPDATE/DELETE` on `gateway_login_attempts` and `gateway_sessions` only, and `SELECT` on `schema_migrations` |

Production connections use TLS; never use the sample's local trust configuration.

## Health checks

| Process | Check |
|---|---|
| Cloud server | `GET /healthz`: an unreachable database returns `503 database_unavailable`; reports whether object storage is configured, which does not mean storage is online |
| Gateway | `GET /healthz`: PostgreSQL liveness |
| model-gateway | `model-gateway -healthcheck`: verifies HTTPS and the public CA normally and checks database readiness, without reading user keys or the master key |
| git-server | `git-server -healthcheck`: calls `/healthz` on the ref listener and verifies the server certificate against the deployment CA, without reading private keys |

Health checks report only whether dependencies are ready and never reveal configuration.

## Shutdown

- **Cloud server**, on SIGTERM or SIGINT, first sends `Drain` on the Controller signal stream, then stops gRPC and HTTP
  gracefully within 10 seconds, and finally waits for the background loops' in-flight passes to finish. Cut Controller
  calls are retried with the same submission identity, so no durable state is lost.
- **Gateway** stops HTTP within 10 seconds and stops the session cleanup loop.
- **model-gateway** cancels in-flight model streams first, then waits for every listener to exit.
- **git-server** drains its three listeners first, then closes the pre-receive hook channel.

## Multiple instances

All authoritative state lives in PostgreSQL, and the background loops re-read and transition with a compare-and-set in
every transaction, so several server instances running together never advance anything twice. But SSE invalidation
events and Controller signals are delivered in process only: a browser only receives events committed on the instance it
is connected to, and the Controller's `Watch` only receives that instance's signals. Both are hints anyway — clients
rely on re-reading REST and the Controller on periodic claims, so no work is lost — but with several instances live
refresh becomes slower.

## Backups

| Data | Notes |
|---|---|
| PostgreSQL | All business state |
| The model-gateway master key volume | Without it the key ciphertexts in the database cannot be decrypted; **back it up together with the database** |
| The git-server repository volume | Project repositories (`GIT_SERVER_REPOSITORY_ROOT`) |
| The object storage bucket | Revision bundles and session JSONL |
| Certificate and key volumes | Each process's identity material |

## CI

The repository has two GitHub Actions workflows:

- **Backend**: with a PostgreSQL 17 service and a pinned RustFS, runs format and static checks, the full integration suite
  with the race detector, proto checks and the build.
- **Frontend**: triggered only when `frontend/`, `api/` or `internal/contract/` change; checks that the generated client
  matches the contract and runs format, lint, type, tests with coverage, module docs and tests, dead code, duplication and
  build gates.

The test gates are described in [Testing and gates](../development/testing.en.md).
