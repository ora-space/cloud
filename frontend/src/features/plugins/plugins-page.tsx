import { Puzzle, Search } from 'lucide-react'
import { useState } from 'react'
import type { PluginCatalogEntry, SpacePlugin } from '@/api/generated.schemas'
import { PageHeader } from '@/components/layout/page-header'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { useInstallPlugin, usePluginCatalog, useRemovePlugin, useSpacePlugins } from './api'

/** Display labels for the eight resolver-1 plugin kinds. */
const KIND_LABELS: Record<string, string> = {
  workbench: '工作台',
  agent: '智能体',
  webview: 'WebView',
  skill: '技能',
  mcp: 'MCP',
  hook: '钩子',
  pack: '插件包',
  workflow: '工作流',
}

/** Filter order for the kind chips, mirroring the desktop marketplace sort. */
const KIND_FILTERS = ['agent', 'workbench', 'webview', 'skill', 'mcp', 'hook', 'pack'] as const

/** Observed-state badge per fan-out aggregate state. */
const OBSERVED_BADGES: Record<
  SpacePlugin['observedState'],
  { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }
> = {
  pending: { label: '待安装', variant: 'secondary' },
  installing: { label: '安装中', variant: 'secondary' },
  installed: { label: '已安装', variant: 'default' },
  failed: { label: '失败', variant: 'destructive' },
  removing: { label: '移除中', variant: 'outline' },
  removed: { label: '已移除', variant: 'outline' },
}

