# cmd/demo-issue-board: CLI Issue board demo

[中文](README.md) | [English](README.en.md)

A development-only, one-shot demo. It creates, moves and lists Issues through real HTTP, PostgreSQL and `router.New`, then reads the row count with a fresh SQL query and prints board and persistence summaries.

## Running and data

Run `go run ./cmd/demo-issue-board` from the repository root. `DEMO_DATABASE_URL` selects a dedicated PostgreSQL database; the default is the local `ora` database at `127.0.0.1:55432` (connection details are in `main.go`). The account needs schema creation and deletion privileges.

Each run creates a `demo_issues_<UUID>` schema, applies embedded migrations, bootstraps Alice's demo tenant and calls the API using two JWTs generated in memory. Deferred cleanup drops the schema on normal return; call-failure branches using `os.Exit` bypass cleanup, so inspect leftover schemas manually.

## Boundaries

There is no external login or production Gateway; demo keys are not persisted. This command does not change the production server's migration gate. See [Issue tests](../../integration/issues_test.go) and [Issue board semantics](../../docs/collaboration/issues.en.md).
