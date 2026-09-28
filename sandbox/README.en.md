# Agent Substrate sandbox adapter

This directory implements the Substrate boundary defined by `docs/execution-contract.md`. It contains the lifecycle effects service, the controlled Ora Node WebSocket router, the in-sandbox runtime, deployment examples, and a minimal protocol client.

The current implementation targets single-cluster POC and integration validation. It listens on loopback by default and invokes Agent Substrate through `kubectl-ate`. Production deployments must add workload authentication, TLS, Project/Workspace authorization, and transactional highly available persistence.

## Layout

| Path | Purpose |
| --- | --- |
| `service/` | `GET/PUT /effects/{effectId}` and the `/ora-node/v1` WebSocket router |
| `runtime/` | Actor command endpoint and Ora Node v1 protocol |
| `deploy/` | ActorTemplate and systemd examples |
| `examples/controller-demo/` | Minimal binary WebSocket Controller |
| `docs/` | API, deployment, recovery, and validation evidence |

## Invariants

- Cloud assigns the UUID effect ID, which is also the idempotency key.
- The service persists intent before starting an external action.
- The same ID and request are retryable; a changed request returns `409`.
- A Workspace has at most one unterminated sandbox.
- Termination persists a tombstone before suspending, tagging, and deleting the Actor.
- Workspace data survives sandbox replacement until `workspace_data_delete`.
- `nodeId` is stable per Workspace; `incarnation_id` changes with each Node process.
- The router transparently proxies binary messages up to 16 MiB.
- Lifecycle APIs never accept Git credential values.

## Build and test

```bash
go test ./sandbox/...
go test -race ./sandbox/...
go vet ./sandbox/...

go build -o ./bin/ora-sandbox-service ./sandbox/service
go build -o ./bin/ora-sandbox-runtime ./sandbox/runtime
go build -o ./bin/ora-controller-demo ./sandbox/examples/controller-demo
docker build -f sandbox/runtime/Containerfile -t ora-sandbox-runtime:dev .
```

See [effects-api.md](docs/effects-api.md) and [deployment.md](docs/deployment.md) for the full contract and operations.
