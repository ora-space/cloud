# internal/logger: Structured Logging Subsystem

`internal/logger` provides process-wide structured logging for Ora Cloud, wrapping Uber Zap and Lumberjack.

## Responsibilities

- **Dual-sink composition**:
  - **Console sink**: Emits colorized, ISO8601 human-readable log lines to `stdout` for local development and container console output.
  - **File sink**: Emits machine-readable JSON log events with log rotation managed by Lumberjack (configurable `max_size`, `max_backups`, `max_age`, and gzip compression).
- **Standardized event fields**: Includes ISO8601 timestamps, log levels, short caller locations, error stack traces, and request correlation IDs (`requestId`).
- **Platform-safe buffer flushing**: `Sync(log)` cleanly flushes buffered entries on shutdown, explicitly handling and suppressing the Windows console handle `EINVAL` error that occurs when syncing console outputs.

## Boundaries and invariants

- **No secret leakage**: Callers must never pass raw credentials, passwords, auth tokens, or unredacted SQL queries to logger calls.
- **Explicit ownership**: Loggers are constructed in command entrypoints and passed explicitly to HTTP routers and middleware; there are no hidden package-global logger singletons.

See [Logger config](../config/README.md) and [cmd/server](../../cmd/server/README.md).
