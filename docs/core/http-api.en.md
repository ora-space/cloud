# HTTP API Conventions

English | [中文](http-api.md)

The Cloud server exposes a single HTTP surface: browsers reach `/api/v1/*` through the
[Gateway](../identity/gateway.en.md). This document covers the conventions shared by that surface —
routing, credentials, request decoding, errors, pagination, SSE — and how the contract files are
generated. Fields, status codes and response shapes of individual endpoints are defined by
[`api/openapi.json`](../../api/openapi.json); this document does not list them.

The internal contract used by the Controller is gRPC; see
[Controller integration](../runtime/controller-integration.en.md).

## Route allowlist

`Routes()` in `internal/api/router` is an explicit allowlist: each route declares its method, path
and the request-body fields it accepts. Paths outside the allowlist return `404 not_found`. The same
allowlist drives the OpenAPI coverage test, which keeps the document and the implementation in step.

Routes fall into two groups:

| Group | Paths | Handling |
|---|---|---|
| Public endpoints | `/api/v1/...` | Both credentials are verified, then `Store.Public` runs |
| Retired internal JSON endpoints | `/internal/v1/...` | The production server always returns `410 retired_management_transport`; only the in-process simulator opens them through `router.NewDevelopment` |

Two routes sit outside the allowlist: `GET /healthz` (checks database connectivity and reports
whether object storage is configured, without revealing the configuration) and the SSE stream
`GET /api/v1/tenants/:tid/spaces/:spaceId/events`.

## Credentials

Every public request carries two independently signed internal credentials, minted by the Gateway
for each request (see [Internal credentials and service trust](../identity/authentication.en.md)):

| Header | Content | Requirement |
|---|---|---|
| `Authorization: Bearer <service JWT>` | The calling service's identity | `kind=service`; public endpoints require `role=gateway` |
| `X-Ora-User-Token: <user JWT>` | The end user's identity | `kind=user`, and `caller` must equal the service JWT's `sub` |

A missing or invalid credential returns `401 invalid_service_credential` or
`401 invalid_user_credential` respectively; a wrong service role returns `403 service_forbidden`.
Credentials only prove who is calling. Authorization is always decided by core from current
PostgreSQL state (tenant membership, role, resource ownership); identities and scopes claimed in
headers or bodies are never accepted.

## Request decoding

The body of every non-`GET` request is checked strictly before it reaches core:

- The body is capped at 64 KiB. The route that appends a Thread message allows 256 KiB, so an
  over-long message is refused by the contract as `400 content_too_large` rather than truncated
  into `invalid_json`.
- It must be a single JSON object with no further JSON value after it; `null` is refused. Numbers
  are decoded as `json.Number` and keep their precision.
- Only the fields the route declares are allowed; anything else returns `400 unknown_field`
  (`params.field` names the field). Server-owned identity and scope fields therefore cannot be
  injected through a body.
- Field types are fixed by name: `version`, `position` and similar are integers, `ids` is an array of
  strings, `filter`, `input`, `graph` and similar are objects, `targets`, `content`, `models` and a
  few others have a fixed element shape, and every other field defaults to a string. A mismatch
  returns `400 invalid_field_type`.
