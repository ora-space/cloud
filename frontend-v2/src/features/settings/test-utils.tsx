import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router'
import { ThemeProvider } from 'next-themes'
import { UserContext } from '@/app/auth/user'
import type {
  PluginCatalogEntry,
  SpacePlugin,
} from '@/api/generated.schemas'
import type {
  MarketSource,
  MarketSourceInput,
  SkillProgress,
} from './market-api/generated'
import type { Skill, SkillInput } from '@/lib/skills-api/generated'
import { demoUser, fakeBackend } from '@/test/backend'

export const MARKET_TENANT = '00000000-0000-4000-8000-0000000000aa'
export const MARKET_SPACE = '00000000-0000-4000-8000-0000000000bb'
const MARKET_TIME = '2026-10-31T12:00:00Z'

export function marketplaceEntry(
  identifier: string,
  overrides: Partial<PluginCatalogEntry> = {},
): PluginCatalogEntry {
  const title =
    overrides.title ??
    identifier
      .split('-')
      .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
      .join(' ')
  const sourceNamespace = overrides.sourceNamespace ?? 'demo'
  return {
    id: `${sourceNamespace}/${identifier}`,
    sourceNamespace,
    identifier,
    title,
    kind: 'mcp',
    version: '1.0.0',
    description: `${title} 说明`,
    homepage: null,
    license: 'MIT',
    logo: null,
    url: null,
    sha256: null,
    targets: null,
    packMembers: null,
    readme: `# ${identifier}\n\n${title} 的说明文档。`,
    marketplaceVisible: true,
    sourceUrl: 'https://github.com/ora-space/marketplace',
    indexedAt: MARKET_TIME,
    ...overrides,
  }
}

export function marketplaceSelection(
  entry: PluginCatalogEntry,
  overrides: Partial<SpacePlugin> = {},
): SpacePlugin {
  return {
    id: `selection-${entry.identifier}`,
    tenantId: MARKET_TENANT,
    spaceId: MARKET_SPACE,
    sourceNamespace: entry.sourceNamespace,
    identifier: entry.identifier,
    kind: entry.kind,
    desiredState: 'installed',
    desiredVersion: entry.version,
    observedState: 'installed',
    observedVersion: entry.version,
    installError: null,
    version: 1,
    createdAt: MARKET_TIME,
    updatedAt: MARKET_TIME,
    ...overrides,
  }
}

/**
 * Serves the four Cloud plugin-market routes through the production generated client. The route
 * keeps mutable in-memory state so a test observes the same install/remove lifecycle as the server.
 */
