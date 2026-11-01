import { listMarketSources, syncMarketSource } from '../market-api/generated'
import { MarketSources } from './market-sources'
import { useState } from 'react'
import { useSearchParams } from 'react-router'
import { toast } from 'sonner'
import { Blocks, BookOpen, Download, ExternalLink, Puzzle } from 'lucide-react'
import type { PluginCatalogEntry, SpacePlugin } from '@/api/generated.schemas'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import {
  pluginInstalled,
  pluginKey,
  usePluginMarketplace,
} from '../data/marketplace'
import {
  ActionError,
  DataState,
  Filter,
  NoResults,
  PageHeader,
  SearchInput,
} from '../components/page-parts'
import { DetailPanel } from '../components/detail-panel'
import { ConfirmDialog } from '../components/confirm-dialog'
import { MarkdownPreview } from '../components/markdown-preview'

const kindLabels: Record<string, string> = {
  mcp: 'MCP',
  skill: '技能包',
  webview: 'WebView',
  workflow: '工作流',
  hook: '钩子',
  workbench: '工作台',
  pack: '合集',
}

const stateLabels: Record<string, string> = {
  installed: '已安装',
  failed: '安装失败',
  pending: '等待安装',
  installing: '安装中',
  removing: '移除中',
  removed: '未安装',
}

function StateBadge({ selection }: { selection?: SpacePlugin }) {
  const state = selection?.observedState ?? 'removed'
  return (
    <Badge
      variant={
        state === 'failed'
          ? 'destructive'
          : state === 'installed'
            ? 'secondary'
            : 'outline'
      }
    >
      {stateLabels[state] ?? state}
    </Badge>
  )
}

function missingEntry(selection: SpacePlugin): PluginCatalogEntry {
  return {
    id: pluginKey(selection),
    identifier: selection.identifier,
    title: selection.identifier,
    kind: selection.kind ?? 'skill',
    sourceNamespace: selection.sourceNamespace,
    description: '来源不可用。可以移除已有选择。',
    version: selection.desiredVersion,
    indexedAt: selection.updatedAt,
    sourceUrl: '',
    marketplaceVisible: false,
    homepage: null,
    license: null,
    logo: null,
    packMembers: null,
    readme: null,
    sha256: null,
    targets: null,
    url: null,
  }
}

