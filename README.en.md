# Ora Cloud

English | [中文](README.md)

Ora Cloud is the authoritative server for Ora's multi-user collaboration. It holds the business state of tenants,
members, Projects, runtime Workspaces, Issues, Agent runs, plugins and workflows, serves the HTTP API to browsers through
the Gateway, and serves the internal control contract to the Controller over mTLS gRPC. PostgreSQL is the only
authoritative store, every decision is made in a short transaction, and intent is persisted before any external side
effect runs. See the [architecture overview](docs/architecture.en.md) for the whole picture.

## Processes

| Process | Entry point | Responsibility |
|---|---|---|
| Cloud server | [`cmd/server`](cmd/server/README.en.md) | The public API, SSE invalidation events, the Controller's gRPC control listener, state machines and background loops |
| Gateway | [`cmd/gateway`](cmd/gateway/README.en.md) | The browser's only entry point: external login, sessions, forwarding to Cloud with freshly minted internal credentials |
| model-gateway | [`cmd/model-gateway`](cmd/model-gateway/README.en.md) | Custody of personal model keys and forwarding of model requests |
| git-server | [`cmd/git-server`](cmd/git-server/README.en.md) | Git service for Project repositories, authorized per request from Cloud's state |
| cloudctl | [`cmd/cloudctl`](cmd/cloudctl/README.en.md) | Migrations, first-tenant bootstrap, credential reference registration |

The product frontend lives in [`frontend/`](frontend/README.en.md) and can be served by the Gateway on the same origin in a deployment. The
Controller and Node live in [ora-space/desktop](https://github.com/ora-space/desktop); the Sandbox Server and the complete
Compose environment live in [ora-space/cluster](https://github.com/ora-space/cluster).

## Quick start

You need Go (the version in `go.mod`), Git, PostgreSQL 17, [Task](https://taskfile.dev) and `buf`; the frontend also needs
Node 24.

```sh
docker compose up -d --wait           # local PostgreSQL 17 on 127.0.0.1:55432
cp config.toml.template config.toml   # the defaults work; config.toml is ignored by Git
task setup                            # generate keys and secret files, write .local/dev.env, run migrations
task dev                              # Cloud :8080 + Gateway :8081 + frontend :5173
task check                            # formatting, lint and the full PostgreSQL test suite
```

The Windows database, the control listener certificates and other details are in
[Local development](docs/development/local-development.en.md); test layers and gates are in
[Testing and gates](docs/development/testing.en.md).

## Contracts

- HTTP: [`api/openapi.json`](api/openapi.json) is generated from `router.Routes` and `internal/contract`, and
  `frontend/src/api` is generated from it; `task frontend:generate` does both. It is the reference for endpoint fields;
  conventions are in [HTTP API conventions](docs/core/http-api.en.md).
- Controller: [`proto/`](proto/README.en.md) defines the gRPC control contract and `internal/controlpb` is generated from
  it; see [Controller integration](docs/runtime/controller-integration.en.md).
- Business decisions: the ADRs and core test cases in [specs](https://github.com/ora-space/specs/tree/main/decisions/cloud).

## Documentation

- [Documentation map](docs/README.en.md): architecture, each domain, operations and development.
- Implementation details of each package live in its directory's `README.md`: [`cmd/`](cmd/README.en.md),
  [`internal/`](internal/README.en.md), [`integration/`](integration/README.en.md), [`pkg/`](pkg/README.en.md).
- Coding rules and the change workflow: [AGENTS.md](AGENTS.md), [Changing the code](docs/development/changing-the-code.en.md).
