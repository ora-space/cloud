# agents: Agent management entry

Standalone Agent management is unavailable. List and old detail routes explain this and offer real plugin installation and task entries. They never read demo Agents, fabricated online status or assigned tasks.

| File | Responsibility |
| --- | --- |
| `agents-page.tsx` | Management explanation and workspace plugin/task navigation |
| `agent-detail-page.tsx` | Old Agent detail URLs reuse the management explanation |
| `*.test.tsx` | Entries, old links and exclusion of demo records |

Depends on unavailable and lib/paths; consumed by the route composition root. Executable identities come from the real collaboration targets in issues; this module must not construct or manage them.
