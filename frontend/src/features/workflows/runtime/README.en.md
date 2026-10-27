# Workflow runtime definitions

Owns framework-free graph definitions, variable catalogs, geometry and pure editing policy.
It neither sends HTTP requests nor stores server state.

| Files | Responsibility |
|---|---|
| `types.ts`, `definition.ts`, `graph-codec.ts`, `graph-import.ts` | Contracts, structural validation and field-preserving codecs |
| `connection-validation.ts` | Endpoint/port deduplication, scope gate (no edge out of loopExit, none across containers, iteration entry only into its region) and stable edge ids |
| `condition-cases.ts` | Compatible branch configuration and canonical wire operators |
| `node-catalog.ts`, `node-factory.ts`, `capabilities.ts` | Authorable nodes, defaults and capability catalogs |
| `variable-*.ts`, `global-variables.ts` | Consumer-visible variables and type derivation |
| `layout.ts`, `node-geometry.ts`, `container-layout.ts`, `iteration-*.ts` | Layout, region membership and pure graph transforms |
| `loop-graph.ts` | Loop member-port append seams, `insertLoopMember`, the frame size (authored widened to measured) and the box members need |
| Other formatting, identity and schema files | Pure run-display derivations |

Depends on basic utilities and static contracts; consumed by editors, run pages and plugin imports.
Distinct branch edges between the same nodes must not overwrite each other. Null and omitted ports
are equivalent; the node-aware draft layer normalizes legacy Condition ELSE connections. Pure tests
cover boundaries and codec tests verify persisted ports and Output bindings across reloads.

Loop catalogs expose named outputs outside and carried values plus container ancestors inside; loopExit declares no output. Import preserves parentId and initial dimensions. Nonempty containerId takes precedence over parentId (`effectiveWorkflowContainer`, the backend's rule). Loop members join only through a member port's append seam, which writes both ownership keys and grows the frame; the loop scope offers no Output (the publish gate refuses it).
