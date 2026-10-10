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
| `executions.proto` | `ExecutionService`: claim work, register before dispatch, take over Node events, store queried results, recovery reads; execution kinds are clone, plugin install/remove, Agent session and Revision delivery, and session and delivery work items carry their target Node (`WorkTarget`) |
| `plugin_executions.proto` | Input of plugin install/remove executions (downloads and SHA-256 assembled by Cloud from its catalog snapshot) and per-plugin results |
| `agent_executions.proto` | Input and results of Agent session and Revision delivery executions, the git identity, user turns, and the memory-only upload grant `UploadGrant` |
| `agent_runs.proto` | `AgentRunService`: ordered batch takeover of Thread events, claiming and recording Thread commands, issuing Revision upload grants |
| `operations.proto` | `WorkspaceOperationService`: claim, plan effects, record effect results, advance and defer Workspace lifecycle operations |
| `nodes.proto` | `NodeReportService`: the Controller registers the desktop Nodes it holds sessions with (`node_id` + `node_incarnation_id`) and reports their status, end and idle |
| `signals.proto` | `ControlSignalService.Watch`: a Controller-opened server stream carrying `WorkAvailable` / `OperationAvailable` / `ThreadCommandAvailable` / `Drain` / `NodeAssignment` |

## Semantics

- **Submission identity**: state-changing requests carry a caller-generated `submission_id`. The same
  identity with identical content returns the original response without reapplying; the same identity
  with different content fails with `ABORTED` + `CONFLICT`. A lost reply is retried with the same
  identity, never a new one.
- **Boundary order**: only after `RecordDispatch` succeeds may a Controller dispatch to a Node; only after
  `TakeOverNodeEvent` succeeds may it acknowledge that event sequence; `RecordQueriedResult` never
  authorizes an acknowledgement.
- **Signal stream**: at-most-once, not persisted, changes no ownership; after a stream loss the Controller
  falls back to periodic `ClaimWork`, `ClaimOperation` and `ClaimThreadCommands`.
- **Upload grants are not input**: an `UploadGrant` is a short-lived bearer credential; it never enters an
  execution input, a registration or any log. Execution input carries object keys only.
- **Authorization source**: Cloud/PostgreSQL decides business authorization. Runtime control carries confirmed tenant, actor, session and target scope; execution endpoints never expand it from caller-asserted identity.

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

## Approved runtime control contract

RuntimeControlService carries Cloud-decided target bindings, closure acknowledgement, fresh dispatch permits and independent force-stop plans. Tenant/workspace, actor/server session and control epoch bind trusted execution scope; they do not authorize caller-asserted membership. User control epoch, Controller lease epoch, runtime generation, Node incarnation, execution ID and Node operation ID remain distinct. Components without runtime_control capability are refused. Idempotent history cannot revive eligibility: Controller refreshes before dispatch and Node checks acceptance and first execution. Cloud–Controller uses mutual TLS. New file/terminal/plugin/Agent execution entry points stay closed until equally protected. Authority: specs/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md.

## Personal-model execution

`AgentSessionSpec.model_binding_id` is a Cloud-frozen configuration reference containing no key
or temporary token. `RegisterNodeRequest.model_proxy` and `NodeRecord.model_proxy` preserve the
handshake capability; older Nodes default to false. Both dispatch and authorization require this
capability. See [model connections](../docs/model-connections.en.md).
