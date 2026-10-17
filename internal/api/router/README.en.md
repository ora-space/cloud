# internal/api/router: HTTP Router & Transport Adapter

[中文](README.md) | [English](README.en.md)

`internal/api/router` establishes the HTTP presentation boundary for Ora Cloud. Built on top of Gin, it binds HTTP routes, verifies two-tier JWT authentication, enforces strict request body parsing limits, normalizes errors, and dispatches requests to `internal/core`.

## Responsibilities

### Route allowlist and dispatch
- `Routes()` declares the explicit allowlist of supported endpoints:
  - **Public API (`/api/v1/...`)**: users, tenants, memberships, collaboration spaces and their members, projects, workspaces, operations, status queries, Issue collaboration (issues, statuses, labels, views, comments, subscribers, runs, context refs, interactions and collaboration targets), workspace plugins (`/tenants/:tid/spaces/:spaceId/plugins`: catalog snapshot, selection list, install, remove), Agent session discovery and complete archive reads.
  - **Internal Control API (`/internal/v1/...`)**: 15 endpoints for controller leasing, operation claiming/advancing, node registration, and ticket admissions.
  - **Health check (`/healthz`)**: Verifies database reachability via `store.Pool.PingContext`; adds configured/unconfigured in `dependencies.objectStore` and `dependencies.gitServer` without probing S3 or the Git Server or exposing endpoints/credentials.
- Any unregistered endpoint is caught by `r.NoRoute` and returns `404 not_found`.

### Two-tier authentication
- **Service credential**: Read from the standard `Authorization: Bearer <token>` header. Must be a valid JWT signed by an authorized key with `kind="service"`.
- **Gateway authorization**: For public endpoints, the service token must possess the `role="gateway"`.
- **User credential**: Read from `X-Ora-User-Token: Bearer <token>`. Must be signed with `kind="user"`.
- **Caller-subject binding**: Enforces that `user.Caller == service.Subject` to prevent credential impersonation across gateways.

### Strict request validation and decoding
- **Payload size bound**: Enforces a strict 64 KiB ceiling on incoming request bodies via `http.MaxBytesReader`.
- **Strict JSON hygiene**: Uses `json.Decoder` with `UseNumber()`. Trailing bytes or extraneous JSON values are rejected with `400 invalid_json`.
- **Disallowed fields**: Only fields explicitly listed in `Route.Fields` are permitted in the JSON body. Unknown properties immediately fail with `400 unknown_field`.
- **Type validation**: Field types are rigorously checked (`validField`), ensuring timestamps, UUIDs, integers, and boolean properties conform to expected schemas before reaching the domain core.

### Fault projection and correlation
- **Request correlation**: Generates a unique UUID `X-Request-Id` for every incoming HTTP request, attaching it to the request context, response header, and structured log events.
- **Error normalization**: Traps panics and domain errors via `core.ErrorCode(err)`. Translates `*core.Fault` into structured JSON responses (`code`, `params`, `requestId`) with appropriate HTTP status codes. Internal database or system errors are masked as `500 internal_error` without disclosing internal infrastructure details.

## Boundaries and invariants

- **No business state**: The router owns no business logic, domain state transitions, or database connections. It delegates entirely to `core.Store.Public` and `core.Store.Control`.
- **Contract synchronization**: Route paths, allowed fields, and HTTP methods must stay synchronized with `internal/contract` and `api/openapi.json`.

See [api overview](../README.en.md), [Core domain](../../core/README.en.md), [OpenAPI contract](../../contract/README.en.md), and [Authentication](../../../docs/authentication.md).

## Personal model routes

`/me/model-connections` and `/me/model-default` use verified user scope and strict model/version
validation. Credential routes exist in the published contract; Cloud rejects them before reading
the body, while Gateway routes directly to model-gateway. Thread exposes the initiator, model summary
and append/end capabilities. See [model connections](../../../docs/model-connections.en.md).

## Agent session Dashboard routes

- `GET /api/v1/tenants/:tid/agent-sessions` accepts `projectId`, `status`, `agent`, `from`, `to`,
  `after` and `limit`. Unknown, repeated or empty query parameters return `400 invalid_session_filter`;
  Core also validates UUIDs, one exact status and RFC 3339 date bounds. Pages default to 50 and cap
  at 100, ordered by descending `(createdAt,id)`. `after` is a preceding page's UUID cursor within
  the current tenant; creation-time filters use `[from,to)`.
- `GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` accepts no query parameters, object key
  or URL. Its response is `{source:"archive",records:[...],sizeBytes,sha256}`, without a storage URL.

Both routes require a gateway service JWT and a user JWT bound to that service. Core checks current
user/tenant membership; archive reads also check Issue/Run scope and reauthorize after external I/O.
Neither endpoint provisions identities, sessions or business state. Browsers use the normal Gateway
session and receive no S3/internal credentials. Session responses use `Cache-Control: no-store`.

Archives cap at 32 MiB and 100000 valid JSONL objects, verified against actual size and SHA-256.
Absent/expired registration returns `404 history_unavailable`; overflow returns
`413 history_too_large`; invalid content returns `502 history_invalid`; storage failure returns
`502 history_unavailable`. Identity/membership and invisible Issue/Run failures retain existing safe
errors. Records retain original fields, numeric values and file order. Discovery covers Cloud Agent
IssueRuns only, does not infer missing token/cost/parent-child data and never scans personal files.
Live projections continue to use the existing Thread API and space SSE invalidation hints.

Model statistics also expose `.../runs/:rid/statistics` (no query parameters) and `.../runs/:rid/model-calls`
(only nonempty singleton `limit/after`). Both require current membership and Agent-run scope, use `private, no-store`,
and do not depend on archives. Calls page ascending `(startedAt,id)`, default 50, maximum 100, with a same-run cursor.
Statistics cover the run's recorded HTTP attempts; missing metrics stay null with explicit coverage. No content, grants
or credentials are returned.
