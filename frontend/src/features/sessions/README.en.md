# Session analysis

The workspace entry `/w/:workspaceSlug/sessions` initially reads real Cloud Agent IssueRuns. Details at `/sessions/agent/:issueId/:runId` link back to their issue. Chat, workflow, squad sessions and uploaded logs are outside this release.

| File                      | Responsibility                                                                                     |
| ------------------------- | -------------------------------------------------------------------------------------------------- |
| `session-statistics-summary.tsx` | Aggregate-only read and compact summary for the selected run |
| `api.ts`                  | Generated clients, session/project pagination, archives and manual Thread snapshots.               |
| `metrics-api.ts` | Generated statistics/request clients, scoped caches and controlled cursor pagination. |
| `session-list-page.tsx`   | Newest-first list, loading/empty/error states and detail navigation.                               |
| `session-filters.tsx`     | Project, Agent identifier, status and local datetime filters.                                      |
| `session-detail-page.tsx` | Overview, model requests, original three-lane timeline, tools, records and authorization. |
| `session-metrics-panel.tsx` | Independent metric loading/error/empty states and presentation-module wiring. |
| `session-overview.tsx`    | Loaded-record counts, run state, frozen model and unknown metrics.                                 |
| `presentation.ts`         | Status, date and safe fault presentation.                                                          |
| `sessions.test.tsx`       | HTTP boundary, filters/pagination, archive/snapshot choice, revoked access and links.              |
| `session-metrics-page.test.tsx` | Gateway pagination, lazy loading, archive independence and revoked-cache regressions. |

Requests use generated clients and Gateway cookies. No host filesystem scanning or static validation fixtures are involved. A registered Revision requires verified history; errors never silently downgrade to a supposedly complete Thread. Unarchived runs read the Thread tail and expose manual older-page loading; oversized record payloads may be absent from these snapshots. Model provenance comes from the frozen Thread binding. Tokens, cache usage, costs and model API latency remain unknown in session logs. `session-data` normalizes records; `session-timeline` renders the timeline and full record inspector.

The model-request tab lazily reads a separate statistics contract, rendered by `session-metrics`. Summaries cover all recorded HTTP attempts in the run; request pages contain up to 25 entries in ascending start-time order. No pre-collection history is inferred. Ordinary archive/Thread failures do not block metrics, while any 401/403 hides cached details. Failed statistics or request refreshes never retain a stale successful view. Gateway-reported usage and future agent-reported usage remain separate; known counters do not imply a fully terminated stream.

Filter changes start a separate first-page cache. Date filters include the lower bound and exclude the upper bound, converting local input to UTC. Failed authorization on refresh hides old details. Cloud checks current membership for every request.

`session-statistics-summary.tsx` fetches only statistics for the selected Run, without model-calls pages. It reuses metric formatting and coverage labels; failed refreshes, including revoked access, hide cached values. Unobserved usage remains unknown and costs are not estimated.

The compact definition list groups each metric's name, value and coverage together. The no-observations explanation stays outside the list so assistive technology can associate descriptions with the correct metric.
