import type { PluginCatalogEntry } from '@/api/generated.schemas'

const DEMO_INDEXED_AT = '2026-11-02T00:00:00Z'
const DEMO_SOURCE_URL = '本地演示'

function demoEntry(
  identifier: string,
  kind: 'mcp' | 'hook',
  title: string,
  description: string,
): PluginCatalogEntry {
  return {
    id: `demo/${identifier}`,
    sourceNamespace: 'demo',
    identifier,
    title,
    kind,
    version: '1.0.0',
    description,
    homepage: null,
    license: 'MIT',
    logo: null,
    url: null,
    sha256: null,
    targets: null,
    packMembers: null,
    readme: `# ${title}\n\n${description}`,
    marketplaceVisible: true,
    sourceUrl: DEMO_SOURCE_URL,
    indexedAt: DEMO_INDEXED_AT,
  }
}

export const demoMcpEntry = demoEntry(
  'sample-mcp',
  'mcp',
  '示例 MCP 工具',
  '演示 MCP 插件的安装与卸载，不会连接外部服务。',
)

export const demoHookEntry = demoEntry(
  'sample-hooks',
  'hook',
  '示例 Hooks',
  '演示生命周期 Hook 的安装与卸载，不会执行自动化脚本。',
)

export function demoPlugin(entry: Pick<PluginCatalogEntry, 'kind'>) {
  return entry.kind === 'mcp' || entry.kind === 'hook'
}

/**
 * v2-server has no MCP/Hook executor yet. Keep one clearly labelled local example visible so the
 * plugin page can still demonstrate the lifecycle when the synced marketplace has no such entry.
 */
export function withDemoCatalog(catalog: PluginCatalogEntry[]) {
  return [
    ...catalog,
    ...(catalog.some((entry) => entry.kind === 'mcp') ? [] : [demoMcpEntry]),
    ...(catalog.some((entry) => entry.kind === 'hook') ? [] : [demoHookEntry]),
  ]
}