/** One selected plugin row: state badge, pinned version, and removal. */
function SelectedPluginRow({
  plugin,
  onRemove,
}: {
  plugin: SpacePlugin
  onRemove: (plugin: SpacePlugin) => void
}) {
  const badge = OBSERVED_BADGES[plugin.observedState]
  const busy = plugin.observedState === 'removing'
  return (
    <div className="flex items-center gap-3 border-b px-4 py-3">
      <Puzzle className="size-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <p className="truncate text-sm font-medium">{plugin.id}</p>
          <Badge variant={badge.variant}>{badge.label}</Badge>
        </div>
        <p className="truncate text-xs text-muted-foreground">
          版本 {plugin.desiredVersion}
          {plugin.installError ? ` · ${plugin.installError}` : ''}
        </p>
      </div>
      <AlertDialog>
        <AlertDialogTrigger
          render={
            <Button variant="outline" size="sm" disabled={busy}>
              移除
            </Button>
          }
        />
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>移除插件</AlertDialogTitle>
            <AlertDialogDescription>
              确认从该工作区移除 {plugin.id}?运行时工作区中的安装将被删除。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={() => onRemove(plugin)}>
              移除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

/** One marketplace card: identity, kind, version, description, install. */
function CatalogCard({
  entry,
  installed,
  busy,
  onInstall,
}: {
  entry: PluginCatalogEntry
  installed: SpacePlugin | undefined
  busy: boolean
  onInstall: (entry: PluginCatalogEntry) => void
}) {
  const pack = entry.kind === 'pack'
  const already = installed && installed.desiredState === 'installed'
  const installing =
    !!installed &&
    (installed.observedState === 'pending' || installed.observedState === 'installing')
  const disabled = busy || pack || already || installing
  // In-flight state wins over "already installed": the row exists the moment
  // the install is accepted, but its badge only settles once the effects do.
  let label = '安装'
  if (pack) label = '暂不支持'
  else if (installing) label = '安装中'
  else if (already) label = '已安装'
  return (
    <div className="flex flex-col gap-2 rounded-lg border p-4">
      <div className="flex items-center gap-2">
        <Puzzle className="size-4 shrink-0 text-muted-foreground" />
        <p className="min-w-0 flex-1 truncate text-sm font-medium">{entry.title}</p>
        <Badge variant="outline">{KIND_LABELS[entry.kind] ?? entry.kind}</Badge>
      </div>
      <p className="text-xs text-muted-foreground">{entry.id}</p>
      <p className="line-clamp-2 min-h-8 text-xs text-muted-foreground">{entry.description}</p>
      <div className="mt-auto flex items-center justify-between">
        <span className="text-xs text-muted-foreground">v{entry.version}</span>
        <Button size="sm" disabled={disabled} onClick={() => onInstall(entry)}>
          {label}
        </Button>
      </div>
    </div>
  )
}

/**
 * The workspace's plugin screen: the selected-plugin list with fan-out state
 * badges on top, and the marketplace catalog (search + kind filter) below.
 * Activation/stop/configuration stay out of scope — they belong to the Node
 * runtime plane, not the cloud selection state.
 */
export function PluginsPage({ slug: _slug }: { slug: string }) {
  const { entries, isPending: catalogPending } = usePluginCatalog()
  const { plugins } = useSpacePlugins()
  const install = useInstallPlugin()
  const remove = useRemovePlugin()
  const [query, setQuery] = useState('')
  const [kindFilter, setKindFilter] = useState('')

  const byID = new Map(plugins.map((p) => [p.id, p]))
  const selected = plugins.filter((p) => p.desiredState === 'installed')
  const normalized = query.trim().toLowerCase()
  const removeCode = remove.error?.response?.data?.code
  const visible = entries.filter(
    (entry) =>
      (!kindFilter || entry.kind === kindFilter) &&
      (!normalized ||
        entry.title.toLowerCase().includes(normalized) ||
        entry.id.toLowerCase().includes(normalized) ||
        entry.description.toLowerCase().includes(normalized)),
  )

  return (
    <div className="flex h-full flex-col">
      <PageHeader title="插件" />
      <div className="flex-1 overflow-y-auto">
        <section aria-label="已选插件">
          <h2 className="px-4 pb-1 pt-4 text-xs font-medium text-muted-foreground">已选插件</h2>
          {removeCode && (
            <p role="alert" className="px-4 py-1 text-sm text-destructive">
              {removeCode === 'version_conflict'
                ? '移除失败:版本冲突,已刷新最新状态,请重试。'
                : '移除失败,请稍后重试。'}
            </p>
          )}
          {selected.length === 0 && (
            <p className="px-4 py-2 text-sm text-muted-foreground">尚未安装插件。</p>
          )}
          {selected.map((plugin) => (
            <SelectedPluginRow
              key={plugin.id}
              plugin={plugin}
              onRemove={(p) => remove.mutate({ identifier: p.id, version: p.version })}
            />
          ))}
        </section>

        <section aria-label="插件市场" className="pb-6">
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-6">
            <h2 className="text-xs font-medium text-muted-foreground">插件市场</h2>
            <div className="relative ml-auto">
              <Search className="absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                aria-label="搜索插件"
                className="h-7 w-44 pl-7"
                placeholder="搜索插件"
                value={query}
                onChange={(event) => setQuery(event.target.value)}
              />
            </div>
          </div>
          <div className="flex flex-wrap gap-1.5 px-4 pb-3">
            <Button
              size="sm"
              variant={kindFilter === '' ? 'default' : 'outline'}
              onClick={() => setKindFilter('')}
            >
              全部
            </Button>
            {KIND_FILTERS.map((kind) => (
              <Button
                key={kind}
                size="sm"
                variant={kindFilter === kind ? 'default' : 'outline'}
                onClick={() => setKindFilter(kindFilter === kind ? '' : kind)}
              >
                {KIND_LABELS[kind]}
              </Button>
            ))}
          </div>
          {catalogPending && (
            <div className="grid gap-3 px-4 sm:grid-cols-2 lg:grid-cols-3">
              {['one', 'two', 'three', 'four', 'five', 'six'].map((key) => (
                <Skeleton key={key} className="h-32 w-full" />
              ))}
            </div>
          )}
          <div className="grid gap-3 px-4 sm:grid-cols-2 lg:grid-cols-3">
            {visible.map((entry) => (
              <CatalogCard
                key={entry.id}
                entry={entry}
                installed={byID.get(entry.id)}
                busy={install.isPending}
                onInstall={(e) => install.mutate({ identifier: e.id })}
              />
            ))}
          </div>
        </section>
      </div>
    </div>
  )
}
