# The Plugin Marketplace and Space Plugins

English | [中文](marketplace.md)

Plugins are a space-level resource: an administrator chooses which plugins the space should have, Cloud
distributes that choice to every live runtime in the space, and the Node inside each runtime actually
downloads and installs them. Cloud is the only authority for two things: the **marketplace catalog snapshot**
and the **space's plugin selection state**. Nodes only download and run what Cloud hands them and never sync
the marketplace themselves.

Plugin identity rules (manifests and the canonical `namespace/identifier` Plugin ID) are in the specs
[domain glossary](https://github.com/ora-space/specs/blob/main/domain/glossary.md#plugin).

## Data

| Table | Content |
|---|---|
| `plugin_sources` | Deployment-level marketplace sources. The default source uses the `official` namespace; `synced_at` and `sync_error` record freshness and the latest failure |
| `plugin_catalog_entries` | The synced catalog snapshot, keyed by `(source_namespace, identifier)` |
| `space_plugins` | The space's plugin selection: `desired_*` is the administrator's intent (the version is pinned at selection), `observed_*` aggregates the runtimes' results, with an optimistic version |
| `workspace_plugin_instances` | The execution facts distributed to each runtime, one row per runtime, with composite foreign keys that prevent cross-tenant references |
| `space_agents` | The space's Agent roster, formed by the installation results of Agent plugins |

## Catalog sync

The Cloud server maintains the catalog itself (`internal/pluginmarket`): it syncs once at startup and then every
`plugins.sync_interval` (5 minutes by default):

- The first sync clones into a temporary directory and renames it atomically; later syncs fetch and fast-forward,
  and a non-fast-forward on the source branch fails the sync. Only one sync runs at a time; a concurrent one
  returns immediately instead of queueing.
- It scans `registry/**/orax.toml` and validates each manifest with the same rules as desktop (size, resolver
  version, kind, identifier syntax, text policy, SHA-256 format, the mutual exclusion of download URL and
  targets, and so on). Broken entries are skipped one by one; when a Plugin ID appears several times the first in
  path order wins; entries with `marketplace_visible=false` stay out of the catalog.
- A successful sync replaces the source's whole catalog in one transaction, so readers never see an intermediate
  state; a failure keeps the previous snapshot, records the reason on the source, and the next pass retries.
- **Reading the catalog never leaves the database**: `GET .../spaces/:spaceId/plugins/catalog` reads only this table
  and returns the full catalog with `syncedAt`; filtering and search happen in the frontend.
- After the catalog is replaced, `plugins.catalog_updated` is broadcast to every SSE subscriber.

With `plugins.sync_enabled: false` no marketplace source is seeded and no sync runs; when enabled, the marketplace
URL must be an `https` Git URL without query or fragment and the branch name must be valid, or the server refuses
to start.

## Installing and removing

| Endpoint | Description |
|---|---|
| `GET .../spaces/:spaceId/plugins` | The space's plugin selections and progress summaries; readable by any current member |
| `POST .../spaces/:spaceId/plugins` | Install: `{identifier, pluginVersion?}`; the version can only be the catalog's current one (otherwise `400 plugin_version_unavailable`), is taken from the catalog when omitted, and is pinned at selection |
| `DELETE .../spaces/:spaceId/plugins` | Remove: `{identifier, version}` |

- Only administrators may change the selection (`403 space_role_required`), with an optimistic version. `pack`
  plugins may appear in the catalog but cannot be installed.
- Installing the same version again is idempotent and distributes nothing new; it is also how an administrator
  re-triggers workflow asset import (see [Workflows](../workflows/workflows.en.md#plugin-asset-import)).
- When a selection is accepted, Cloud writes a pending instance for every live runtime that **already exists** at
  that moment; runtimes created later are not added to this maintenance and instead install the plugins when they
  are created or started (see below). In a space with no live runtime the selection completes immediately.
- Summaries give only counts — affected, completed, waiting for start and so on — and never expose another
  runtime's failure output.

## Execution inside runtimes

Installing and removing plugins are Node executions that happen in the `plugin` step of a runtime operation (see
[Workspace lifecycle](../runtime/workspaces.en.md)):

- **The set is frozen on entering the `plugin` step**: creating and starting a runtime take every plugin the space
  wants installed at that moment; `install_plugin` and `remove_plugin` maintenance operations contain just that one
  plugin. An empty set skips the step. Plugins selected after the freeze are not part of this set.
- Each item is a **self-contained payload** assembled from the catalog snapshot: Plugin ID, version, download URL,
  SHA-256 or targets. The Node needs no marketplace lookup and verifies the SHA-256 before writing to disk.
- The Controller registers one `install_plugins` or `remove_plugins` execution with `RecordDispatch`; the target must
  be the runtime's current-generation Node and the input must equal the frozen set.
- **Per-item results**: an installed version must equal the planned one, or the whole result is refused; failed
  items are recorded on their instance with a bounded error code and do **not** keep the runtime from becoming
  ready. When the whole execution fails it is deferred like the clone step and retried with a new execution. A late
  result that does not belong to the current selection returns the instance to pending instead of overwriting the
  newer selection.

**Maintenance waits for idleness.** For runtimes that already exist, a maintenance operation starts only when the
runtime is idle, and it uses the same exclusive boundary as user operations (see
[Runtime control](../runtime/runtime-control.en.md)). Every pending instance carries a waiting reason:

| Reason | Meaning |
|---|---|
| `waiting_start` | The runtime is stopped; install when it next starts |
| `waiting_control` | A page holds the runtime's control |
| `waiting_lifecycle` | The runtime is not ready, or the Project has another operation |
| `waiting_execution` | The runtime still has unfinished executions |
| `legacy_selection_unverified` | A historical selection without a verifiable requester; never executed automatically |

Every time the Controller claims an operation, Cloud rescans pending instances and creates a maintenance operation
for any whose conditions are met, so no extra timer is needed. A stopped runtime is never started just to install
plugins.

**Aggregation rule**: if any instance failed the space state is `failed`; otherwise, while any instance is
unfinished it is `installing` or `removing`; once all have finished (or there is no live runtime) it is `installed`
or `removed`. Instances on run Workspaces are only execution facts of that run and do not take part in the space
aggregate.

## The Agent roster

When the space aggregate of a `kind = agent` plugin becomes `installed`, Cloud creates (or restores) one
`space_agents` row for it in the same transaction, which makes it `@`-mentionable on Issues; when the plugin is
removed the row becomes `retired` rather than deleted, so historical runs can still resolve their executor. A failed
installation on one runtime never removes an existing Agent from the list.

Agent runs use their own run Workspace: the `plugin` step at creation installs every plugin the space wants, and
before the run is ready Cloud also checks that the run's pinned Agent plugin was installed at its pinned version
(see [Agent IssueRuns](../agent-runs/issue-runs.en.md#phases)).

## Related decisions

- [Plugin marketplace: cloud-authoritative selection state, catalog snapshot and self-contained effect payloads](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/0-plugin-marketplace.md)
- [Administrators store the space's plugin intent; per-runtime maintenance waits for idleness and uses the same exclusive boundary](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20260927-admin-selection-and-idle-maintenance.md)
- [Plugin installation and removal are executed by the Node; new Workspaces install the Space's plugins automatically](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20260928-node-executes-plugin-installs.md)
- [Workflow plugin assets are fetched by Cloud and imported document by document as editable copies](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
