# internal/simulator: Execution Doubles for Development & Testing

[中文](README.md) | [English](README.en.md)

`internal/simulator` provides in-process and disk-backed execution doubles representing the Controller, Workspace Node, and Substrate storage systems for Ora Cloud's phase-one development and acceptance testing.

## Responsibilities

### Substrate double (`substrate.go`)
- Simulates external storage and effect journal execution over local HTTP.
- Manages effect journal JSON files on disk (`<root>/effects/<effect-id>.json`).
- Executes mock infrastructure operations:
  - **Sandbox**: Simulates sandbox instance allocation and termination; sandbox_ensure mounts the Workspace's own data (`<root>/workspaces/<workspace-id>/home`) and returns the Node's `nodeId`.
  - **Workspace data**: `workspace_data_delete` removes one Workspace's data directory.
  - **Node clone** (`node.go`): `PUT/GET /clones/<execution-id>` stands in for the desktop Node fetching the snapshot ref from the Git Server double into a `home/checkout` without any remote, verifying the fetched commit equals `baseCommit` and checking out the Workspace branch's short name with `-B <branch>`, journaled per execution; the external repository is never contacted. `NewSubstrate(root, git *GitServer)` injects the Git Server double.
- Supports deterministic fault injection (`SetFault`) for testing error recovery and retry policies.

### Git Server double (`gitserver.go`)
- `GitServer` keeps Project repositories on disk laid out exactly like the Git Server volume, with repository base URL `https://git-server.simulator.invalid:8445` (nothing listens there; the double maps it onto disk); it enforces no ref rules.
- `Config()` hands it to a Store as `core.GitServer`, so Cloud's ref query is answered from disk.
- `SeedSnapshot` stands in for the external line through the git-server-preset fixtures to record a Project's snapshot.

### Controller double (`controller.go`)
- Simulates the external control plane worker:
  - Periodically acquires and renews controller leases via `/internal/v1/controller-lease/acquire`.
  - Claims pending operations via `/internal/v1/operations/claim`.
  - Plans and executes required effects against Substrate.
  - Reports effect execution outcomes via `/internal/v1/operations/{oid}/effects/{eid}/result`.
  - Advances or defers operations with proper monotonic epoch fencing.
  - Drives the clone step (`clone.go`) through the gRPC `ExecutionService`: registers the execution with Cloud's frozen `cloneInput` forwarded verbatim, lets the simulated Node run it, records the queried result, and defers `clone_failed` to retry_wait; it drops an operation that an advance left non-running (parked or failed).

### Agent session double (`agent.go`)

Every bounded Controller step services run work, Thread commands and Node evidence so quiesce cannot starve its end command. Disk-backed `AgentNode` freezes input, deduplicates commands and echoes/ends a session. Delivery first pushes the final commit to the delivery ref on the Git Server double (changed and unchanged alike), then tries to fast-forward `workspaceRef`, and uploads only the sealed JSONL with checksum-signed PUT headers, refreshing expired grants for the same key. Terminal protojson evidence survives Controller/Node restart; grants never enter the journal. Real PostgreSQL and S3 tests prove A's loop, not production Rust relay or a real D Agent.

Uploads carry signed `If-None-Match: *` and cannot replace the first object. A 412 after a lost response preserves that object and submits the declaration for Cloud verification; it does not prove matching bytes. Controller cancellation returns its cause without journaling a delivery terminal fact, so restart can continue the original execution. Actual Node terminal failures remain durable.

Before the first PUT, the Node durably freezes JSONL bytes and metadata under its journal root. Restart after a partial upload reuses that payload rather than creating a new timestamped snapshot. Once terminal evidence is atomically saved, artifact copies are removed and the original terminal remains replayable. Historical pending journals with `Result: null` remain pending; new journals omit absent results. Grants never enter the preparation plan or terminal journal.

### Ephemeral credential issuer
- `NewCredentials()` generates in-memory Ed25519 cryptographic keypairs for the four distinct actor roles: `gateway`, `controller`, `node`, and `user`.
- Signs short-lived JWT tokens on demand for simulator test runs, matching production cryptographic token structures without requiring external authentication infrastructure.

## Boundaries and invariants

- **Development and testing only**: This package is an execution double. It is never deployed to production environments or imported by production daemon binaries.
- **Contract fidelity**: The simulator interacts with the core cloud server strictly over standard HTTP APIs and respects all leasing, fencing, and idempotency contracts.

See [cmd/simulator](../../cmd/simulator/README.en.md), [Execution contract](../../docs/execution-contract.md), and [Integration tests](../../integration/README.en.md).
