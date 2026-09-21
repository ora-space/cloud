# lib: React-free infrastructure

[中文](README.md) | [English](README.en.md)

## Responsibility

Utilities shared by generated code and components that contain neither React nor domain concepts. Every file here must be testable in plain Node.

## Contents

| File | Description |
| --- | --- |
| `api-client.ts` | Shared axios instance `AXIOS_INSTANCE` and orval mutator `customInstance`. It supplies idempotency keys for writes, reconciles react-query's `AbortSignal` with orval's `cancel()`, and never injects browser-readable authentication headers. |
| `api-client.test.ts` | Verifies body unwrapping, write idempotency keys, and both cancellation paths. |
| `mock-api-client.ts` | Routes simulated business endpoints to `/mock-api` and maps real space paths to seeded demo data; authentication and `/api/v1/me` always use the real Gateway. |
| `utils.ts` | Re-exports `cn` (Tailwind-aware class merging); shadcn components import it via `@/lib/utils`. |

## Dependency direction

Third-party libraries only. **Never** imports `react`, `@/components` or `@/api` (`@/api` depends on this module; the reverse would be a cycle).

## Invariants

- The first parameter of `customInstance` must accept orval's `signal: AbortSignal | undefined` (an explicit `undefined` under `exactOptionalPropertyTypes`). Run `npm run typecheck` after touching the signature to confirm the generated client still compiles.
- The shared client supplies missing idempotency keys for writes without replacing caller-provided values.
- Authentication never reads localStorage, Zustand, or a simulated token. The only Cloud authentication facts are the HttpOnly session cookie and `/api/v1/me`.