export function installMarketplaceBackend({
  role = 'owner',
  officialEnabled = true,
  catalog = [],
  selections = [],
  marketSources = [],
  skills = [],
  progress = [],
}: {
  role?: 'owner' | 'member'
  officialEnabled?: boolean
  catalog?: PluginCatalogEntry[]
  selections?: SpacePlugin[]
  marketSources?: MarketSource[]
  skills?: Skill[]
  progress?: { namespace: string; identifier: string; results: SkillProgress[] }[]
} = {}) {
  const state = {
    catalog: [...catalog],
    selections: [...selections],
    marketSources: [...marketSources],
    skills: [...skills],
    progress: [...progress],
  }
  const seen: string[] = []
  const bodies: unknown[] = []
  fakeBackend((config) => {
    const method = config.method?.toUpperCase() ?? 'GET'
    const url = config.url ?? ''
    seen.push(`${method} ${url}`)
    if (url === '/api/v1/me/tenants')
      return {
        status: 200,
        data: {
          items: [{ id: MARKET_TENANT, name: '演示团队', role, status: 'active' }],
          nextCursor: '',
        },
      }
    if (url === `/api/v1/tenants/${MARKET_TENANT}/spaces`)
      return {
        status: 200,
        data: {
          items: [
            {
              id: MARKET_SPACE,
              tenantId: MARKET_TENANT,
              name: '演示空间',
              slug: 'demo',
              description: '',
              role,
              version: 1,
              createdBy: demoUser.id,
              createdAt: MARKET_TIME,
              updatedAt: MARKET_TIME,
              archivedAt: null,
            },
          ],
          nextCursor: '',
        },
      }
    const skillBase = `/api/v2/tenants/${MARKET_TENANT}/skills`
    if (url === skillBase && method === 'GET')
      return { status: 200, data: { items: state.skills } }
    if (url === skillBase + '/import' && method === 'POST') {
      const body = (
        typeof config.data === 'string'
          ? JSON.parse(config.data)
          : config.data
      ) as { fileName: string; content: string; overwrite?: boolean }
      const match = body.content.match(
        /^---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/,
      )
      if (!match)
        return {
          status: 400,
          data: { code: 'skill_front_matter_missing', params: {} },
        }
      const name = match[1].match(/^name:\s*["']?([^"'\r\n]+)["']?$/m)?.[1]
      const description = match[1].match(
        /^description:\s*["']?([^"'\r\n]+)["']?$/m,
      )?.[1]
      if (!name)
        return {
          status: 400,
          data: { code: 'skill_name_missing', params: {} },
        }
      if (!description)
        return {
          status: 400,
          data: { code: 'skill_description_missing', params: {} },
        }
      const index = state.skills.findIndex(
        (item) => item.name.toLowerCase() === name.toLowerCase(),
      )
      if (index >= 0 && !body.overwrite)
        return {
          status: 409,
          data: { code: 'skill_name_conflict', params: {} },
        }
      const now = new Date().toISOString()
      const item: Skill = {
        id: index < 0 ? crypto.randomUUID() : state.skills[index].id,
        name,
        description,
        content: match[2].trim(),
        version: index < 0 ? 1 : state.skills[index].version + 1,
        createdAt: index < 0 ? now : state.skills[index].createdAt,
        updatedAt: now,
      }
      if (index < 0) state.skills.unshift(item)
      else state.skills[index] = item
      return { status: 201, data: item }
    }
    if (url === skillBase && method === 'POST') {
      const body = (
        typeof config.data === 'string'
          ? JSON.parse(config.data)
          : config.data
      ) as SkillInput
      if (
        state.skills.some(
          (item) =>
            item.name.toLowerCase() === body.name.trim().toLowerCase(),
        )
      )
        return {
          status: 409,
          data: { code: 'skill_name_conflict', params: {} },
        }
      const now = new Date().toISOString()
      const item: Skill = {
        id: crypto.randomUUID(),
        ...body,
        version: 1,
        createdAt: now,
        updatedAt: now,
      }
      state.skills.unshift(item)
      return { status: 201, data: item }
    }
    if (url.startsWith(skillBase + '/')) {
      const id = decodeURIComponent(url.slice(skillBase.length + 1))
      const index = state.skills.findIndex((item) => item.id === id)
      if (index < 0)
        return {
          status: 404,
          data: { code: 'skill_not_found', params: {} },
        }
      if (method === 'DELETE') {
        state.skills.splice(index, 1)
        return { status: 204 }
      }
      if (method === 'PUT') {
        const body = (
          typeof config.data === 'string'
            ? JSON.parse(config.data)
            : config.data
        ) as SkillInput
        if (
          state.skills.some(
            (item) =>
              item.id !== id &&
              item.name.toLowerCase() === body.name.trim().toLowerCase(),
          )
        )
          return {
            status: 409,
            data: { code: 'skill_name_conflict', params: {} },
          }
        const item = {
          ...state.skills[index],
          ...body,
          version: state.skills[index].version + 1,
          updatedAt: new Date().toISOString(),
        }
        state.skills[index] = item
        return { status: 200, data: item }
      }
    }
    const base = `/api/v1/tenants/${MARKET_TENANT}/spaces/${MARKET_SPACE}/plugins`
    const sourceBase = `${base}/sources`
    const resultsBase = `${base}/results`
    if (url.startsWith(resultsBase + '/') && method === 'GET') {
      const [namespace, identifier] = url
        .slice(resultsBase.length + 1)
        .split('/')
        .map(decodeURIComponent)
      return {
        status: 200,
        data: {
          items: state.progress
            .filter(
              (item) =>
                item.namespace === namespace && item.identifier === identifier,
            )
            .flatMap((item) => item.results),
        },
      }
    }
    if (url === sourceBase && method === 'GET')
      return { status: 200, data: { items: state.marketSources, officialEnabled } }
    if (url === sourceBase && method === 'POST') {
      if (role !== 'owner')
        return { status: 403, data: { code: 'forbidden', params: {} } }
      const body = (
        typeof config.data === 'string'
          ? JSON.parse(config.data)
          : config.data
      ) as MarketSourceInput
      bodies.push(body)
      const source: MarketSource = {
        ...body,
        namespace: 'custom-market',
        syncedAt: null,
        failure: '',
      }
      state.marketSources.push(source)
      return { status: 204 }
    }
    if (url.startsWith(sourceBase + '/')) {
      if (role !== 'owner')
        return { status: 403, data: { code: 'forbidden', params: {} } }
      const [namespace, action] = url
        .slice(sourceBase.length + 1)
        .split('/')
        .map(decodeURIComponent)
      const index = state.marketSources.findIndex(
        (source) => source.namespace === namespace,
      )
      if (namespace === 'official' && action === 'sync' && officialEnabled) return { status: 204 }
      if (index < 0)
        return { status: 404, data: { code: 'not_found', params: {} } }
      if (action === 'sync') return { status: 204 }
      if (method === 'DELETE') {
        state.marketSources.splice(index, 1)
        return { status: 204 }
      }
      if (method === 'PATCH') {
        const body = (
          typeof config.data === 'string'
            ? JSON.parse(config.data)
            : config.data
        ) as MarketSourceInput
        bodies.push(body)
        state.marketSources[index] = {
          ...state.marketSources[index],
          ...body,
        }
        return { status: 204 }
      }
      return undefined
    }
    if (url === `${base}/catalog`)
      return {
        status: 200,
        data: { items: state.catalog, syncedAt: MARKET_TIME },
      }
    if (url === base && method === 'GET')
      return { status: 200, data: { items: state.selections } }
    if (url !== base || !['POST', 'DELETE'].includes(method)) return undefined
    const body =
      typeof config.data === 'string'
        ? (JSON.parse(config.data) as {
            identifier: string
            version: number
            pluginVersion?: string
          })
        : (config.data as {
            identifier: string
            version: number
            pluginVersion?: string
          })
    bodies.push(body)
    const entry = state.catalog.find((item) => item.id === body.identifier)
    if (!entry) return { status: 404, data: { code: 'plugin_not_found', params: {} } }
    const index = state.selections.findIndex(
      (item) =>
        `${item.sourceNamespace}/${item.identifier}` === body.identifier,
    )
    if (role !== 'owner')
      return { status: 403, data: { code: 'forbidden', params: {} } }
    if (method === 'DELETE') {
      if (index < 0) return { status: 404, data: { code: 'plugin_not_installed', params: {} } }
      const current = state.selections[index]
      if (!current || current.version !== body.version)
        return { status: 409, data: { code: 'version_conflict', params: {} } }
      if (current.kind === 'agent' && current.requestedByUserId == null)
        return { status: 409, data: { code: 'plugin_runtime_managed', params: {} } }
      const resource = marketplaceSelection(entry, {
        ...current,
        desiredState: 'removed',
        observedState: 'removed',
        observedVersion: null,
        version: current.version + 1,
      })
      state.selections[index] = resource
      return { status: 200, data: { resource } }
    }
    if (entry.kind === 'pack')
      return {
        status: 400,
        data: { code: 'plugin_kind_not_installable', params: {} },
      }
    const current = index < 0 ? undefined : state.selections[index]
    if (current?.kind === 'agent' && current.requestedByUserId == null)
      return { status: 409, data: { code: 'plugin_runtime_managed', params: {} } }
    if ((current?.version ?? 0) !== body.version)
      return { status: 409, data: { code: 'version_conflict', params: {} } }
    const resource = marketplaceSelection(entry, {
      ...current,
      desiredState: 'installed',
      desiredVersion: body.pluginVersion ?? entry.version,
      observedState: 'installed',
      observedVersion: body.pluginVersion ?? entry.version,
      requestedByUserId: demoUser.id,
      version: (current?.version ?? 0) + 1,
    })
    if (index < 0) state.selections.push(resource)
    else state.selections[index] = resource
    return { status: 200, data: { resource } }
  })
  return { state, seen, bodies }
}

export function renderSettingsPage(element: ReactNode, path = '/settings') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const router = createMemoryRouter([{ path: '*', element }], {
    initialEntries: [path],
  })
  const result = render(
    <ThemeProvider
      attribute="class"
      storageKey="ora-v2-theme"
      defaultTheme="light"
      scriptProps={{ type: 'text/plain' }}
    >
      <QueryClientProvider client={client}>
        <UserContext.Provider value={demoUser}>
          <RouterProvider router={router} />
        </UserContext.Provider>
      </QueryClientProvider>
    </ThemeProvider>,
  )
  return { ...result, client, router }
}
