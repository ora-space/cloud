# Controller Integration: the Internal Control Contract

English | [中文](controller-integration.md)

The Controller (`ora-controller` in the desktop repository) is the only execution channel between
Cloud and the Nodes inside sandboxes. It only dials out to Cloud's control listener; Cloud is the
server for every RPC and owns the authoritative persistence. The Controller exposes no service to
Cloud and holds no database connection. This document covers the general semantics of this gRPC
contract and the responsibility of each service; fields are defined in [`proto/`](../../proto/README.en.md).

```text
proto/ora/cloud/internal/v1/*.proto     the contract's single source (package ora.cloud.internal.v1)
internal/controlpb                       generated Go code, read-only
internal/controlgrpc                     the gRPC server: caller identity, Fault → status; translation only
internal/core  Store.Control             business transactions shared with the other entry points
```

Each RPC in `controlgrpc` becomes one `core.ControlRequest`, runs through `Store.Control` in one short
transaction, and has its result and error translated back to gRPC. No business rule, cache or implicit
retry lives in the gRPC layer.

## Identity and connection

- The listener at `control.grpc_addr` accepts TLS 1.3 with mutual authentication only; the Controller
  certificate must carry the configured SPIFFE URI and every call carries `x-ora-controller-id`
  metadata (see [Internal credentials and service trust](../identity/authentication.en.md#the-controllers-grpc-identity)).
- The listener accepts client HTTP/2 PINGs no more often than every 5 seconds, even with no active
  stream, so the Controller's keepalive is never cut by `GOAWAY(too_many_pings)`; Cloud sends no PINGs
  of its own.
- On shutdown Cloud first sends `Drain` on the signal stream and then stops gRPC gracefully with the
  same deadline as HTTP. A cut call can be retried with the same submission identity, so no durable
  state is lost.

## General semantics

### Lease and epoch

The whole deployment has one global coordination lease (`ControllerLeaseService`):

- `AcquireLease` takes the lease when it is free or expired and **increments the epoch**; while it is
  still valid and held by someone else the answer is `lease_held`. A holder acquiring again only
  confirms.
- `RenewLease` extends it by 30 seconds; the convention is to renew every 10 seconds. `ReleaseLease`
  expires it immediately. Expiry is judged by the database clock.
- **The epoch is the fencing token for every write.** Claims, registrations, advances, effect plans,
  permits and other writes all check that the caller holds the current lease with a matching epoch,
  and return `stale_controller` otherwise. An old Controller replaced by a new holder fails every write
  even while it keeps running.
- Recovery reads (`GetDispatch`, `ListPendingDispatches`) need no lease: a successor has to find the
  original executions before it can decide what to do next, and reading changes no ownership.

### Submission identity

State-changing requests carry a caller-generated `submission_id`:

- The same identity with the same content returns the first recorded response and is **not applied
  again**; the same identity with different content returns `ABORTED` with `CONFLICT`.
- After a lost reply the request must be resent with the **same** identity, never a new one. The replay
  happens before the version check, so the original response comes back even after the version moved
  on.
- Permit reads (`GetEffectPermit`, `GetExecutionPermit`) deliberately take no part in replay: they are
  decided afresh every time, so a withdrawn permit is never revived by an old response.

### Ordering at the boundary

- Only after `RecordDispatch` succeeds may the Controller dispatch the execution to the Node.
- Only after `TakeOverNodeEvent` succeeds may the Controller acknowledge that sequence number to the
  Node; Cloud counts the event as received only once the takeover commits.
- `RecordQueriedResult` stores a result the Controller queried for, but is not grounds for
  acknowledging anything to the Node.

### Error mapping

The gRPC status code is the primary classification; `ErrorDetail{ErrorCode}` refines it as a
`google.rpc.Status` detail:

| Cloud `Fault` | gRPC status | `ErrorCode` |
|---|---|---|
| `lease_held` | `FAILED_PRECONDITION` | `LEASE_HELD` |
| `stale_controller`, `stale_operation` | `FAILED_PRECONDITION` | `STALE_CONTROLLER` |
| Any other 409 | `ABORTED` | `CONFLICT` |
| 400 | `INVALID_ARGUMENT` | `INVALID_INPUT` |
| 403 | `PERMISSION_DENIED` | `SERVICE_FORBIDDEN` |
| 404 | `NOT_FOUND` | `NOT_FOUND` |
| Database failure | `UNAVAILABLE` (with no database detail) | `UNAVAILABLE` |

### The signal stream

`ControlSignalService.Watch` is a server stream the Controller opens. On open it checks the epoch once
(read-only, no renewal); after that it relays the Cloud process's control signals:

| Signal | When it is sent |
|---|---|
| `WorkAvailable` | A new execution work item (Agent session, Revision delivery) was committed |
| `OperationAvailable` | A Workspace lifecycle operation was created or retried |
| `ThreadCommandAvailable` | A new Thread command was committed |
| `Drain` | This Cloud instance is about to stop; the holder should stop claiming from it |

- Signals are **at most once, not persisted, and change no ownership**; a slow subscriber loses them.
  After the stream ends the Controller falls back to periodic `ClaimWork`, `ClaimOperation` and
  `ClaimThreadCommands`, so a lost signal only delays work and never loses it. Changes with no commit to
  follow, such as a `retry_wait` coming due, rely on periodic claims anyway.
- The stream ends cleanly only when draining (EOF after `Drain`); every other end carries an error
  status, and a new `Watch` during shutdown returns `UNAVAILABLE`. `Drain` does not ask the holder to
  release its lease.
- The proto also defines `NodeAssignment`; Cloud does not send it at present.

## Services

| Service | Responsibility |
|---|---|
| `ControllerLeaseService` | The global lease: `AcquireLease`, `RenewLease`, `ReleaseLease` |
| `WorkspaceOperationService` | Workspace lifecycle operations: `ClaimOperation`, `PlanEffect`, `RecordEffectResult`, `AdvanceOperation`, `DeferOperation`, `RecordProvisioningFact`, `ListLiveSandboxes` |
| `NodeReportService` | The Controller reports the Nodes it holds sessions with: `RegisterNode`, `ReportNodeStatus`, `EndNode`, `ReportNodeIdle` |
| `ExecutionService` | The execution registry: `ClaimWork`, `RecordDispatch`, `TakeOverNodeEvent`, `RecordQueriedResult`, `GetDispatch`, `ListPendingDispatches` |
| `AgentRunService` | Agent runs: `TakeOverThreadEvents`, `ClaimThreadCommands`, `RecordThreadCommandDelivered`, `GrantRevisionUpload`, `GrantRevisionDownload` |
| `RuntimeControlService` | Runtime control: `ListBindings`, `AcknowledgeBinding`, `GetExecutionPermit`, `GetEffectPermit`, `ListForceStops`, `ConfirmForceStop` |
| `ControlSignalService` | The `Watch` signal stream |

**`WorkspaceOperationService`** carries the operations of the [Workspace lifecycle](workspaces.en.md).

- `ClaimOperation` rotates runnable operations (`queued`, a due `retry_wait`, or one already
  `running`) by ascending `COALESCE(last_claimed_at, created_at), created_at, id`. Only an actual
  return updates `last_claimed_at` using database time; this internal field is absent from public
  operations and gRPC snapshots. Each claim transaction examines at most 16 candidates, settling
  eligible deterministic repository credential refusals in that transaction and continuing. An
  exhausted budget returns empty; the next claim continues, so an empty response does not prove
  the queue is empty. See [operation claiming](workspaces.en.md#operation-claiming-and-deterministic-refusals)
  for refusal scope and settlement. A returned operation is set `running` with the current epoch
  and **increments its version**, which fences
  any worker still holding an old snapshot; it is a write, not a pure read. The returned snapshot holds
  everything the operation needs: the Project (with its credential reference), every related Workspace,
  sandbox, Node and effect, and the clone and plugin executions this operation already has, so a
  recovering Controller finds the original execution instead of registering a second one.
- `RecordProvisioningFact` records late real facts against the original operation, effect and execution identities after a run
  Workspace creation was abandoned, and reopens nothing (see
  [Bounded abandonment of a blocked creation](../agent-runs/startup-settlement.en.md#bounded-abandonment-of-a-blocked-creation)).
- `ListLiveSandboxes` is a read-only list for the lease holder of the sandboxes it should hold Node
  sessions with (current generation, not terminating, ensure succeeded), with the `nodeId`, the
  Substrate identifier and the unended Node records, so a restarted Controller can reconnect every Node
  without waiting for the next operation.

**`NodeReportService`** is the only way Node state enters Cloud. `RegisterNode` is idempotent by
(sandbox instance, `node_incarnation_id`); `node_id` must equal the value `sandbox_ensure` returned,
and a new instance is accepted only after the sandbox's previous one has ended. Capabilities the Node
declared during the handshake (such as whether it has the model proxy) are recorded authoritatively
and cannot change on a replay for the same instance. Every report is bound to the Workspace's current
generation and live sandbox.

**`ExecutionService`** is one general execution registry covering several kinds of Node execution:

| Execution kind | Origin | Registry |
|---|---|---|
| `clone` | The `clone` step of a Workspace operation | `clone_executions` |
| `install_plugins`, `remove_plugins` | The `plugin` step of a Workspace operation | `node_executions` |
| `agent_session`, `deliver_revision` | Execution work items declared by an Agent run (`execution_work`) | `node_executions` |

- `ClaimWork` is a **pure read**: it returns the oldest claimable work item, and ownership is decided
  only in `RecordDispatch`'s transaction, so a Controller that crashes between claiming and registering
  leaves nothing to recover.
- For Agent session and Revision delivery work owned by an IssueRun (a work item carrying `run_id`),
  `ClaimWork` skips unregistered items whose Run is `releasing`, `done` or soft-deleted; Runs with
  residuals are also excluded. WorkflowRun session work carries `workflow_run_id` instead and stays
  claimable: the IssueRun lifecycle does not govern it. A new `RecordDispatch` rechecks Run eligibility
  and residuals under the same transaction lock as business settlement, so an earlier claim snapshot
  cannot register a new session or delivery afterwards. Work rows and execution records remain;
  existing executions use the original idempotency, query and result-takeover paths, subject to their
  existing residual and other checks. Leaving the claim queue does not prove Node termination or
  physical Workspace deletion (see [Agent run settlement](../agent-runs/issue-runs.en.md#phases)).
- `RecordDispatch` registers an execution before it is dispatched. Cloud, not the Controller, decides
  the input and the target Node: registration checks that the input equals the one frozen by the
  operation or work item, that the target is the current Node, and that no execution of the same kind is
  still undecided. Replaying the same execution with the same input returns the original registration;
  a contradicting input is a conflict.
- Inside an `install_plugins` input, an agent plugin's install item may carry a frozen declaration
  association (`agent_configuration`, fields per proto): an identity field written from the catalog
  snapshot when the install plan was frozen. It takes part in the same input-equality check, so a
  dispatch that loses or tampers with the association is refused; the static copy never leaves Cloud.
  See [The plugin marketplace](../plugins/marketplace.en.md#agent-declaration-snapshots-and-freezing).
- `TakeOverNodeEvent` takes over Node events by execution identity and **consecutive sequence number**
  and stores a receipt; a repeated event must match the original receipt byte for byte. Terminal events
  of sessions and deliveries use the same sequence and cannot overtake events not yet taken over.

**`AgentRunService`** carries the session side of an Agent run: batched Thread event takeover,
claiming and acknowledging delivery of Thread commands, and issuing object storage upload and download
grants for deliveries. Semantics are in [Threads](../agent-runs/threads.en.md) and
[Revisions](../agent-runs/revisions.en.md).

**`RuntimeControlService`** delivers runtime control bindings, issues execution and effect permits and
carries out force-stops; see [Runtime control](runtime-control.en.md).

## Changing the contract

- `buf.yaml` uses `STANDARD` lint and `FILE`-level breaking checks: `v1` accepts only non-breaking
  changes, and a breaking change is expressed as a parallel `v2` package.
- After editing a `.proto`, run `task proto:lint` and `task proto:breaking`, then `task proto:generate`,
  and commit the generated output with the change. `task check` and CI run lint, breaking and drift
  checks on the generated code.
- The commit merged into main is the contract version consumers can pin. desktop pins a Cloud commit as
  a submodule and generates its own client without copying the `.proto` files; a desktop build failure
  means the contract is out of step.
- The contract's semantics are owned by specs ADRs; this repository's `proto/` only carries the fields.

## Related decisions

- [Cloud owns the Internal Control gRPC contract and the authoritative persistence](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [The Controller control listener allows HTTP/2 keepalive](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260924-control-listener-keepalive.md)
- [Internal Control gRPC adds the read-only ListLiveSandboxes](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-list-live-sandboxes.md)
- [Internal Control gRPC adds Workspace operations and Controller-reported Nodes](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md)
- [Runtime control is authorized by Cloud, delivered by the Controller, and fenced at the Node's real entry points](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
- [Internal Control gRPC adds plugin, Agent session and Revision delivery executions](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260928-agent-run-executions-thread-and-upload-grants.md)
- [Session and delivery inputs gain a prior Revision, plus `GrantRevisionDownload`](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261010-revision-restore-contract.md)
- [Agent declarations travel with the `install_plugins` input (the install-input part of the Agent configuration metadata decision)](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261017-agent-configuration-metadata.md)
