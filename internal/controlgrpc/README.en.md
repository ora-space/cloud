# internal/controlgrpc: Controller gRPC adapter

[中文](README.md) | [English](README.en.md)

Translates [controlpb](../controlpb/README.en.md) requests into Core calls and Faults into gRPC statuses. Core owns business rules, submission deduplication, leases and recovery state.

| File | Responsibility |
|---|---|
| `server.go` | `NewSecure` / `NewDevelopment`, identity interceptors, service registration and keepalive |
| `lease.go`, `nodes.go`, `operations.go` | Lease, Node report, Workspace operation and effect conversion |
| `executions.go`, `execution_codec.go` | Claiming, registration, recovery and stored shapes for clone, plugins, Agent sessions and Revision delivery |
| `agent_runs.go` | Thread events, commands, upload grants and optional live previews |
| `runtime_control.go` | Control acknowledgements, dispatch permits and force-stop |
| `signals.go` | Forwards ControlHub Watch hints and ends streams on drain |
| `fault.go` | Safe statuses and ErrorDetail classifications without database details |

Production `NewSecure` requires mutual TLS and matches the certificate URI to `x-ora-controller-id`; metadata cannot select another identity. `NewDevelopment` is for in-process fixtures; production has no plaintext fallback. Service identity does not replace user authorization or runtime epochs.

`AgentRunService.PublishThreadPreview` is optional: `Store.PublishThreadPreview` checks the same identity, lease and live execution before updating bounded memory. It bypasses durable submission/receipt processing and never returns a Node Ack. ThreadEvent `history_sequence` is forwarded only as metadata linking settled records to previews.

Watch is not durable and grants no work ownership; slow subscribers may lose hints. Drain sends Drain and ends the stream; shutdown refuses new Watch calls. The listener accepts client PINGs at intervals of at least 5 seconds, including without active streams; it does not initiate PINGs.

See [proto](../../proto/README.en.md) for fields/generation and [Controller contract](../../docs/runtime/controller-integration.en.md), [Runtime control](../../docs/runtime/runtime-control.en.md) and [Model access](../../docs/models/model-access.en.md) for cross-module semantics. Local tests cover conversion, errors, TLS, signals and keepalive; [integration](../../integration/README.en.md) exercises real database paths.
