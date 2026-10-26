# internal/api/router: HTTP route adapter

[中文](README.md) | [English](README.en.md)

`Routes()` explicitly lists routes, actions and permitted request fields. `New` builds the production Gin engine; `NewDevelopment` additionally enables retired management JSON entrypoints used by simulators. Production returns `410 retired_management_transport` for retired `/internal/v1/...` routes; Controllers use the [gRPC service](../../controlgrpc/README.en.md).

## Package processing

- Establish `X-Request-Id` and logging context. Health checks read database readiness and the object-store and Git Server configuration, without probing S3 or the Git Server.
- Verify service/user credentials and caller binding; validate public fields using `Route.Fields` and `validField`.
- The default body cap is 64 KiB. `ThreadMessagePath` and `TaskConversationMessagesPath` have a separate 256 KiB cap to contain a legal message's JSON envelope; core still limits decoded text to 64 KiB.
- The JSON decoder uses `UseNumber` and rejects unknown fields, extra JSON values and invalid types; query parsing follows each read entrypoint's constraints.
- Ordinary requests go to `Store.Public`; archives, statistics and external-service checks use dedicated Store methods. Development management calls go to `Store.Control`.
- `core.ErrorCode` maps domain errors to safe Faults; unregistered paths return `404 not_found`.

Task conversation adds `GET/POST /issues/:iid/conversation`, explicitly addressed Agent messages, pending-message cancel/retry, saved-result resume and explicit undelivered-result discard under the same member authentication and idempotency rules. URL `:mid` only identifies a pending message; callers never choose author or state. `POST .../conversation/stop` returns 202 and atomically requests cancellation plus a queue pause. Reads identify the current execution, earliest unresolved barrier, manual pause and `canStop`. Historical selection never changes the stop target; explicit recovery must address the earliest barrier.

`thread_preview.go` serves run-scoped snapshot SSE. Every send and periodic heartbeat rechecks credentials and membership; disconnect only cancels the subscription. Slow browsers consume the latest snapshot without blocking Agent or durable records.

Recovery requests strictly accept a JSON boolean for `acknowledgeLostWork`. The domain rejects `false`; HTTP rejects strings, numbers and null. Real HTTP tests cover acknowledgement, type refusals and same-key replay.

`tianzhou.go` adapts the deployment directory query; Core still owns membership authorization. Local tests cover validation and transport boundaries; [integration](../../../integration/README.en.md) covers real database/API contracts.

## Contract entrypoints

[OpenAPI](../../../api/openapi.json) owns fields and status codes; see [contract](../../contract/README.en.md) for generation. Cross-module semantics live in [HTTP API](../../../docs/core/http-api.en.md), [Session reads](../../../docs/agent-runs/session-reads.en.md) and [Personal model access](../../../docs/models/model-access.en.md). Gateway/model-gateway handle API Key writes; Cloud rejects credential requests before reading their body and never falls back to ordinary JSON dispatch.

Scheduled-artifact lists and downloads use the session-read boundary: all query parameters are refused, successful responses use `private, no-store`, and callers select only route-bound run identities and indices.

Single-artifact responses are verified raw bytes with a safely encoded attachment filename, binary media type, nosniff and private no-store; errors remain JSON Faults.
