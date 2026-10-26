# cmd/demo-issue-board-web: browser board demo

[中文](README.md) | [English](README.en.md)

A development-only, single-file board entrypoint. It embeds this directory's `index.html` and serves the real Cloud HTTP API on the same origin.

## Composition and lifecycle

- `DEMO_DATABASE_URL` selects dedicated PostgreSQL; the default is the local `ora` database at `127.0.0.1:55432` (details in `main.go`).
- Each run creates an isolated `demo_issues_<UUID>` schema, applies migrations and bootstraps Alice's tenant. Normal exit drops the schema.
- `/demo/config` reads tenant, user, statuses and labels from the Store; other non-root paths go to `router.New`, with two JWTs signed server-side for the demo identity.
- `DEMO_WEB_ADDR` defaults to `127.0.0.1:8899`; a bind failure falls back to a random loopback port. Empty or `1` `DEMO_WEB_OPEN` attempts to open the browser via Windows `cmd /c start`.
- Ctrl+C drains HTTP for at most 3 seconds.

Run `go run ./cmd/demo-issue-board-web` from the repository root. The database account needs schema creation/deletion privileges.

## Boundaries

Requests share the demo user; there is no real login, identity verification or production session mechanism. Use only in a controlled local environment. Unlike [ora-web](../ora-web/README.en.md), which retains data, each normal exit clears demo data. See [Issue board](../../docs/collaboration/issues.en.md).
