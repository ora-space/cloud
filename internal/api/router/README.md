# internal/api/router: HTTP Router & Transport Adapter

`internal/api/router` establishes the HTTP presentation boundary for Ora Cloud. Built on top of Gin, it binds HTTP routes, verifies two-tier JWT authentication, enforces strict request body parsing limits, normalizes errors, and dispatches requests to `internal/core`.

## Responsibilities

### Route allowlist and dispatch
- `Routes()` declares the explicit allowlist of supported endpoints:
  - **Public API (`/api/v1/...`)**: 19 endpoints for users, tenants, memberships, projects, workspaces, operations, and status queries.
  - **Internal Control API (`/internal/v1/...`)**: 15 endpoints for controller leasing, operation claiming/advancing, node registration, and ticket admissions.
  - **Health check (`/healthz`)**: Verifies database reachability via `store.Pool.PingContext`.
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

See [api overview](../README.md), [Core domain](../../core/README.md), [OpenAPI contract](../../contract/README.md), and [Authentication](../../../docs/authentication.md).
