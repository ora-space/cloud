import { http, HttpResponse } from 'msw'
import type { PluginCatalogEntry, SpacePlugin } from '@/api/generated.schemas'
import { jsonObject, stringField } from './shared'

/**
 * MSW fixtures for the cloud-backed plugin screen. The handlers answer the
 * real `/api/v1/.../plugins` contract (not the mock-api domain), so tests
 * exercise the same shapes the generated client types describe. They are test
 * scaffolding only: the app never registers them in its browser worker, so
 * the live backend always serves the plugin screen. Tests install them with
 * `server.use(...pluginCloudHandlers(...))` per scenario.
 */

export const PLUGIN_FIXTURE_SPACE_ID = '22222222-2222-2222-2222-222222222222'
export const PLUGIN_FIXTURE_TENANT_ID = '11111111-1111-1111-1111-111111111111'
export const PLUGIN_FIXTURE_SYNCED_AT = '2026-09-23T10:00:00+08:00'

/** One catalog fixture entry; the shape matches PluginCatalogEntry exactly. */
export function catalogEntry(
  id: string,
  overrides: Partial<PluginCatalogEntry> = {},
): PluginCatalogEntry {
  return {
    id,
    sourceNamespace: 'official',
    identifier: id.split('/')[1] ?? id,
    title: id.split('/')[1] ?? id,
    kind: 'agent',
    version: '1.0.0',
    description: 'A marketplace fixture plugin.',
    homepage: null,
    license: null,
    logo: null,
    url: null,
    sha256: null,
    targets: null,
    packMembers: null,
    readme: null,
    marketplaceVisible: true,
    sourceUrl: 'https://example.invalid/market.git',
    indexedAt: PLUGIN_FIXTURE_SYNCED_AT,
    ...overrides,
  }
}

/** One space-plugin fixture row with the default installed/installed state. */
export function spacePlugin(id: string, overrides: Partial<SpacePlugin> = {}): SpacePlugin {
  return {
    id,
    spaceId: PLUGIN_FIXTURE_SPACE_ID,
    tenantId: PLUGIN_FIXTURE_TENANT_ID,
    sourceNamespace: 'official',
    identifier: id.split('/')[1] ?? id,
    desiredState: 'installed',
    desiredVersion: '1.0.0',
    observedState: 'installed',
    observedVersion: '1.0.0',
    installError: null,
    version: 1,
    createdAt: PLUGIN_FIXTURE_SYNCED_AT,
    updatedAt: PLUGIN_FIXTURE_SYNCED_AT,
    ...overrides,
  }
}

/** Fault body of the backend error contract. */
function fault(code: string, status: number, params: Record<string, unknown> = {}) {
  return HttpResponse.json({ code, params, requestId: 'fixture' }, { status })
}

/** Options steering the fixture behavior, including the conflict branches. */
export interface PluginCloudFixtureOptions {
  /** Catalog rows the GET catalog endpoint answers with. */
  catalog: PluginCatalogEntry[]
  /** Initial space-plugin rows; the fixture mutates its own copy. */
  plugins?: SpacePlugin[]
  /** Failure mode for DELETE: stale optimistic version or missing version. */
  removeConflict?: 'version_conflict' | 'version_required'
  /** Failure mode for POST: 404 for identifiers absent from the catalog. */
  installMissing?: boolean
  /** Observes every DELETE body so tests can pin the optimistic version. */
  onRemove?: (identifier: string, version: number) => void
}

/**
 * Handlers for the plugin screen with mutable in-test state: a successful
 * POST appends (or returns) the row so the invalidated query refetches it,
 * and DELETE honors the requested conflict branch.
 */
export function pluginCloudHandlers(options: PluginCloudFixtureOptions) {
  const plugins: SpacePlugin[] = [...(options.plugins ?? [])]
  return [
    http.get('/api/v1/tenants/:tid/spaces/:spaceId/plugins/catalog', () =>
      HttpResponse.json({ items: options.catalog, syncedAt: PLUGIN_FIXTURE_SYNCED_AT }),
    ),
    http.get('/api/v1/tenants/:tid/spaces/:spaceId/plugins', () =>
      HttpResponse.json({ items: plugins }),
    ),
    http.post('/api/v1/tenants/:tid/spaces/:spaceId/plugins', async ({ request }) => {
      const body = jsonObject(await request.json())
      const identifier = stringField(body['identifier'])
      const entry = options.catalog.find((candidate) => candidate.id === identifier)
      if (!entry || options.installMissing) return fault('plugin_not_found', 404)
      const existing = plugins.find((p) => p.id === identifier && p.desiredState === 'installed')
      if (existing) return HttpResponse.json({ resource: existing })
      const row = spacePlugin(entry.id, {
        observedState: 'pending',
        observedVersion: null,
        desiredVersion: entry.version,
      })
      plugins.push(row)
      return HttpResponse.json({ resource: row })
    }),
    http.delete('/api/v1/tenants/:tid/spaces/:spaceId/plugins', async ({ request }) => {
      const body = jsonObject(await request.json())
      const identifier = stringField(body['identifier'])
      const version = typeof body['version'] === 'number' ? body['version'] : 0
      options.onRemove?.(identifier ?? '', version)
      const row = plugins.find((p) => p.id === identifier)
      if (!row) return fault('plugin_not_found', 404)
      if (options.removeConflict === 'version_required' && version === 0)
        return fault('version_required', 428)
      if (options.removeConflict === 'version_conflict') return fault('version_conflict', 409)
      row.desiredState = 'removed'
      row.observedState = 'removed'
      row.version += 1
      return HttpResponse.json({ resource: row })
    }),
  ]
}
