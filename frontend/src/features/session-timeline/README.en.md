# session-timeline: session event browsing

English | [中文](README.md)

Browses normalized Cloud sessions using the original dashboard's three lanes, time brush, event list and five detail tabs. It owns no requests, archive downloads, provider parsing or personal log scanning.

| File | Responsibility |
| --- | --- |
| `index.ts` | Public `SessionTimeline`, `SessionRecords`, `ToolSummary`, each accepting `SessionSnapshot` |
| `session-timeline.tsx` | Combines kind filters, full-content search and time selection |
| `timeline-strip.tsx`, `use-time-brush.ts` | Three recorded-time lanes, independent zoom viewport, pointer capture/cancellation and keyboard sliders |
| `compact-timeline.tsx`, `timeline-controls.tsx` | Default equal-width event layout, real-time mode switch and viewport controls |
| `event-list.tsx`, `event-workspace.tsx`, `use-event-selection.ts` | Incrementally rendered lists, turn grouping and stable-ID selection |
| `event-details.tsx` | Read-only Summary, Payload, Result, Timing and JSON tabs |
| `session-records.tsx` | Searches every category and exposes complete pre-replay inputs |
| `tool-summary.tsx`, `timeline-values.ts` | Tool-kind statistics and conservative display formatting |
| `session-timeline.test.tsx`, `timeline-strip.test.tsx` | Synthetic cases for filtering, brushing, zoom, layouts, inspection and selection stability |

Depends on the public `features/session-data` types and `components/ui`; session screens may consume this module. Untimed events have no invented chart position and remain in the list during brushing. Unknown duration/usage stays unknown; measured zero shows `0ms`. Reordered adjacent times never become zero or model latency. Selection follows event IDs through filters and refreshes; changing Run resets interaction state. Model text counts are not LLM call counts, and tool kinds are not native tool names. Raw JSON is rendered as React text. Tests use no real logs or network.

Tool events describe the reported operation kind (for example, `读取 · Cargo.toml`). File names and command titles describe the operation rather than naming a tool; unknown kinds use `工具调用`. Lists, lane tooltips and detail headings share this label, while details also retain the original title.

The default compact view stably sorts a copy by recorded start time. Equal columns and fixed-width blocks express event order, explicitly not duration; source-sequence list order and turn groups stay intact. Real-time mode preserves proportional time and supports zooming a brushed window or returning to the full range. The viewport is separate from filtering: offscreen events are omitted rather than clamped onto edges, crossing tools are clipped to their measured intervals, and details retain full durations. Brushes include crossing intervals and keep unknown-start rows in the list. Subsecond viewports use millisecond ticks; adjacent event gaps never fill absent durations.
