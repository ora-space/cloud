# Workflow runtime definitions

Owns framework-free graph definitions, variable catalogs, geometry and pure editing policy.
It neither sends HTTP requests nor stores server state.

| Files | Responsibility |
|---|---|
| `types.ts`, `definition.ts`, `graph-codec.ts`, `graph-import.ts` | Contracts, structural validation and field-preserving codecs |
| `connection-validation.ts` | Endpoint/port deduplication and stable edge ids; preserve ordinary unhandled edge ids |
| `condition-cases.ts` | Compatible branch configuration and canonical wire operators |
| `node-catalog.ts`, `node-factory.ts`, `capabilities.ts` | Authorable nodes, defaults and capability catalogs |
| `variable-*.ts`, `global-variables.ts` | Consumer-visible variables and type derivation |
| `layout.ts`, `node-geometry.ts`, `container-layout.ts`, `iteration-*.ts` | Layout, region membership and pure graph transforms |
| Other formatting, identity and schema files | Pure run-display derivations |

Depends on basic utilities and static contracts; consumed by editors, run pages and plugin imports.
Distinct branch edges between the same nodes must not overwrite each other. Null and omitted ports
are equivalent; the node-aware draft layer normalizes legacy Condition ELSE connections. Pure tests
cover boundaries and codec tests verify persisted ports and Output bindings across reloads.

Loop catalogs expose named outputs outside and carried values plus container ancestors inside; loopExit declares no output. Import preserves parentId and initial dimensions. Nonempty containerId takes precedence over parentId.
