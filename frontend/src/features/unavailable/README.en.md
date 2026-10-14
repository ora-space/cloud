# unavailable: unavailable product entries

Retains product entries and old links, explains that a feature is unavailable, and offers caller-supplied real workspace routes. It never queries catalogs, generates demo records, or accepts input for an unavailable feature.

| File | Responsibility |
| --- | --- |
| `unavailable-page.tsx` | Shared explanation screen and supported route buttons |
| `unavailable-page.test.tsx` | Explanation, navigation and absence of demo requests |

Depends on layout, ui and React Router; consumed by chat and agents. Callers supply valid current-workspace paths and must not substitute a tenant UUID for a workspace slug.
