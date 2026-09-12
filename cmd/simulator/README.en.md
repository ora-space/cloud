# cmd/simulator: End-to-End Local Execution Simulator

[中文](README.md) | [English](README.en.md)

`cmd/simulator` provides a complete local demonstration harness for Ora Cloud's phase-one architecture. It spins up in-process execution doubles, an ephemeral Git repository fixture, and loopback HTTP services to validate the full project lifecycle without external cloud infrastructure.

## Responsibilities

- **Durable demo fixture**: Initializes a real local Git repository fixture under `.local/demo/fixture` with initial commits and branches.
- **In-process cloud server**: Launches an ephemeral `httptest.Server` serving the complete Gin router, backed by a real PostgreSQL database.
- **Substrate simulation**: Launches a local HTTP server exposing Substrate storage and effect simulation under `.local/demo/substrate`.
- **Ephemeral cryptography**: Generates in-memory Ed25519 keypairs for `gateway`, `controller`, `node`, and `user` roles to sign and verify short-lived JWT tokens without external IdP infrastructure.
- **Controller execution loop**:
  1. Bootstraps a demo tenant and user.
  2. Acquires a controller lease via `/internal/v1/controller-lease/acquire`.
  3. Dispatches a project creation request via the public API (`POST /api/v1/tenants/{tid}/projects`).
  4. Simulates Controller queue draining: executes the effect plan (allocating project storage, provisioning Git worktrees, scheduling sandboxes, and registering nodes).
  5. Verifies that the workspace reaches `ready` state and queries it through the public API.
  6. Releases the controller lease cleanly.
  7. Emits JSON summary to `stdout`.

## Boundaries and invariants

- **Testing and demonstration only**: The simulator is an engineering double. It does not interface with Kubernetes, deploy real container sandboxes, or launch live Deno or agent runtimes.
- **Real boundaries preserved**: Uses real HTTP framing, real PostgreSQL schema constraints, and real local Git CLI operations; it does not bypass the domain state machine.

See [cmd overview](../README.en.md), [Simulator internals](../../internal/simulator/README.en.md), and [Execution contract](../../docs/execution-contract.md).