export default function PluginsPage() {
  const market = usePluginMarketplace()
  const [sourcesOpen, setSourcesOpen] = useState(false)
  const [syncing, setSyncing] = useState(false)
  const synchronize = async () => {
    setSyncing(true)
    try {
      const { items } = await listMarketSources(market.tenantId, market.spaceId)
      const failures: string[] = []
      for (const source of [
        'official',
        ...items.filter((s) => s.enabled).map((s) => s.namespace),
      ]) {
        try {
          await syncMarketSource(market.tenantId, market.spaceId, source)
        } catch {
          failures.push(source)
        }
      }
      await market.refetch()
      if (failures.length) toast.error('部分来源同步失败，已保留上次目录。')
      else toast.success('市场目录已同步')
    } catch {
      toast.error('未能同步市场，请重试。')
    } finally {
      setSyncing(false)
    }
  }
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState('all')
  const [status, setStatus] = useState('all')
  const [order, setOrder] = useState('asc')
  const [removing, setRemoving] = useState<{
    entry: PluginCatalogEntry
    selection: SpacePlugin
  }>()
  const [params, setParams] = useSearchParams()
  const available = new Set(market.catalog.map(pluginKey))
  const catalog = [
    ...market.catalog,
    ...market.installations
      .filter(
        (s) => s.desiredState === 'installed' && !available.has(pluginKey(s)),
      )
      .map(missingEntry),
  ].filter((entry) => entry.kind !== 'agent')
  const selected = catalog.find(
    (entry) =>
      pluginKey(entry) === params.get('plugin') && entry.kind !== 'agent',
  )
  const filtered = catalog
    .filter((entry) => {
      const selection = market.selection(entry)
      return (
        `${entry.title} ${entry.description} ${pluginKey(entry)}`
          .toLowerCase()
          .includes(search.trim().toLowerCase()) &&
        (kind === 'all' || entry.kind === kind) &&
        (status === 'all' ||
          (status === 'installed'
            ? pluginInstalled(selection)
            : status === 'failed'
              ? selection?.observedState === 'failed'
              : !selection || selection.observedState === 'removed'))
      )
    })
    .sort(
      (a, b) =>
        (order === 'asc' ? 1 : -1) * a.title.localeCompare(b.title, 'zh-CN'),
    )
  const clear = () => {
    setSearch('')
    setKind('all')
    setStatus('all')
  }
  const open = (entry: PluginCatalogEntry) => {
    setParams({ plugin: pluginKey(entry) })
  }
  const install = (entry: PluginCatalogEntry) => {
    market.resetErrors()
    void market
      .install(entry)
      .then(() => toast.success(`已安装 ${entry.title}`))
      .catch(() => undefined)
  }
  const requestRemove = (entry: PluginCatalogEntry) => {
    market.resetErrors()
    const selection = market.selection(entry)
    if (selection) setRemoving({ entry, selection })
  }
  const confirmRemove = () => {
    if (!removing) return
    void market
      .remove(removing.selection)
      .then(() => {
        toast.success(`已移除 ${removing.entry.title}`)
        setRemoving(undefined)
      })
      .catch(() => undefined)
  }
  function actions(entry: PluginCatalogEntry) {
    const selection = market.selection(entry)
    if (!available.has(pluginKey(entry)))
      return (
        <Button
          size="sm"
          variant="outline"
          disabled={!market.canManage || market.mutating}
          onClick={() => requestRemove(entry)}
        >
          移除
        </Button>
      )
    if (entry.kind === 'pack')
      return (
        <span className="text-xs text-muted-foreground">合集安装尚未支持</span>
      )
    const busy =
      market.mutating ||
      selection?.observedState === 'installing' ||
      selection?.observedState === 'removing'
    return pluginInstalled(selection) ? (
      <Button
        size="sm"
        variant="outline"
        disabled={!market.canManage || busy}
        onClick={() => requestRemove(entry)}
      >
        移除
      </Button>
    ) : (
      <Button
        size="sm"
        variant={
          selection?.observedState === 'failed' ? 'outline' : 'secondary'
        }
        disabled={!market.canManage || busy}
        onClick={() => install(entry)}
      >
        <Download data-icon="inline-start" />
        {selection?.observedState === 'failed' ? '重试安装' : '安装'}
      </Button>
    )
  }
  return (
    <section>
      <PageHeader
        title="Plugins"
        description="浏览并管理空间插件；安装状态保存在服务端，刷新、换浏览器和服务重启后仍保留。"
        count={catalog.length}
        actions={
          <>
            <Button variant="outline" onClick={() => setSourcesOpen(true)}>
              管理市场
            </Button>
            {market.canManage && (
              <Button
                variant="secondary"
                disabled={syncing || !market.spaceId}
                onClick={() => void synchronize()}
              >
                {syncing ? '同步中…' : '同步'}
              </Button>
            )}
          </>
        }
      />
      <DataState
        pending={market.pending}
        error={market.error}
        retry={() => void market.refetch()}
      >
        {!market.canManage && (
          <p className="text-sm text-muted-foreground">
            当前账号为只读成员；安装与移除需要工作区 owner 权限。
          </p>
        )}
        <div className="settings-toolbar">
          <SearchInput label="搜索插件" value={search} onChange={setSearch} />
          <Filter
            label="插件类型"
            value={kind}
            onChange={setKind}
            options={[
              ['all', '全部类型'],
              ...[...new Set(catalog.map((entry) => entry.kind))].map(
                (value) => [value, kindLabels[value] ?? value] as const,
              ),
            ]}
          />
          <Filter
            label="安装状态"
            value={status}
            onChange={setStatus}
            options={[
              ['all', '全部状态'],
              ['installed', '已安装'],
              ['available', '未安装'],
              ['failed', '安装失败'],
            ]}
          />
          <Filter
            label="插件排序"
            value={order}
            onChange={setOrder}
            options={[
              ['asc', '名称 A–Z'],
              ['desc', '名称 Z–A'],
            ]}
          />
        </div>
        <ActionError error={market.mutationError} />
        <div className="settings-resource-list">
          {filtered.map((entry) => (
            <article
              key={pluginKey(entry)}
              aria-label={entry.title}
              className="settings-resource-row"
            >
              <button
                type="button"
                className="settings-resource-main"
                onClick={() => open(entry)}
                aria-label={`查看插件 ${entry.title}`}
              >
                <span className="settings-resource-icon">
                  {entry.kind === 'skill' ? (
                    <BookOpen className="size-5" />
                  ) : entry.kind === 'pack' ? (
                    <Blocks className="size-5" />
                  ) : (
                    <Puzzle className="size-5" />
                  )}
                </span>
                <span className="min-w-0">
                  <span className="settings-resource-title">{entry.title}</span>
                  <span className="settings-resource-description block">
                    {entry.description}
                  </span>
                  <span className="settings-resource-meta">
                    <span>{kindLabels[entry.kind]}</span>
                    <span aria-hidden="true">·</span>
                    <span>{entry.sourceNamespace}</span>
                    <span aria-hidden="true">·</span>
                    <span>v{entry.version}</span>
                  </span>
                </span>
              </button>
              <div className="settings-resource-actions">
                <StateBadge selection={market.selection(entry)} />
                {actions(entry)}
              </div>
            </article>
          ))}
        </div>
        {!filtered.length && (
          <NoResults filtered={catalog.length > 0} onClear={clear} />
        )}
        <div className="settings-list-footer">
          <span>
            显示 {filtered.length} / {catalog.length} 个插件 · 共{' '}
            {
              market.installations.filter(
                (selection) =>
                  selection.kind !== 'agent' && pluginInstalled(selection),
              ).length
            }{' '}
            个已安装
          </span>
          <span>保存空间选择，不下载或执行插件包</span>
        </div>
      </DataState>
      {params.has('plugin') && !market.pending && !selected && (
        <p role="alert" className="mt-4 text-sm text-muted-foreground">
          找不到该插件。
          <Button variant="link" onClick={() => setParams({})}>
            返回全部插件
          </Button>
        </p>
      )}
      {sourcesOpen && (
        <MarketSources
          tenant={market.tenantId}
          space={market.spaceId}
          owner={market.canManage}
          officialUrl={
            market.catalog.find(
              (entry) => entry.sourceNamespace === 'official',
            )?.sourceUrl
          }
          officialSyncedAt={market.syncedAt}
          onClose={() => setSourcesOpen(false)}
          refresh={market.refetch}
        />
      )}
      {selected && (
        <DetailPanel
          title={selected.title}
          description={selected.description}
          onClose={() => setParams({}, { replace: true })}
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <StateBadge selection={market.selection(selected)} />
            {actions(selected)}
          </div>
          <ActionError error={market.mutationError} />
          <dl className="settings-facts">
            <dt>插件标识</dt>
            <dd>{pluginKey(selected)}</dd>
            <dt>类型</dt>
            <dd>{kindLabels[selected.kind]}</dd>
            <dt>目录版本</dt>
            <dd>
              {available.has(pluginKey(selected))
                ? selected.version
                : '来源不可用'}
            </dd>
            <dt>安装选择</dt>
            <dd>
              {market.selection(selected)?.desiredState === 'installed'
                ? `安装 ${market.selection(selected)?.desiredVersion}`
                : '未选择'}
            </dd>
            <dt>安装结果</dt>
            <dd>{market.selection(selected)?.observedVersion ?? '尚未安装'}</dd>
            <dt>来源</dt>
            <dd>
              {selected.sourceNamespace} · {selected.sourceUrl}
            </dd>
            <dt>许可证</dt>
            <dd>{selected.license ?? '未提供'}</dd>
          </dl>
          {market.selection(selected)?.installError && (
            <p role="alert" className="text-sm text-destructive">
              {market.selection(selected)?.installError}
            </p>
          )}
          <p className="rounded-lg bg-muted p-3 text-xs leading-relaxed text-muted-foreground">
            这里保存 v1 契约中的空间插件选择与最终状态。v2
            临时后端没有通用插件执行器，因此不会下载或运行包内代码。
          </p>
          <Separator />
          {selected.readme ? (
            <MarkdownPreview content={selected.readme} />
          ) : (
            <p className="text-sm text-muted-foreground">
              该插件未提供 README。
            </p>
          )}
          {selected.homepage?.startsWith('https://') && (
            <a
              href={selected.homepage}
              target="_blank"
              rel="noreferrer"
              className="flex items-center gap-2 text-sm underline"
            >
              插件主页
              <ExternalLink className="size-3" />
            </a>
          )}
        </DetailPanel>
      )}
      {removing && (
        <ConfirmDialog
          title={`移除 ${removing.entry.title}？`}
          description={
            removing.entry.kind === 'skill'
              ? '将从空间卸载该技能包，其提供的技能立即变为不可用；重新安装可恢复。'
              : '将从空间移除该插件选择。此操作不会卸载其他工作空间或 Agent 运行时中的包。'
          }
          confirmLabel="移除"
          pending={market.mutating}
          error={market.mutationError}
          onClose={() => setRemoving(undefined)}
          onConfirm={confirmRemove}
        />
      )}
    </section>
  )
}
