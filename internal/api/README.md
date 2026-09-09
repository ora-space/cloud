# internal/api: HTTP Presentation Layer

`internal/api` contains the HTTP translation and routing components of Ora Cloud. It translates inbound HTTP requests into domain requests for `internal/core`, enforces transport security and payload size bounds, and formats responses and errors according to the OpenAPI contract.

## Module map

- [router](router/README.md) configures the Gin HTTP engine, registers routes, verifies caller credentials, parses request bodies with strict limits, and maps domain errors to public fault contracts.

## Responsibilities and boundaries

- **Transport translation only**: This layer is purely an adapter between HTTP and the core domain. It contains no business logic, state machines, or SQL queries.
- **Strict input validation**: Decodes JSON bodies with 64KB size limits and rejects requests with unknown or malformed fields before invoking domain methods.
- **Contract conformity**: All routes, query parameters, request bodies, and response codes adhere strictly to the specification defined in `internal/contract` and `api/openapi.json`.
- **Database isolation**: Handlers in this layer never touch database handles or GORM instances directly; all database interaction is mediated by `core.Store`.

See [HTTP router](router/README.md), [Contract package](../contract/README.md), and [API specification](../../api/openapi.json).
