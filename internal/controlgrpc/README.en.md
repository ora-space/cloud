# internal/controlgrpc: gRPC server of the Controller internal control contract

[中文](README.md) | [English](README.en.md)

`internal/controlgrpc` serves the [`internal/controlpb`](../controlpb/README.en.md) contract over gRPC. It
is a translation layer only: every RPC reads the calling Controller's identity from metadata, converts the request
into a `core.ControlRequest`, runs it through the same `Store.Control` transaction as the JSON internal
API, and maps the resulting `Fault` to a gRPC status. No business rule, cache or implicit retry lives here.

## Caller identity

Production requires TLS 1.3 mutual authentication: both peers verify a trusted certificate.
The Controller certificate must carry the configured service URI and its ControllerId must
match `x-ora-controller-id`. Metadata cannot select another service identity. Missing or
incorrect certificates are rejected; plaintext has no production fallback. `NewDevelopment`
is only an explicit in-process test fixture, never the production listener.

## Error mapping

The status code is the primary classification and `ErrorDetail{ErrorCode}` is attached as a
`google.rpc.Status` detail; both are decided in one place (`fault.go`):

| `Fault` | gRPC status | `ErrorCode` |
|---|---|---|
| `lease_held` | `FAILED_PRECONDITION` | `LEASE_HELD` |
| `stale_controller`, `stale_operation` | `FAILED_PRECONDITION` | `STALE_CONTROLLER` |
| other 409 | `ABORTED` | `CONFLICT` |
| 400 | `INVALID_ARGUMENT` | `INVALID_INPUT` |
| 403 | `PERMISSION_DENIED` | `SERVICE_FORBIDDEN` |
| 404 | `NOT_FOUND` | `NOT_FOUND` |
| database failure | `UNAVAILABLE` (no database detail) | `UNAVAILABLE` |

## Implemented services

- `ControllerLeaseService`: `AcquireLease` / `RenewLease` / `ReleaseLease` map to `lease_acquire` /
  `lease_renew` / `lease_release`; one global lease, 30-second expiry, monotonic `epoch`, expiry judged
  by the database clock.

- `ExecutionService`: the execution registry of the clone loop, mapping to the `clone_*` control
  actions. `ClaimWork` is a pure read (ownership is decided inside the `RecordDispatch` transaction);
  `RecordDispatch` / `TakeOverNodeEvent` / `RecordQueriedResult` carry a `submission_id`: the same
  identity with the same content replays the recorded response, different content fails with
  `ABORTED+CONFLICT`; `GetDispatch` / `ListPendingDispatches` are recovery reads that need no lease.
  Input and result are stored in fixed JSON shapes (`{kind, repositoryUrl, branch}`;
  `{node, outcome, path, commit | reason, retainedPath}`) and conflicts are compared on those shapes.

- `WorkspaceOperationService`: the Workspace lifecycle operations (specs
  `decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md`).
  `ClaimOperation` / `PlanEffect` / `RecordEffectResult` / `AdvanceOperation` / `DeferOperation` map
  to the same control actions as the `/internal/v1/operations` JSON routes, so both surfaces share
  lease, operation-version and step fencing. `ClaimOperation` claims (bumping the version on a
  re-claim), it is not a pure read. The snapshot carries the operation's clone executions; retired
  storage/worktree effects are left out. `ListLiveSandboxes` has no JSON route: it is the lease
  holder's read-only list of sandboxes to hold Node sessions with (not terminating or terminated,
  current generation, ensure succeeded), with the ensured NodeId, Substrate identity and unended
  Node records, so a restarted Controller reconnects without waiting for each Workspace's next
  operation.

- `NodeReportService`: the Controller reports the desktop Nodes it holds sessions with.
  `RegisterNode` is idempotent per (sandbox instance, `node_incarnation_id`) and rejects a `node_id`
  other than the one sandbox_ensure returned; a new incarnation is accepted only after the previous
  one ended. `ReportNodeStatus` / `EndNode` / `ReportNodeIdle` share the Node-credential routes'
  transactions and fencing.

- `ControlSignalService.Watch`: the Controller-opened server stream. Opening verifies the epoch with
  `lease_check` (read-only, no renewal); afterwards it forwards signals from the in-process
  `core.ControlHub`: `WorkAvailable{operation_id}` after a `clone_requests` row commits, `OperationAvailable{operation_id}` after a Workspace operation is created or retried, `Drain` before
  the server stops. At-most-once, not persisted, a slow subscriber loses signals; after `Drain` the
  stream ends cleanly (EOF) and new `Watch` calls during shutdown return `UNAVAILABLE`. The stream ends with
  OK only while draining and with an error status otherwise, so a holder may treat a clean end as `Drain`;
  `Drain` only says this instance is about to stop and does not ask the holder to release its lease. The
  listener accepts client HTTP/2 PINGs spaced at least 5 seconds apart, also while the connection has no
  active stream, so the Controller's keepalive is never cut by `GOAWAY(too_many_pings)`; the server sends
  no PINGs of its own. The listen address comes
from `control.grpc_addr`; production always uses authenticated TLS.

## Runtime controls

`RuntimeControlService` delivers exact tenant, Workspace, sandbox, runtime generation, Node incarnation,
user control epoch and Controller lease epoch bindings. Binding acknowledgements reconcile closed input
and unfinished responsibility; a fresh execution permit is required before dispatch. Delayed snapshots
return `ABORTED stale_runtime_control` and cannot renew or revive a withdrawn session.
See [runtime-control](../../docs/runtime-control.en.md) for the business authority and evidence limits.

## Personal model references and capabilities

Session codecs carry nonempty `model_binding_id` only, preserving Echo input compatibility.
Node registration stores the handshake's model-proxy capability; replay rejects capability changes
for the same incarnation. Personal-model dispatch and authorization refuse older Nodes without it.
No API key or temporary model access token enters this service. See [model connections](../../docs/model-connections.en.md).
