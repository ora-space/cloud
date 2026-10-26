# internal/contract: OpenAPI definitions

[中文](README.md) | [English](README.en.md)

`Document()` builds OpenAPI 3.0 using Go values. `openapi.go` organizes paths and common schemas; `repository.go` and `session_statistics.go` supply their domain schemas. [api/openapi.json](../../api/openapi.json) is the generated contract; this README does not maintain a second field table.

## Editing and generation

1. Align paths, fields, requests/responses and statuses with [router.Routes](../api/router/router.go).
2. Edit this package and run `task frontend:generate` to generate both OpenAPI and the `frontend/src/api` client.
3. Commit definitions and generated artifacts; never hand-edit JSON or the generated client.

This package generates `TaskConversationMessage`, the task conversation's two-direction sequence cursors, send/cancel/retry/resume/explicit-discard shapes and the `issue.conversation_changed` invalidation event. Task-read `activeRunId`, `blockedRunId`, `pausedRunId`, `canStop` and the stop action's 202 response come from the same contract; older clients may ignore the additional state fields. The optional live path declares `ThreadPreviewSnapshot`, Thread `historySequence`, snapshot reads and `getThreadPreviewEvents` SSE. Snapshots are complete replacements and may be unavailable; they are neither durable Thread data nor execution-success evidence.

`object()` emits `required` only when nonempty, avoiding null/empty arrays rejected by strict OpenAPI consumers.

## Validation and boundaries

- `TestPublishedOpenAPIIsValidAndCurrent` verifies a valid specification and no generated drift.
- `TestRequiredIsOmittedWhenEmpty` walks the JSON tree to verify required shapes.
- [integration/contract_test.go](../../integration/contract_test.go) validates real HTTP responses against the same contract.
- This package defines public transport shapes only; implementation packages own authorization, lifecycle, databases and external I/O. A browser credential-write contract does not mean Cloud accepts raw Keys.

See [cmd/openapi](../../cmd/openapi/README.en.md) for generation and [HTTP API](../../docs/core/http-api.en.md) / the [document map](../../docs/README.en.md) for cross-module semantics.

Artifact list and single-file download responses have their own schemas, matched before the generic `/runs` fallback. Scheduled-artifact invalidation also belongs to the public SSE enum. `scheduled_artifact_test.go` pins both boundaries so generated clients match actual responses.

Single-artifact responses are verified raw bytes with a safely encoded attachment filename, binary media type, nosniff and private no-store; errors remain JSON Faults.
