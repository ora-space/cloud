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
| Repo Executor | `/app/repo-executor` | The only process reaching external Git repositories: sync, explicit publish and read-only probes; stateless, with no database |
| Credential Store | `/app/credential-store` | Independent custody of credentials: browser submission, executor redemption, Cloud management, revocation and expiry sweeping; ciphertext only |
| cloudctl | `/app/cloudctl` | Migrations, first-tenant bootstrap, credential reference registration; run once, not a service |
| PostgreSQL 17 | — | The only business authority |
| S3-compatible object storage | — | Revision session JSONL (Revisions from before the switch to Git also have bundles); optional, no Revision is delivered without it |

Outside Cloud, cluster also deploys the Controller (desktop's `ora-controller`), the Sandbox Server (cluster's own
Substrate implementation) and the Node image (desktop's `ora-node`).

## Images

`scripts/Dockerfile` produces four images:

- **The runtime image** (default target): the five statically built binaries `server`, `gateway`, `model-gateway`,
  `git-server` and `cloudctl`, the sample `configs/config.yaml` and `configs/gateway.yaml`, and the frontend build at
  `/app/web`; it includes `git` and `git-daemon` (git-server needs `git http-backend`). The default entry point is the
  server, other processes are selected with `--entrypoint`, and everything runs as a non-root user (UID 10001).
- **The devsetup image** (`devsetup` target): `devsetup` (local key generation and migrations) and `git-server-preset`
  (an acceptance-only Git Server preset tool), used by cluster's Compose and **never** part of the service image.
- **The Repo Executor image** (`repo-executor` target): the `repo-executor` binary plus `git-credential-ora`, the only
  credential helper Git may use; it carries `git` and CA certificates. It runs as UID 10002, and the deployment mounts
  its task directory `/var/lib/repo-executor/tasks` as a tmpfs that is emptied at startup; **never** part of the runtime
  image.
- **The Credential Store image** (`credential-store` target): only the `credential-store` binary, running as UID 10003;
  its durable data lives only in the `/var/lib/ora-credentials` volume, mode `0700`. **Never** part of the runtime image.

cluster's Compose runs both the Repo Executor and the Credential Store with a read-only root filesystem,
`cap_drop: ALL` and `no-new-privileges`, so no key ever lands in a writable layer; production deployments should
keep the same restrictions.

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

cluster's Compose places services on four networks, and production deployments should keep the same isolation:

| Network | Members | Purpose |
|---|---|---|
| Default network | Cloud, Gateway, model-gateway, git-server (ref queries), `credential-edge`, Controller, Sandbox Server, PostgreSQL, object storage | Internal service-to-service traffic |
| Sandbox network | Sandboxes, Controller, Sandbox Server, model-gateway (`ora-model-gateway`), the git-server repository listener (`ora-git-server`), the object storage public address | Everything a sandbox may reach |
| `git-services` (`internal: true`) | Cloud, the git-server service repository listener (`ora-git-service`), the Repo Executor, the Credential Store and `credential-edge` | Traffic inside the external Git line; no egress, and sandboxes never join |
| `repo-egress` | The Repo Executor only | The only network that reaches external Git repositories |

Sandboxes can reach only the model proxy, the Git Server repository listener and the object storage public address;
they cannot reach Cloud, the Gateway or the database directly. On the external Git line, Cloud joins both the default
network and `git-services`, so its calls to the executor, the Credential Store and the ref query all stay on the
internal network; the Credential Store joins only `git-services`; the Repo Executor joins `git-services` and
`repo-egress`, which makes it the only process with egress.

Browser credential submission never passes through Cloud: `credential-edge` is an nginx that publishes only the
Store's submission ingress at `127.0.0.1:8460` (HTTPS, TLS 1.3, a 16 KiB body limit), clears `X-Forwarded-For` and
cookies before forwarding, and rate-limits by the TCP source address.

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
4. **The external Git line (optional)**: prepare the executor's, Credential Store's and Git Server's certificates, CAs,
   the two Ed25519 private keys and the keyring, start the Repo Executor and the Credential Store first, then set
   `repository.enabled` to `true` and start Cloud. Cloud only validates the material at startup and makes no network
   call; once sync or publish tasks exist, the background loop claims a pass every 2 seconds.

For **upgrades**, run the new version's migrations first, then start the new binaries. Migrations only append and
applied files cannot change; once the database contains a new migration, an old binary refuses to start on restart
because of the unknown migration, so a rollback has to deal with the database as well.

## Keys and certificates

| Process | Materials needed |
|---|---|
| Cloud server | The Gateway's two verification public keys (`auth.keys`); the control listener's service certificate, private key and Controller client CA; object storage credential files; the CA for the Git Server ref query and Cloud's client certificate and private key; the directory machine key on corporate deployments |
| Gateway | The service and user signing private keys; the PKCE key; the chosen provider's client secret; model-gateway's public CA |
| model-gateway | Its service certificate and key; the CA verifying Node model client certificates; the encryption master key; the Gateway's verification public keys (through Cloud's configuration) |
| git-server | Its service certificate and key; the CA of sandbox git-access certificates; the CA of the Repo Executor certificate; Cloud's ref query client certificate |
| Cloud server (external Git line) | The dispatcher's and the credential manager's mTLS client certificates and keys; the Git Server ref query client certificate and key; the executor CA, the Credential Store CA and the Git Server CA; two Ed25519 private keys signing execution authorizations and browser submission grants (they must differ); the origin of the browser submission ingress |
| Repo Executor | Its service certificate and key, shared by the task listener, Git Server and Credential Store; the CA that issued Cloud's dispatcher certificate; Cloud's execution verification public keys; the Git Server and Credential Store CAs |
| Credential Store | One service certificate and key for its four listeners; the CA that issued executor, management and KEK-operator client certificates; Cloud's execution and submission verification key sets (they must differ); the KEK keyring and credential volume; the browser ingress certificate |
| credential-edge (cluster's Compose) | The browser ingress origin's certificate and key, and the CA verifying the Store's service certificate |
| Controller | A client certificate accepted by the control listener, whose URI is the configured SPIFFE identity |

- Every secret is mounted as a read-only file and kept only on management volumes, never in code directories or runtime
  Workspaces.
- Certificates only prove identity; the three kinds of sandbox certificate (WSS server, `git-access`, `model-access`) may
  chain to the same CA and are told apart by the purpose segment of their URI, so none can stand in for another.
- The Repo Executor's certificate is issued by its own CA: the Git Server's service repository listener accepts only
  clients signed by that CA and its sandbox listener only sandbox certificates, so neither identity can stand in for the
  other. The executor CA's private key stays on the management volume; the Git Server receives only its certificate.
- The two Ed25519 private keys stay only in Cloud; the matching public keys are mounted read-only for the Repo Executor
  (execution) and the Credential Store (one set each for execution and submission). Only Cloud signs a browser
  submission grant, and the Store verifies it with the submission public keys before storing anything.
- To rotate the Gateway's signing keys, configure the old and new `kid` together in Cloud's `auth.keys`, restart, and
  remove the old entry once old credentials have expired.

## Database accounts

| Account | Rights |
|---|---|
| Migration account | Creating and altering tables; used only by `cloudctl migrate` |
| Cloud server, model-gateway, git-server | DML on business tables |
| Gateway | `SELECT/INSERT/UPDATE/DELETE` on `gateway_login_attempts` and `gateway_sessions` only, and `SELECT` on `schema_migrations` |

The Repo Executor and the Credential Store do not connect to PostgreSQL: Cloud records every task's state, and the
Store's data lives in its own credential volume. Production connections use TLS; never use the sample's local trust
configuration.

## Shared egress

The Cloud server, every model-gateway and every Repo Executor mount one read-only `deployment-egress.json`. The first
two point at it with `-deployment-egress` or `ORA_DEPLOYMENT_EGRESS_FILE`; the Repo Executor uses
`REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` (which wins) or `ORA_DEPLOYMENT_EGRESS_FILE`. To replace it, stop all three
kinds of process, confirm the old processes have exited, then swap the file and start again; editing the file alone
does not change the snapshot already loaded. The policy is in
[Shared deployment egress](deployment-egress.en.md).

## Health checks

| Process | Check |
|---|---|
| Cloud server | `GET /healthz`: an unreachable database returns `503 database_unavailable`; reports whether object storage and the Git Server are configured (`objectStore`, `gitServer`), which does not mean they are online. The response also carries `X-Ora-Egress-Schema` and `X-Ora-Egress-Digest` |
| Gateway | `GET /healthz`: PostgreSQL liveness |
| model-gateway | `model-gateway -healthcheck`: verifies HTTPS and the public CA normally and checks database readiness, without reading user keys, the master key or the egress digest. `GET /healthz` on the credential listener is what carries the same `X-Ora-Egress-Schema` and `X-Ora-Egress-Digest` as Cloud |
| git-server | `git-server -healthcheck`: calls `/healthz` on the ref listener and verifies the server certificate against the deployment CA, without reading private keys |
| Repo Executor | `GET /healthz` on its health listener (`REPO_EXECUTOR_HEALTH_ADDR`, `:8084` by default, plaintext): a successful listen means the certificates, CAs and public keys are usable; the response carries the same `X-Ora-Egress-Schema` and `X-Ora-Egress-Digest` as Cloud |

The Credential Store exposes no separate health endpoint: a successful listen means the certificates, CA, public keys
and keyring are usable; redemption failures surface through Cloud's task state and the Store's logs.

Health checks report only whether dependencies are ready and never reveal configuration.

## Shutdown

- **Cloud server**, on SIGTERM or SIGINT, first sends `Drain` on the Controller signal stream, then stops gRPC and HTTP
  gracefully within 10 seconds, and finally waits for the background loops' in-flight passes to finish. Cut Controller
  calls are retried with the same submission identity, so no durable state is lost.
- **Gateway** stops HTTP within 10 seconds and stops the session cleanup loop.
- **model-gateway** cancels in-flight model streams first, then waits for every listener to exit.
- **git-server** drains its three listeners first, then closes the pre-receive hook channel.
- **Repo Executor** cancels in-flight tasks: Git processes are killed, each task's staging directory and credential
  material are destroyed, and it stops within 30 seconds.
- **Credential Store** drains its four listeners within 10 seconds and waits for the expiry sweep loop to exit.

## Multiple instances

All authoritative state lives in PostgreSQL, and the background loops re-read and transition with a compare-and-set in
every transaction, so several server instances running together never advance anything twice. But SSE invalidation
events and Controller signals are delivered in process only: a browser only receives events committed on the instance it
is connected to, and the Controller's `Watch` only receives that instance's signals. Both are hints anyway — clients
rely on re-reading REST and the Controller on periodic claims, so no work is lost — but with several instances live
refresh becomes slower.

The external Git line does not change this: sync and publish leases, publish checks and credential verifications all
live in PostgreSQL and do not depend on the executor instance's identity; the Repo Executor is stateless, empties its
task directory at startup and relies on Cloud to dispatch work again after a restart. The Credential Store's data lives
in its own credential volume, whose scaling and sharing are decided separately and are outside Cloud's multiple-instance
scope.

## Backups

| Data | Notes |
|---|---|
| PostgreSQL | All business state |
| The model-gateway master key volume | Without it the key ciphertexts in the database cannot be decrypted; **back it up together with the database** |
| The git-server repository volume | Project repositories (`GIT_SERVER_REPOSITORY_ROOT`) |
| The object storage bucket | Revision session JSONL (and bundles registered before the switch to Git) |
| The Credential Store credential volume and KEK keyring | External Git credential ciphertext and the key that opens it; **back them up together**, since losing the keyring makes the volume unreadable |
| Certificate and key volumes | Each process's identity material; the Repo Executor has nothing to back up, as its task directory is a tmpfs |

## CI

The repository has two GitHub Actions workflows:

- **Backend**: with a PostgreSQL 17 service and a pinned RustFS, runs format and static checks, the full integration suite
  with the race detector, proto checks and the build.
- **Frontend**: triggered only when `frontend/`, `api/` or `internal/contract/` change; checks that the generated client
  matches the contract and runs format, lint, type, tests with coverage, module docs and tests, dead code, duplication and
  build gates.

The test gates are described in [Testing and gates](../development/testing.en.md).
