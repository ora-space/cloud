# internal/pluginmarket: plugin marketplace sync

[English](README.en.md) | [中文](README.md)

`internal/pluginmarket` syncs the configured plugin marketplace git repository into cloud's
PostgreSQL catalog snapshot. It owns only the network/git/scan half of the chain: it validates
and mirrors the desktop marketplace contract (orax.toml layout, identifier namespaces, release
triples) and hands the result to `internal/core` through the `CatalogSink` interface. It holds no
database handle, spawns no goroutines of its own (`RunSyncLoop` is driven by the process
lifecycle), and never modifies the desktop repository.

## Files

- `manifest.go`: orax.toml parsing and validation, mirroring desktop `crates/plugin-manifest`
  resolver-1 rules field by field (size cap, 8 kinds, semver, slug identifiers, text policies,
  sha256, URL/object keys, target-triple allowlist, pack exclusivity).
- `catalog.go`: scans `registry/**/orax.toml` into catalog entries; `marketplace_visible=false`
  and broken listings are skipped; duplicates on one canonical id resolve first-in-path-order;
  logo variants (universal/light/dark with extension priority) and READMEs (truncated) resolve.
- `sync.go`: `Syncer.Sync` — seed → go-git clone/pull --ff-only → scan → transactional catalog
  replace; single-flight admission (concurrent callers get `ErrSyncInFlight`); failures record
  `sync_error` and keep the previous snapshot.
- `loop.go`: `RunSyncLoop` — one sync at startup, then once per interval until ctx cancels;
  failures are logged and never stop the loop; the injectable `SleepFunc` keeps ticks testable.

## Dependencies and callers

- Depends on: `pelletier/go-toml/v2` (manifest parsing), `go-git/go-git/v5` (pure-Go git
  transport), `zap` (logging).
- Called by: `cmd/server` (wires the sync loop); `internal/core` implements `CatalogSink`
  (authoritative persistence).
- Must not depend on: any `internal/core` type (avoids a cycle); this is a leaf package.

## Invariants

- Network and scanning happen entirely outside database transactions; the write is one
  transactional replace (DELETE + INSERT + timestamp).
- A failed sync never overwrites the previous catalog snapshot (failure paths only write
  `plugin_sources.sync_error`).
- Catalog reads never leave the database: the API serves the PG snapshot, matching desktop's
  cache-only read semantics.
- `ErrNonFastForwardMerge` is the `pull --ff-only` contract; desktop's `gitlancer` has the same
  semantics.

## Tests

- Unit tests are fully offline: go-git local bare repositories, no real network; `fakeSink`
  asserts the transaction boundary; single-flight uses a blocking seed for deterministic sync;
  the loop uses an injected sleep to drive ticks.
- Real PG constraints and the HTTP chain live in `integration/plugins_test.go`.
