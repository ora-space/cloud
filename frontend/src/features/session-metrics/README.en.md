# Model request statistics

Presents HTTP attempts recorded by Cloud Gateway and usage reported by upstream providers, independently of session archives and the existing three-lane timeline. It performs no network access, host-log reading, token estimation or cost calculation.

| File | Responsibility |
| --- | --- |
| `index.ts` / `session-metrics.tsx` | Public presentation entry, historical-session empty state and controlled pagination. |
| `metric-summary.tsx` | All recorded attempt outcomes, independent counters, timing and coverage. |
| `metric-values.ts` | Field labels, accounting conventions and unknown/zero-safe formatting. |
| `request-trend.tsx` | One counter per request on the current page; solid adjacent observations and explained dashed connections across missing data. |
| `request-table.tsx` | Request identity, protocol, outcome, terminal observation, timing and reported counters. |
| `session-metrics.test.tsx` | Synthetic coverage, unknown/zero, trend, pagination and empty-state tests. |

Depends only on generated API types and UI primitives. The `sessions` module owns authorization, requests, errors and pagination state. Summary metrics cover all recorded attempts in the run; the chart and table cover the current page only. Retries count separately; text records never imply request counts. Cache counters are not added to input, reasoning is not added to output, and upstream total tokens remain independent. Gateway timing is not model-internal compute time; first meaningful output excludes keepalives and empty chunks. Unknown measurements display as unknown, real zero remains zero, and partial aggregates include only known observations. No recorded attempts does not mean no historical consumption. Future agent-reported usage must remain a separate source.

Tests exercise the public component through real form interactions and synthetic values, without copying real request or personal session content.

The trend retains every request's horizontal position and page-relative ordinal. Dots represent reported values only; real zero sits on the numeric baseline. Missing values use `×` marks in a separate strip below the value axis with the existing request outcome in their tooltip, never zero-filled points. Dashed segments connect known endpoints and report the number of intervening missing observations; they do not interpolate, accumulate, or extrapolate over unknown page edges. An entirely unknown page retains request positions and missing marks without inventing a numeric scale. Regressions cover consecutive and edge gaps, zero, isolated observations, entirely unknown pages, field selection and page ordinals.

`started` means a completion observation is pending, not proof that the request is still active: it may be running or may have ended without a captured completion. Even when a counter is known for every request, server-provided `partial` availability remains partial or nonterminal snapshot data.
