# Session observations

[中文](README.md) | [English](README.en.md)

This framework-free boundary projects ora-history archives or public Thread entries into conservative session observations. It does not fetch, authorize, price, infer model calls, or parse native agent logs.

| File | Responsibility |
| --- | --- |
| `index.ts`, `types.ts` | Public adapters and view contracts. |
| `adapter.ts` | Compose a snapshot from explicit inputs. |
| `records.ts` | Validate envelopes and replay final versions by source sequence. |
| `values.ts` | Validate timestamps, frozen model metadata and text blocks. |
| `events.ts` | Classify observations and explicit user-marker groups. |
| `tool-data.ts` | Preserve tool evidence and aggregate known durations. |
| `projection.ts` | Three-lane rows and loaded-record statistics. |
| `adapter.test.ts` | Synthetic, deterministic boundary and fidelity tests. |

Dependencies: standard JavaScript only; session screens, timeline components and query adapters may depend on `@/features/session-data`. Import its public surface, never the helper files.

Invariants: `events` contains final-version replay ordered by source seq; `rawRecords` retains every physical input, including replaced versions. Invalid identities remain unknown rows after ordered records. Archive seq zero is valid; Thread identity uses top-level positive seq independently of `record.seq`. A missing `record.at` stays unknown; `createdAt` is never event time. User marker ordinals group observations, not provider calls; Thread echoes do not create new groups. Thoughts, lifecycle and unknown observations remain in the complete event list. Timeline rows retain unknown time, while bars require `startMs`; tool ends extend the observed extent. Tool kind/title never imply a native name. Counts are tool records, and duration averages use only known values, including measured zero. Provider metrics remain null; frozen metadata is labeled `frozen_run_binding`. Public `turnId` is kept only when explicitly present; no internal database field is read.

Run `npx vitest run src/features/session-data/adapter.test.ts`. Fixtures contain no real logs or session identities; tests make no network, clock or filesystem calls. JSON rendering belongs to the consumer and must remain plain text.

Timeline extents and tool maxima are accumulated without spreading record-sized arrays into function arguments. A synthetic 100,000-record archive verifies the server record limit can be projected without an argument-stack overflow; this is not a browser rendering performance benchmark.
