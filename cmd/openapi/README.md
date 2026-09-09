# cmd/openapi: OpenAPI Document Generator

`cmd/openapi` is a code-generation and synchronization tool that outputs the canonical OpenAPI 3.0 specification from Go contract definitions.

## Responsibilities

- **Contract compilation**: Calls `internal/contract.Document()`, which constructs the authoritative OpenAPI 3.0 document representing all 19 public endpoints, 15 internal control endpoints, and the health check endpoint.
- **Artifact synchronization**: Serializes the document into indented JSON and writes it to `api/openapi.json`.
- **Single Source of Truth (SSOT)**: Guarantees that `api/openapi.json`, `router.Routes()`, and `internal/contract` stay strictly aligned.

## Invariants

- **No hand edits**: `api/openapi.json` must never be modified manually. All route, request body, query parameter, or status code changes must be made in `internal/contract` and `internal/api/router`, then regenerated using:
  ```sh
  task openapi
  ```
- **CI verification**: CI enforces that the committed `api/openapi.json` matches the output of `cmd/openapi` exactly, failing if there is any uncommitted schema drift.

See [cmd overview](../README.md), [Contract package](../../internal/contract/README.md), and [HTTP router](../../internal/api/router/README.md).
