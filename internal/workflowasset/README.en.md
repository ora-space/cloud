# internal/workflowasset: workflow plugin document reader

[中文](README.md) | [English](README.en.md)

Downloads and verifies plugin packages, extracts workflow JSON and parses names and graph objects. It has no database handle, executes no workflow and writes no package to disk.

| File | Responsibility |
|---|---|
| `fetch.go` | `Fetcher.Fetch` accepts credential-free HTTPS URLs or S3 object keys and verifies package SHA-256; HTTPS downloads have a 60-second deadline and no redirects, with deployment egress enforced by the injected transport; nil transport refuses network access |
| `archive.go` | `Documents` reads only direct `assets/workflows/*.json` ZIP children, ignoring directories, symlinks, other extensions and nested files; sorts by path and keeps the first duplicate path |
| `document.go` | `Parse` requires a JSON object and a nonempty, trimmed name of at most 200 bytes; the caller's publish gate decides graph executability |
| `workflowasset.go` | Package/document result types, stable error codes and size constants |

Packages are bounded to 16 MiB, expanded total to 64 MiB, entries to 512 and candidate documents to 1–256. Ignored ZIP entries still consume the expansion budget; extraction also bounds actual reads. Package failure returns no document list; the caller handles each JSON failure independently so other documents can proceed.

`Fetcher.Timeout` can inject the whole HTTPS download deadline before first use; nonpositive values retain 60 seconds. Production wiring keeps the default. Boundary tests shorten deadlines while retaining the real transport, TLS and failure cleanup paths instead of waiting production durations.

`Failure.Detail` is for structured logs only; persisted provenance uses bounded codes. `DocumentError` supplies bounded reasons. On a failed download the detail comes from the checked dialer's per-address attribution (numeric addresses and bounded reasons such as timeout or refused), never from the `http.Client` error text, which carries the request URL and signed query. Tests cover download digests/limits/cancellation/redirects, ZIP bounds and document parsing. Import leases, idempotency and database writes belong to `core/workflow_asset_imports.go`; see [Plugin marketplace](../../docs/plugins/marketplace.en.md).
