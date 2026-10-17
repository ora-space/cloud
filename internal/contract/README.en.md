# internal/contract: API Contract & OpenAPI Specification

[中文](README.md) | [English](README.en.md)

`internal/contract` programmatically defines the authoritative OpenAPI 3.0 data models and schemas for Ora Cloud. It serves as the single source of truth for all API requests, responses, and fault definitions.

## Responsibilities

- **Programmatic OpenAPI generation**: `contract.Document()` builds the complete OpenAPI 3.0 specification tree, defining metadata, security schemes (HTTP Bearer JWT), parameters, request bodies, status codes, and response schemas.
- **Component schema modeling**: Defines strict JSON schemas for domain entities:
  - Core resources: `Tenant`, `TenantMember`, `User`, `Project`, `Workspace`, `Task`, `Operation`, `Effect`, `WorkspaceNode`, `Ticket`.
  - Clone step: `CloneInput` (`{kind, repositoryUrl, snapshotRef, baseCommit, workspaceRef}`, the internal control snapshot's `cloneInput` and `OperationRequest.clone`) and `CloneExecution`.
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

## Agent session and archive contract

`AgentSessionSummary` is the item returned by `GET /api/v1/tenants/:tid/agent-sessions`: Run/Issue identity,
Issue title/number, nullable project/plugin metadata, run/Thread state, timestamps, a nullable model
summary and `historyAvailable`. That flag denotes an unexpired archive registration, not a successful
storage read. This version discovers Agent IssueRuns only.

List filters are `projectId`, `status`, `agent`, `from`, `to`, `after` and `limit`. Creation times use
`[from,to)`, status is one enum value and Agent is an exact plugin identifier. Page size defaults to
50 and caps at 100. Ordering is descending `(createdAt,id)`; `nextCursor` is an exclusive UUID cursor,
distinct from the Thread's integer `seq` cursor. Unknown, empty and repeated query values are rejected.

`SessionHistory` belongs to `GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history`, with
`source="archive"`, `records`, `sizeBytes` and `sha256`. Each record is a JSON object retaining unknown
fields, original numeric values and file order; provider/history schemas are not hard-coded into the
Cloud contract. Consumers replay subsequent updates to the same history `seq`; Cloud Thread paging
`seq` must not replace the history sequence.

The history route accepts no query parameters, path or URL. Existing two-tier credentials and current
member/Issue/Run authorization apply, rechecked with the registration after storage I/O. Limits are
32 MiB / 100000 records. Safe errors are `404 history_unavailable` for absent/expired registration,
`413 history_too_large`, `502 history_invalid` or `502 history_unavailable`, without private storage
details. Model, tool timing, token/cost and parent-child data depend on persisted evidence; missing
values must not be invented. Update schemas, routes, generated OpenAPI and frontend clients together;
real conversation logs must never become contract test fixtures.

## Model request metrics contract

`session_statistics.go` defines `MetricAggregate`, `ModelAttemptUsage`, `SessionModelAttempt` and
`SessionModelStatistics`. `/statistics` takes no query; `/model-calls` accepts only `limit/after`. Scope is
`recorded_http_attempts` from `model_gateway`, not historical backfill or billing, independent of page size.
Input/output/total/cache read/cache write/reasoning counters remain independently nullable with coverage.
OpenAI input includes cache reads; Anthropic input excludes cache reads/writes; reasoning is an output subset.
Do not add these fields indiscriminately. `firstOutputMs` observes gateway first effective output, not model TTFT.
A `started` observation does not guarantee the request is still alive.
