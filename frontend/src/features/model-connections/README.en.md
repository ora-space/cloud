# model-connections: personal model connections

## Responsibility

The signed-in user's private multi-connection, multi-model settings and default selection. The server owns metadata, versions and defaults. Credentials use only Gateway write/clear endpoints; there is no secret readback or Pi configuration import. Live model switching and thinking levels are outside this module.

## Files

| File | Purpose |
| --- | --- |
| `api.ts` | Generated-client adapter, complete pagination, versioned metadata/defaults and a credential boundary that discards raw HTTP errors |
| `fields.tsx` | Labelled metadata inputs and bounded choices |
| `connection-form.tsx` | Metadata draft for two protocols, authentication modes and removable model rows |
| `credential-form.tsx` | Password input, immediate clearing before submission and safe outcome text |
| `default-form.tsx` | Offers only enabled connections with credentials and their models |
| `model-connections-page.tsx` | List, editing, deletion confirmation and page composition |
| `model-connections-page.test.tsx` | Production generated-client/MSW tests for forms, requests, versions and credential cache exclusion |

## Dependencies and invariants

Depends on generated `api/me`, idempotency helpers from `features/spaces/api`, fault/pagination adapters in `lib`, and UI primitives. Only application routes consume this module; lower layers must not depend on it. TanStack Query owns server state; resource versions remount editing drafts, while refused writes preserve drafts. Slash-containing model identifiers remain unchanged. Secret inputs are never prefilled or persisted in browser storage and never enter TanStack mutation variables. Inputs clear before submission, and HTTP failures retain only safe fault messages, avoiding Axios request configuration containing the original key. Disabled controls never substitute for server authorization.

Credential PUT also requires an idempotency key. The shared adapter generates it from safe connection/version metadata without retaining the secret as idempotency variables.

## Testing

MSW intercepts real `/api/v1/me/model-*` routes. Secrets are invalid synthetic values generated during each test, never real credentials. Tests cover models/protocols, defaults, optimistic versions, idempotent deletion, pagination, failed loading, and empty password inputs plus secret-free mutation caches while a request is running.