- A few boolean fields (such as force-stop's `impactConfirmed`) must be present explicitly; a missing
  one returns `400 missing_field`.

Query parameters are constrained as well. The paging parameter `limit` must be an integer from 1 to
100 (the Thread read allows up to 500), and the session read endpoints accept only the documented
single-valued parameters; unknown, repeated or empty parameters return `400`.

Two routes are refused before their body is read: writing and clearing a personal model
connection's credential belongs exclusively to [model-gateway](../models/model-gateway.en.md), so a
plaintext key cannot enter the Cloud process even if someone bypasses the Gateway and calls Cloud
directly.

## Errors

Every error has the same shape:

```json
{"code": "version_conflict", "params": {}, "requestId": "8c1f…"}
```

- `code` is a stable machine-readable error code that clients localize. `params` holds only safe
  identifiers and enum values (such as the offending field name) — never prose, and never anything
  the caller is not already allowed to read.
- Every response carries an `X-Request-Id` header equal to the error body's `requestId`, for finding
  the request in server logs.
- Internal detail (SQL, stacks, credentials, infrastructure information) never reaches a response.
  Unexpected errors outside core map to `500 internal_error`; details go to structured logs only.

What the common status codes mean:

| Status | Typical codes | Meaning |
|---|---|---|
| 400 | `invalid_json`, `unknown_field`, `invalid_input`, `idempotency_key_required` | The request itself is invalid |
| 401 | `invalid_service_credential`, `invalid_user_credential` | Credentials missing or invalid |
| 403 | `membership_required`, `admin_required`, `user_disabled`, `runtime_use_forbidden` | Valid identity, not allowed to do this |
| 404 | `not_found` | The resource does not exist, or the caller may not learn that it exists |
| 409 | `version_conflict`, `idempotency_conflict`, `operation_in_progress` | Conflicts with current state |
| 410 | `retired_management_transport` | A retired entry point |
| 428 | `version_required` | A required resource version is missing |
| 503 | `database_unavailable`, `directory_unavailable` | A dependency is temporarily unavailable |

Invisible resources return `404` rather than `403` so that "exists but forbidden" is never revealed
to non-members.

## Consistency conventions

These conventions are implemented in core; see
[Authoritative persistence and the transaction model](persistence.en.md#consistency-tools):

- **Idempotency**: `POST` and `DELETE` require an `Idempotency-Key` (at most 200 bytes). The same key
  with the same content replays the original response; the same key with different content returns
  `409 idempotency_conflict`.
- **Resource versions**: modifying requests carry the current `version`; a missing one returns
  `428`, a mismatch `409`.
- **Asynchronous acceptance**: creating a Project, Workspace lifecycle actions, retrying an
  operation, force-stop and similar requests return `202` with a durable operation in the response.
  Clients follow progress by reading the resource or the operation and do not wait for external
  execution to finish.

## Pagination

List endpoints return:

```json
{"items": [...], "nextCursor": "<uuid or empty string>"}
```

- The default order is ascending resource UUID. `after` is an exclusive cursor (the previous page's
  `nextCursor`); `limit` defaults to 50 with a maximum of 100. An empty `nextCursor` means there is no
  next page.
- Identity and scope filters apply before paging, so a cursor never walks past rows the caller
  cannot see.
- A few endpoints define their own order; OpenAPI is authoritative: `GET /api/v1/me/tenants` is in
  ascending tenant creation time, the session list is in descending `(createdAt, id)`, and the Thread
  uses an integer `seq` cursor.

## SSE invalidation stream

`GET /api/v1/tenants/:tid/spaces/:spaceId/events` is a Space-level server-sent event stream
(`text/event-stream`). It carries **invalidation notices** only, never business state; on an event
the client re-reads the affected resources over REST. REST is the authority; SSE only says "time to
refresh".

- **Authorization is the REST one**: opening the stream checks credentials and membership along the
  `GET /spaces/:spaceId` path. Credentials and membership are checked **again** before every event is
  written, so once a member is disabled not even the "members changed" event reaches them.
- **Connections expire**: the re-check uses the request headers the connection was opened with, and
  a user credential lives at most five minutes. After it expires, the first event that arrives makes
  the server close the connection, and the client has to reconnect.
- **Delivery is not guaranteed**: events are emitted after commit, at most once, with no replay. A
  slow subscriber loses events, and only subscribers connected to the same Cloud process receive
  them. Clients must not derive state from events and should refresh after reconnecting.

An event looks like `data: {"type":"project.created","spaceId":"…","projectId":"…"}`. The types are:

| Type | Trigger |
|---|---|
| `space.updated`, `space.member_updated` | Space details or members changed |
| `project.created`, `project.updated`, `project.archived` | A Project was created, renamed, or entered deletion |
| `space.plugins_updated`, `plugins.catalog_updated` | The space's plugin selection or install progress changed; the plugin catalog refreshed (sent to every subscriber) |
| `issue_run.thread_appended`, `issue_run.thread_changed` | A Thread gained content or its REST representation changed; `lastSeq` is only a high-water hint, never a cursor |

## How the contract files are generated

The public HTTP contract has one source, and four places must stay in step:

```text
internal/api/router  Routes()         route allowlist and request fields
internal/contract    Document()       the OpenAPI 3.0 model (Go code)
        │  task openapi (go run ./cmd/openapi)
        ▼
api/openapi.json                      generated; never edited by hand
        │  task frontend:generate (orval)
        ▼
frontend/src/api                      the frontend's typed client; generated; never edited by hand
```

- When a route, field, status code or response changes, edit the Go sources, run
  `task frontend:generate` (which runs `task openapi` first), and commit both generated artifacts
  with the source change.
- `TestPublishedOpenAPIIsValidAndCurrent` requires the committed `api/openapi.json` to match
  `Document()` byte for byte and to pass structural validation. Integration tests validate real HTTP
  responses against the same document, so any undeclared response field fails. The CI frontend job
  checks the generated client for drift.
- The OpenAPI document must pass strict validators: an object schema emits `required` only when it
  is non-empty (`TestRequiredIsOmittedWhenEmpty`).

## Related decisions

- [Space API and a Space-level invalidation stream: REST is authoritative, SSE only invalidates](https://github.com/ora-space/specs/blob/main/decisions/cloud/api-boundary/0-space-api-and-event-stream.md)
- [External identity is normalized by the Gateway; Cloud keeps authorization authority](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
