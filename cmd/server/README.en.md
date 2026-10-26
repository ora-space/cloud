# cmd/server: Cloud service composition

[中文](README.md) | [English](README.en.md)

The production entrypoint composes configuration, databases, HTTP/gRPC adapters and lifecycle loops around one Store. Core owns business state and policy.

## Startup and serving

- Load YAML / `CLOUD_*` configuration, logging and the PostgreSQL pool. `CheckSchema` rejects missing, unknown or checksum-mismatched migrations without running DDL.
- Read deployment trust material and construct the Authenticator, object store and external-service clients.
- `router.New` serves public HTTP; `controlgrpc.NewSecure` serves mutually authenticated Controller gRPC. Retired management JSON paths stay retired.
- `configureCollaboration` composes real Space Agent and Workflow directories in production; collab fixtures require the explicit development switch.
- `wireRepository` connects Git ref queries, Credential Store and the Repo Executor worker according to configuration; network I/O stays outside Core transactions.

## Background tasks and shutdown

`main.go` owns loops with one cancellation context and WaitGroup: optional catalog sync and workflow asset imports, Agent dispatch/startup, blocked windows, Thread end/cancellation, delivery/release recovery and Workflow scheduling. Core methods still decide state; loops do not make process-local state authoritative.

The residual-ledger recovery loop calls `ReconcileAgentRunResidualsOnce` immediately at startup and then at `issue_runs.residual_reconcile_interval` (default `1m`), sharing that cancellation context and WaitGroup. It retains recovery responsibility after the business run ends and consumes existing fact entrypoints without adding a Controller discovery RPC or contract. Missing evidence leaves the ledger open; timeouts, reconnection and forced stops do not establish successful cleanup.

Cleanup differs between shutdown and failure paths:

- Signal-exit branch (SIGINT/SIGTERM): cancel background tasks and send the Controller drain signal; stop gRPC gracefully, forcing it to stop at the shared 10-second shutdown deadline, then call HTTP Shutdown with that same deadline, join background loops, close the database and flush logs.
- Serving-failure branch: cancel background tasks and join the loops before returning, then close the database and flush logs. This branch does not call HTTP Shutdown or gRPC Stop/GracefulStop and does not guarantee that the other service's in-flight requests drain.
- Startup failure: after background loops have started, errors from `wireRepository`, `net.Listen`, `Control.TLS` or `NewSecure` return directly without joining the loops. Registered defers run in reverse order, closing the database before cancelling the context; cleanup of already-started tasks is not guaranteed.

See [Deployment](../../docs/operations/deployment.en.md) and [Configuration](../../docs/operations/configuration.en.md) for operations, and [IssueRun](../../docs/agent-runs/issue-runs.en.md), [Workflows](../../docs/workflows/workflows.en.md) and [External Git line](../../docs/repositories/external-git.en.md) for execution semantics. Local tests verify composition boundaries.
