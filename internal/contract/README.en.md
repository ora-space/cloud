# internal/contract: API Contract & OpenAPI Specification

[中文](README.md) | [English](README.en.md)

`internal/contract` programmatically defines the authoritative OpenAPI 3.0 data models and schemas for Ora Cloud. It serves as the single source of truth for all API requests, responses, and fault definitions.

## Responsibilities

- **Programmatic OpenAPI generation**: `contract.Document()` builds the complete OpenAPI 3.0 specification tree, defining metadata, security schemes (HTTP Bearer JWT), parameters, request bodies, status codes, and response schemas.
- **Component schema modeling**: Defines strict JSON schemas for domain entities:
  - Core resources: `Tenant`, `TenantMember`, `User`, `Project`, `Workspace`, `Task`, `Operation`, `Effect`, `WorkspaceNode`, `Ticket`.
  - Clone work items: `CloneOperation` and the `kind`-tagged `CloneState` (`pending` / `succeeded{path, commit}` / `failed{reason, retainedPath?}`), field for field the transitional Controller DTO.
  - Error schema: Standardized `Fault` schema with error code, parameter mapping, and request ID.
  - Parameter typing: Strong validation formats including `uuid`, `date-time`, `int64`, and string enumerations.
- **Contract verification tests**:
  - `TestPublishedOpenAPIIsValidAndCurrent`: the committed `api/openapi.json` must be byte-identical to `Document()` and pass kin-openapi's OpenAPI 3.0 structural validation.
  - `TestRequiredIsOmittedWhenEmpty`: walks the raw JSON tree and requires every object schema's `required` to be a non-empty array. kin-openapi tolerates `null` / `[]`; the specification and the frontend generator (orval) do not.
  - `integration/contract_test.go`: integration tests validate real HTTP responses against the same document.

## Invariants and workflow

- **No manual JSON edits**: `api/openapi.json` is generated directly from this package via `cmd/openapi`. Developers modify Go definitions here, run `task openapi`, and commit both the code and the resulting JSON artifact.
- **`required` is omitted when empty**: `object()` only emits `required` when there is at least one required field. OpenAPI 3.0 demands a non-empty array, and strict downstream consumers reject `null` or `[]`.
- **Four-way alignment**: Every API route change requires simultaneous updates to:
  1. `internal/api/router` (`router.Routes()`).
  2. `internal/contract` (`contract.Document()`).
  3. `api/openapi.json` (regenerated via `task openapi`).
  4. `frontend/src/api` (regenerated via `task frontend:generate`; CI fails on drift).

See [OpenAPI JSON artifact](../../api/openapi.json), [HTTP router](../api/router/README.en.md), [cmd/openapi](../../cmd/openapi/README.en.md), and [Web frontend](../../frontend/README.en.md).

## Model and Thread additions

`ModelDefinition`, `ModelConnection` and `ModelDefault` describe personal configuration. API keys
are write-only; Gateway/model-gateway own credential routes and Cloud never reads their bodies.
Thread adds its initiator, nullable model summary and append/end capabilities. Regenerate OpenAPI
and the frontend client from these authoritative Go sources.
