# proto: Controller internal contract source

[中文](README.md) | [English](README.en.md)

`ora/cloud/internal/v1/` is the single field source for `ora.cloud.internal.v1`. Cloud serves all RPCs; Controllers dial out. Consumers pin a repository commit and generate clients without copying proto files.

Live previews use a separate `PublishThreadPreview` RPC with bounded execution/version replacements and no Ack. `ThreadEvent.history_sequence` links settled records to preview positions. Controller consumers must update generated clients before negotiating the optional path.

| File | Content |
|---|---|
| `errors.proto` | ErrorDetail / ErrorCode |
| `lease.proto` | ControllerLeaseService |
| `executions.proto` | ExecutionService, claiming/registration/recovery |
| `plugin_executions.proto` | Plugin install/remove inputs and results, including frozen Agent declaration version/digest associations (without static copies) |
| `agent_executions.proto` | Agent sessions (including a scheduled run's verdict and artifact declaration), Revision and scheduled-artifact inputs/results, upload grants |
| `agent_runs.proto` | AgentRunService, Thread events and commands, artifact upload grants |
| `operations.proto` | WorkspaceOperationService |
| `nodes.proto` | NodeReportService, identity and capabilities |
| `runtime_control.proto` | RuntimeControlService, snapshots/acknowledgements/permits/force-stop |
| `signals.proto` | ControlSignalService.Watch |

## Generation and changes

- `buf.yaml` uses STANDARD lint and FILE-level breaking; v1 accepts compatible changes only, with breaking changes in a sibling v2.
- `buf.gen.yaml` pins remote Go/gRPC plugins generating [controlpb](../internal/controlpb/README.en.md); network is needed, local protoc is not.
- After edits run `task proto:lint`, `task proto:breaking` (`PROTO_BASE_REF` defaults to main) and `task proto:generate`; commit definitions and generated files together.
- `task proto:check` regenerates and checks drift. `task check` includes lint and drift; CI additionally checks breaking. Consumers advance their pinned commit and regenerate.

See [Controller contract](../docs/runtime/controller-integration.en.md) for submission identity, dispatch/ack order, stream recovery and credential boundaries; [Runtime control](../docs/runtime/runtime-control.en.md) for permits; and [Model access](../docs/models/model-access.en.md) for bindings. [controlgrpc](../internal/controlgrpc/README.en.md) implements the services.
