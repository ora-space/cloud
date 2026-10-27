# Workflow editor

Owns the canvas, inspectors and editable draft. Runtime owns policy and variable catalogs;
workflow boundary adapters own HTTP.

| Files | Responsibility |
|---|---|
| `workflow-node-card.tsx`, `workflow-node-source-handles.tsx` | Cards and IF/ELIF/ELSE ports registered by case id |
| `use-workflow-draft.ts` | Sole draft writer, edge identity, removed-case cleanup, saves and history restoration |
| `workflow-inspector.tsx`, `workflow-*-fields.tsx` | Compose per-kind inspectors; Output edits named result bindings |
| `workflow-variable-select.tsx` | Display/select consumer-visible variables without rewriting persisted selectors |
| `loop-node-frame.tsx` | Loop container frame: outer ports, round ceiling, delete and resize; the member-port append menu lives in the port component |
| `workflow-canvas.tsx`, `iteration-*.tsx` | React Flow composition and iteration-region interactions |
| Other `workflow-*` files | Editor page, history, import, publication and launch actions |

Depends on runtime, UI components, i18n and workflow API adapters; workflow pages consume this module.
Case ids and connection ports persist together; different ports may join one target. Output retains
authored bindings and incomplete draft rows, leaving publication validation to reject invalid data.
Tests exercise real React Flow ports, controlled inspectors and draft save/reload behavior; network
tests replace the shared test boundary. Leaving the editor waits for the draft write; after a failure the author
chooses to stay or discard changes and leave, cancelling pending draft state on discard.

Legacy empty condition ports project to ELSE on the canvas without mutating drafts. Loop initial selectors list outer variables; round-end selectors also list body roots and carried values. New members immediately use their parent and relative coordinates; deleting a Loop removes its whole body and edges in one undoable step without a confirmation, and outer nodes dropped onto the frame return. The frame draws from its authored size and what its members need (never its measured size, so it shrinks with its body); the inspector's early-exit action appends only after a member that does round work and is unavailable with just Start and Conditions.
