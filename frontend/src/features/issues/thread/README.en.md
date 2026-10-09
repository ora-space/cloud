# issues/thread: the issue page's Agent session panel

[中文](README.md) | [English](README.en.md)

## Responsibility

The "Agent 会话" column on the issue detail page: it picks the issue's most recent agent run, reads that run's Thread (`/runs/:rid/thread`), pages it in both directions, refreshes it from space events, and offers sending a message and ending the session.

Not owned: creating and listing runs (`useRuns` in `features/issues/api`), the SSE connection itself (`features/spaces/use-space-events`), the issue's other columns.

## Files

| File | Purpose |
| --- | --- |
| `thread-entries.ts` | Pure functions: pick the newest agent run, merge windows by seq, map ThreadEntry to display rows (consecutive agent chunks of one turn merge), Chinese state labels |
| `thread-api.ts` | TanStack Query layer: `threadQueryKey`, `useThread` (tail first read + incremental reads + 404 waiting + fallback poll), `useLoadOlderThread`, `useSendThreadMessage`, `useEndThread`, fault messages |
| `thread-messages.tsx` | Presentational: the message list (`role=list`, named "会话消息") and the "加载更早" button |
| `thread-composer.tsx` | The "给 Agent 发送消息" textarea, "发送", and "结束会话" with a confirm step |
| `run-delivery.tsx` | The Revision delivery line under the Thread (`role=status`, named "Revision 交付"): a registered Revision shows its short commit and whether files changed; otherwise `result.deliveryState` explains a skip or failure, and an ended session whose run has not settled shows saving |
| `thread-panel.tsx` | `IssueThreadPanel`: run selection, the "Agent 会话" heading, the state badge, the waiting notice and composition |
| `*.test.ts(x)` | Pure-function unit tests and MSW integration tests |

## Dependencies

Depends on `src/api` (generated Thread client and types), `features/issues/api` (`useRuns`), `features/issues/types`, `features/spaces/api` (`mutationHeaders`, `useIdempotencyKeys`), `lib/api-client` (`faultCode`), `components/ui`. Used by `features/issues/issue-detail-page.tsx`. `features/spaces/use-space-events.ts` invalidates this module's query through the same generated-client query key without importing this module.

## Invariants

- Cached entries are always one contiguous, seq-ascending window: the first read takes the tail, "加载更早" prepends with `before=<oldest seq>`, incremental reads use `after=<highest seen seq>` (pulled back to just before the oldest queued user turn, so queued→delivered refreshes). Entries are deduplicated by seq and never repeat.
- A sent message's returned entry is **not** merged into the cache: its seq may be ahead of entries not read yet, and advancing the window past them would skip them forever. Only a re-read picks it up, in order.
- An event's `lastSeq` is a hint and is never used as a cursor.
- A GET 404 means the session is not declared yet; it is a normal waiting state (polled every 2.5s), not an error. Once declared, a 5s fallback poll runs in every state but `ended`, in case SSE drops.
- A Revision shows public metadata only (commits, changed, sizes); run settlement publishes no space event, so the line relies on `useRuns` polling while an agent run is unsettled.
- Sending and ending are disabled in `ending` / `ended` (the server's 409 `thread_closed` rule); each submission's idempotency key stays stable across its retries.

## Testing

`thread-panel.test.tsx` drives the panel against a fake Thread backend with the server's cursor semantics (tail / after / before): tail load, 404 → appears, load older, send (including thread_closed), end session, and an SSE event triggering an incremental read. The 404 → appears case relies on the real 2.5s poll and takes about 3 seconds.
