# Thread live previews

[中文](README.md) | English

Owns the run-scoped ephemeral stream boundary, version adoption and final-record dominance. Content lives in TanStack Query and is released after its last subscriber leaves; disconnect never stops an Agent.

| File | Responsibility |
| --- | --- |
| `preview.ts` | Snapshot validation, duplicate/out-of-order rejection and history-position merge. |
| `use-thread-preview.ts` | Cancellable subscription, reconnect, permission refusal cleanup and connection UI. |
| `*.test.ts(x)` | Version, final dominance and subscription lifecycle evidence. |

Depends on generated API, `lib/event-stream` and TanStack Query. Issue Thread presentation may depend on this module. Each valid snapshot resets disconnect backoff so credential renewal during a long run reconnects promptly. Previews never mutate the durable Thread cache or trigger a full Thread fetch per chunk.
