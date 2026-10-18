# internal/simulator: Execution Doubles for Development & Testing

[中文](README.md) | [English](README.en.md)

`internal/simulator` provides in-process and disk-backed execution doubles representing the Controller, Workspace Node, and Substrate for Ora Cloud's development and integration tests.

## Responsibilities

### Substrate double (`substrate.go`)
- Simulates external storage and effect journal execution over local HTTP.
- Manages effect journal JSON files on disk (`<root>/effects/<effect-id>.json`).
- Executes mock infrastructure operations:
  - **Sandbox**: Simulates sandbox instance allocation and termination; sandbox_ensure mounts the Workspace's own data (`<root>/workspaces/<workspace-id>/home`) and returns the Node's `nodeId`.
  - **Workspace data**: `workspace_data_delete` removes one Workspace's data directory.
  - **Node clone** (`node.go`): `PUT/GET /clones/<execution-id>` stands in for the desktop Node running a clone with the local Git CLI into `home/checkout`, journaled per execution.
- Supports deterministic fault injection (`SetFault`) for testing error recovery and retry policies.

#### HTTP interface

The Substrate double listens on loopback only, for the simulator and tests; it does not define the production Substrate's interface:

| Method and path | Request | Response and invariants |
|---|---|---|
| `GET /effects/{effectId}` | The effect UUID Cloud allocated in advance | `404` means the intent is known not to exist; otherwise the original request, `externalId`, `state` and `result`; a query never creates an effect |
| `PUT /effects/{effectId}` | `kind`, `projectId`, `workspaceId`, plus `sandboxInstanceId` for terminate | Journaled before it runs; the same ID with the same request is idempotent and with a different request is `409`; a succeeded effect returns its original result |
| `PUT /clones/{executionId}`, `GET /clones/{executionId}` | `workspaceId`, `repositoryUrl`, `branch` | Stands in for a Node running a clone: one execution runs once and is journaled per execution; returns `clone_ready` with `commitId`, or `clone_failed` with a reason |

| `kind` | Result |
|---|---|
| `sandbox_ensure` | `sandboxInstanceId`, `nodeId`; fails once the sandbox has terminated |
| `sandbox_terminate` | `terminated`; the sandbox must belong to the request's Workspace |
| `workspace_data_delete` | `removed` |

`SetFault` modes: `fail` and `unconfirmed` journal a `running` effect and answer `503`; `lose_response` performs the effect but answers `504`, to exercise
"query by stable identity when the outcome is unknown"; `fail` for clones returns `clone_failed`. Repositories come only from an explicit repository URL →
local fixture mapping, never real Git credentials. Plugin installs are Node executions now and Cloud refuses to plan plugin effects, so the double's leftover `plugin_ensure` branch is never reached from Cloud.

### Controller double (`controller.go`)
- Simulates the external control plane worker. The lease, operations and Node registration use the retired `/internal/v1/...` JSON endpoints, which only
  `router.NewDevelopment` opens and the production server answers with `410`; executions and Agent runs use gRPC:
  - Periodically acquires and renews controller leases via `/internal/v1/controller-lease/acquire`.
  - Claims pending operations via `/internal/v1/operations/claim`.
  - Plans and executes required effects against Substrate.
  - Reports effect execution outcomes via `/internal/v1/operations/{oid}/effects/{eid}/result`.
  - Advances or defers operations with proper monotonic epoch fencing.
  - Drives the clone step (`clone.go`) through the gRPC `ExecutionService`: registers the execution, lets the simulated Node run it, records the queried result, and defers `clone_failed` to retry_wait.

### Agent session double (`agent.go`)

Every bounded Controller step services run work, Thread commands and Node evidence so quiesce cannot starve its end command. Disk-backed `AgentNode` freezes input, deduplicates commands and echoes/ends a session. Delivery creates cumulative bundles and sealed JSONL from real Git and session data, uploads with checksum-signed PUT headers and refreshes expired grants for the same keys. Terminal protojson evidence survives Controller/Node restart; grants never enter the journal. Real PostgreSQL and S3 tests prove A's loop, not production Rust relay or a real D Agent.

Uploads carry signed `If-None-Match: *` and cannot replace the first object. A 412 after a lost response preserves that object and submits the declaration for Cloud verification; it does not prove matching bytes. Controller cancellation returns its cause without journaling a delivery terminal fact, so restart can continue the original execution. Actual Node terminal failures remain durable.

Before the first PUT, the Node durably freezes bundle/JSONL bytes and metadata under its journal root. Restart after a partial upload reuses that payload rather than creating a new timestamped snapshot. Once terminal evidence is atomically saved, artifact copies are removed and the original terminal remains replayable. Historical pending journals with `Result: null` remain pending; new journals omit absent results. Grants never enter the preparation plan or terminal journal.

### Ephemeral credential issuer
- `NewCredentials()` generates in-memory Ed25519 cryptographic keypairs for the four distinct actor roles: `gateway`, `controller`, `node`, and `user`.
- Signs short-lived JWT tokens on demand for simulator test runs, matching production cryptographic token structures without requiring external authentication infrastructure.

## Boundaries and invariants

- **Development and testing only**: This package is an execution double. It is never deployed to production environments or imported by production daemon binaries.
- **Contract fidelity**: The simulator interacts with the core cloud server strictly over standard HTTP APIs and respects all leasing, fencing, and idempotency contracts.

See [cmd/simulator](../../cmd/simulator/README.en.md), [Workspace lifecycle](../../docs/runtime/workspaces.en.md), and [Integration tests](../../integration/README.en.md).
