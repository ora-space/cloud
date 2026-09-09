# internal/simulator: Execution Doubles for Development & Testing

`internal/simulator` provides in-process and disk-backed execution doubles representing the Controller, Workspace Node, and Substrate storage systems for Ora Cloud's phase-one development and acceptance testing.

## Responsibilities

### Substrate double (`substrate.go`)
- Simulates external storage and effect journal execution over local HTTP.
- Manages effect journal JSON files on disk (`<root>/effects/<effect-id>.json`).
- Executes mock infrastructure operations:
  - **Storage**: Prepares local project directories (`<root>/projects/<project-id>`).
  - **Worktree**: Performs real Git clone and worktree checkouts using local Git CLI.
  - **Sandbox**: Simulates sandbox instance allocation and termination lifecycles.
- Supports deterministic fault injection (`SetFault`) for testing error recovery and retry policies.

### Controller double (`controller.go`)
- Simulates the external control plane worker:
  - Periodically acquires and renews controller leases via `/internal/v1/controller-lease/acquire`.
  - Claims pending operations via `/internal/v1/operations/claim`.
  - Plans and executes required effects against Substrate.
  - Reports effect execution outcomes via `/internal/v1/operations/{oid}/effects/{eid}/result`.
  - Advances or defers operations with proper monotonic epoch fencing.

### Ephemeral credential issuer
- `NewCredentials()` generates in-memory Ed25519 cryptographic keypairs for the four distinct actor roles: `gateway`, `controller`, `node`, and `user`.
- Signs short-lived JWT tokens on demand for simulator test runs, matching production cryptographic token structures without requiring external authentication infrastructure.

## Boundaries and invariants

- **Development and testing only**: This package is an execution double. It is never deployed to production environments or imported by production daemon binaries.
- **Contract fidelity**: The simulator interacts with the core cloud server strictly over standard HTTP APIs and respects all leasing, fencing, and idempotency contracts.

See [cmd/simulator](../../cmd/simulator/README.md), [Execution contract](../../docs/execution-contract.md), and [Integration tests](../../integration/README.md).
