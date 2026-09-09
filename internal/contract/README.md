# internal/contract: API Contract & OpenAPI Specification

`internal/contract` programmatically defines the authoritative OpenAPI 3.0 data models and schemas for Ora Cloud. It serves as the single source of truth for all API requests, responses, and fault definitions.

## Responsibilities

- **Programmatic OpenAPI generation**: `contract.Document()` builds the complete OpenAPI 3.0 specification tree, defining metadata, security schemes (HTTP Bearer JWT), parameters, request bodies, status codes, and response schemas.
- **Component schema modeling**: Defines strict JSON schemas for domain entities:
  - Core resources: `Tenant`, `TenantMember`, `User`, `Project`, `Workspace`, `Task`, `Operation`, `Effect`, `WorkspaceNode`, `Ticket`.
  - Error schema: Standardized `Fault` schema with error code, parameter mapping, and request ID.
  - Parameter typing: Strong validation formats including `uuid`, `date-time`, `int64`, and string enumerations.
- **Contract verification tests**:
  - `TestRoutesCovered`: Verifies that every route defined in `router.Routes()` is explicitly represented in the generated OpenAPI paths.
  - `TestDocumentValid`: Validates the structural correctness of the generated OpenAPI JSON against specification rules.

## Invariants and workflow

- **No manual JSON edits**: `api/openapi.json` is generated directly from this package via `cmd/openapi`. Developers modify Go definitions here, run `task openapi`, and commit both the code and the resulting JSON artifact.
- **Three-way alignment**: Every API route change requires simultaneous updates to:
  1. `internal/api/router` (`router.Routes()`).
  2. `internal/contract` (`contract.Document()`).
  3. `api/openapi.json` (regenerated via `task openapi`).

See [OpenAPI JSON artifact](../../api/openapi.json), [HTTP router](../api/router/README.md), and [cmd/openapi](../../cmd/openapi/README.md).
