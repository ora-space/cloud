# proto: single source of the Controller internal control contract

[中文](README.md) | [English](README.en.md)

`proto/ora/cloud/internal/v1/` is the single source of the internal control contract
(package `ora.cloud.internal.v1`) between Cloud and a Controller. Cloud is the server for every RPC
and owns authoritative persistence; a Controller only dials out and exposes no service to Cloud.
Consumers (`ora-controller` in `ora-space/desktop`) pin a commit of this repository and generate
their own clients; they never copy the `.proto` files.

| File | Content |
|---|---|
| `errors.proto` | `ErrorDetail{ErrorCode}` attached as a `google.rpc.Status` detail; the gRPC status code is the primary classification, the enum refines it |
| `lease.proto` | `ControllerLeaseService`: the global coordination lease whose `epoch` fences every write |
| `executions.proto` | `ExecutionService`: claim work, register before dispatch, take over Node events, store queried results, recovery reads; the first version covers the clone closed loop only |
| `operations.proto` | `WorkspaceOperationService`: claim, plan effects, record effect results, advance and defer Workspace lifecycle operations |
| `nodes.proto` | `NodeReportService`: the Controller registers the desktop Nodes it holds sessions with (`node_id` + `node_incarnation_id`) and reports their status, end and idle |
| `signals.proto` | `ControlSignalService.Watch`: a Controller-opened server stream carrying `WorkAvailable` / `OperationAvailable` / `Drain` / `NodeAssignment` |

## Semantics

- **Submission identity**: state-changing requests carry a caller-generated `submission_id`. The same
  identity with identical content returns the original response without reapplying; the same identity
  with different content fails with `ABORTED` + `CONFLICT`. A lost reply is retried with the same
  identity, never a new one.
- **Boundary order**: only after `RecordDispatch` succeeds may a Controller dispatch to a Node; only after
  `TakeOverNodeEvent` succeeds may it acknowledge that event sequence; `RecordQueriedResult` never
  authorizes an acknowledgement.
- **Signal stream**: at-most-once, not persisted, changes no ownership; after a stream loss the Controller
  falls back to periodic `ClaimWork`.
- **No tenant fields**: the contract carries only opaque identities Cloud has already authorized; review
  rejects changes that add tenant / user / membership fields.

## Generation and checks

- `buf.yaml`: `STANDARD` lint, `FILE`-level breaking; `v1` accepts additive changes only, a breaking change
  is expressed as a sibling `v2` package.
- `buf.gen.yaml`: pinned `protocolbuffers/go` and `grpc/go` remote plugins generating into
  [`internal/controlpb`](../internal/controlpb/README.en.md) (needs network, not a local `protoc`).
- `task proto:lint`, `task proto:breaking` (against `PROTO_BASE_REF`, default `main`), `task proto:generate`,
  `task proto:check` (regenerate and fail on diff); `task check` and CI run lint, breaking and the drift check.

## Changing the contract

1. Edit the `.proto` files, run `task proto:lint` and `task proto:breaking`.
2. Run `task proto:generate` and commit the generated code with the change; once merged, that commit is the
   contract version consumers pin.
3. desktop moves its submodule pointer to that commit and regenerates its client; a compile failure is the
   misalignment signal.

The semantics belong to specs
`decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md`; this directory only
carries the fields.
