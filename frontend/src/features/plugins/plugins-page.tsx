import { Puzzle, Search, Workflow } from 'lucide-react'
import { useState } from 'react'
import type {
  PluginCatalogEntry,
  SpacePlugin,
  WorkflowAssetDocument,
  WorkflowAssetImport,
} from '@/api/generated.schemas'
import { useCurrentSpace } from '@/features/spaces/current-space'
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
import {
  useInstallPlugin,
  usePluginCatalog,
  useRemovePlugin,
  useSpacePlugins,
  useWorkflowAssetImports,
} from './api'

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
  pending: { label: '等待执行', variant: 'secondary' },
  installing: { label: '安装中', variant: 'secondary' },
  installed: { label: '已安装', variant: 'default' },
  failed: { label: '失败', variant: 'destructive' },
  removing: { label: '移除中', variant: 'outline' },
  removed: { label: '已移除', variant: 'outline' },
}

/** Import-state badge per workflow asset import row. */
const ASSET_IMPORT_BADGES: Record<
  WorkflowAssetImport['state'],
  { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline' }
> = {
  pending: { label: '等待导入', variant: 'secondary' },
  importing: { label: '导入中', variant: 'secondary' },
  completed: { label: '导入完成', variant: 'default' },
  failed: { label: '导入失败', variant: 'destructive' },
}

/**
 * Human labels for the bounded package and document failure codes. The codes
 * are the stable contract; this table is presentation only, and an unmapped
 * code falls back to the raw value so a new server code stays readable.
 */
const ASSET_FAILURE_LABELS: Record<string, string> = {
  asset_package_unavailable: '包不可用',
  asset_package_too_large: '包超出大小上限',
  asset_package_digest_mismatch: '包摘要不匹配',
  asset_package_invalid: '包格式无效',
  asset_package_expansion_too_large: '包展开后超出大小上限',
  asset_package_too_many_entries: '包内条目过多',
  asset_package_without_documents: '包内没有工作流文档',
  asset_package_too_many_documents: '包内工作流文档过多',
  document_invalid_json: 'JSON 无效',
  document_without_name: '缺少名称',
  document_not_executable_graph: '不是可执行工作流图',
  name_conflict: '工作流名称已被占用',
}

/** Resolves a bounded failure code to a label without hiding unknown codes. */
function failureLabel(code: string): string {
  return ASSET_FAILURE_LABELS[code] ?? code
}

/** One imported or failed document inside a workflow asset import. */
function AssetDocumentRow({ document }: { document: WorkflowAssetDocument }) {
  const imported = document.outcome === 'imported'
  return (
    <li className="flex items-center gap-2 text-xs text-muted-foreground">
      <span className="min-w-0 flex-1 truncate font-mono">{document.documentPath}</span>
      {imported ? (
        <span>
          已导入 · 快照 v{document.snapshotVersion ?? '?'}
          {document.workflowId ? ` · ${document.workflowId}` : ''}
        </span>
      ) : (
        <span className="text-destructive">失败：{failureLabel(document.failureCode ?? '')}</span>
      )}
    </li>
  )
}

/**
 * One workflow plugin's asset import: package provenance, per-document
 * outcomes and the package-level failure. It never shares a badge with the
 * install row: a completed install and a failed import are both true at once.
 */
function WorkflowAssetImportRow({ item }: { item: WorkflowAssetImport }) {
  const badge = ASSET_IMPORT_BADGES[item.state]
  return (
    <div className="border-b px-4 py-3">
      <div className="flex items-center gap-2">
        <Workflow className="size-4 shrink-0 text-muted-foreground" />
        <p className="min-w-0 flex-1 truncate text-sm font-medium">{item.pluginId}</p>
        <Badge variant={badge.variant}>{badge.label}</Badge>
      </div>
      <p className="truncate text-xs text-muted-foreground">
        {item.sourceNamespace}/{item.identifier} · 包版本 {item.packageVersion || '未知'}
      </p>
      {item.failureCode && (
        <p role="alert" className="text-xs text-destructive">
          导入失败：{failureLabel(item.failureCode)}
        </p>
      )}
      {item.documents.length > 0 && (
        <ul className="mt-1 space-y-0.5">
          {item.documents.map((document) => (
            <AssetDocumentRow key={document.documentPath} document={document} />
          ))}
        </ul>
      )}
    </div>
  )
}

/** One selected plugin row: state badge, pinned version, and removal. */
function SelectedPluginRow({
  plugin,
  onRemove,
  canManage,
}: {
  plugin: SpacePlugin
  canManage: boolean
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
      <p className="text-xs text-muted-foreground">
        运行时 {plugin.affectedCount ?? 0} · 已完成 {plugin.completedCount ?? 0} · 等待占用{' '}
        {plugin.waitingControlCount ?? 0} · 等待启动 {plugin.waitingStartCount ?? 0} · 暂不可执行{' '}
        {plugin.unavailableCount ?? 0} · 失败 {plugin.failedCount ?? 0}
      </p>
      <AlertDialog>
        <AlertDialogTrigger
          render={
            <Button variant="outline" size="sm" disabled={busy || !canManage}>
              移除
            </Button>
          }
        />
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>移除插件</AlertDialogTitle>
            <AlertDialogDescription>
              确认从该协作空间移除 {plugin.id}
              ？选择会先保存，各运行时将在空闲并取得维护独占权后执行移除；已停止的运行时等待下次正常启动。
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
  canManage,
}: {
  entry: PluginCatalogEntry
  installed: SpacePlugin | undefined
  busy: boolean
  canManage: boolean
  onInstall: (entry: PluginCatalogEntry) => void
}) {
  const pack = entry.kind === 'pack'
  const already = installed && installed.desiredState === 'installed'
  const installing =
    !!installed &&
    (installed.observedState === 'pending' || installed.observedState === 'installing')
  const disabled = !canManage || busy || pack || already || installing
  // In-flight state wins over "already installed": the row exists the moment
  // the install is accepted, but its badge only settles once the effects do.
  let label = '安装'
  if (pack) label = '暂不支持'
  else if (installing) label = '已选择，等待执行'
  else if (already) label = '已选择'
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
 * badges on top, the separately readable workflow asset imports below it, and
 * the marketplace catalog (search + kind filter) last. Activation/stop/
 * configuration stay out of scope — they belong to the Node runtime plane,
 * not the cloud selection state.
 */
function KindFilters({
  selected,
  onChange,
}: {
  selected: string
  onChange: (value: string) => void
}) {
  return (
    <div className="flex flex-wrap gap-1.5 px-4 pb-3">
      <Button
        size="sm"
        variant={selected === '' ? 'default' : 'outline'}
        onClick={() => onChange('')}
      >
        全部
      </Button>
      {KIND_FILTERS.map((kind) => (
        <Button
          key={kind}
          size="sm"
          variant={selected === kind ? 'default' : 'outline'}
          onClick={() => onChange(selected === kind ? '' : kind)}
        >
          {KIND_LABELS[kind]}
        </Button>
      ))}
    </div>
  )
}

/** The selected-plugin list with removal, its conflict notice and empty state. */
function SelectedPluginsSection({
  plugins,
  canManage,
  removeCode,
  onRemove,
}: {
  plugins: SpacePlugin[]
  canManage: boolean
  removeCode: string | undefined
  onRemove: (plugin: SpacePlugin) => void
}) {
  return (
    <section aria-label="已选插件">
      <h2 className="px-4 pb-1 pt-4 text-xs font-medium text-muted-foreground">已选插件</h2>
      {removeCode && (
        <p role="alert" className="px-4 py-1 text-sm text-destructive">
          {removeCode === 'version_conflict'
            ? '移除失败:版本冲突,已刷新最新状态,请重试。'
            : '移除失败,请稍后重试。'}
        </p>
      )}
      {plugins.length === 0 && (
        <p className="px-4 py-2 text-sm text-muted-foreground">尚未安装插件。</p>
      )}
      {plugins.map((plugin) => (
        <SelectedPluginRow
          key={plugin.id}
          plugin={plugin}
          canManage={canManage}
          onRemove={onRemove}
        />
      ))}
    </section>
  )
}

/** The workflow asset import results; never merged into the install badges. */
function WorkflowAssetsSection({
  imports,
  isError,
}: {
  imports: WorkflowAssetImport[]
  isError: boolean
}) {
  return (
    <section aria-label="工作流资产导入">
      <h2 className="px-4 pb-1 pt-4 text-xs font-medium text-muted-foreground">工作流资产</h2>
      {isError && (
        <p role="alert" className="px-4 py-1 text-sm text-destructive">
          工作流资产导入结果读取失败，请稍后重试。
        </p>
      )}
      {!isError && imports.length === 0 && (
        <p className="px-4 py-2 text-sm text-muted-foreground">
          选择工作流插件后，包内的 assets/workflows/*.json 会导入当前租户的工作流库。
        </p>
      )}
      {imports.map((item) => (
        <WorkflowAssetImportRow key={`${item.sourceNamespace}/${item.identifier}`} item={item} />
      ))}
    </section>
  )
}

export function PluginsPage({ slug: _slug }: { slug: string }) {
  const { space } = useCurrentSpace()
  const canManage = space?.role === 'admin'
  const { entries, isPending: catalogPending } = usePluginCatalog()
  const { plugins } = useSpacePlugins()
  const { imports, isError: importsError } = useWorkflowAssetImports()
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
      <p className="px-4 py-2 text-sm text-muted-foreground">
        {canManage
          ? '管理员修改空间插件选择；各运行时等待空闲后执行。当前执行能力未开放，保存选择不会启动运行时。'
          : '成员可查看空间插件，由管理员修改选择。'}
      </p>
      <div className="flex-1 overflow-y-auto">
        <SelectedPluginsSection
          plugins={selected}
          canManage={canManage}
          removeCode={removeCode}
          onRemove={(plugin) => remove.mutate({ identifier: plugin.id, version: plugin.version })}
        />
        <WorkflowAssetsSection imports={imports} isError={importsError} />

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
          <KindFilters selected={kindFilter} onChange={setKindFilter} />
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
                canManage={canManage}
                installed={byID.get(entry.id)}
                busy={install.isPending}
                onInstall={(e) =>
                  install.mutate({ identifier: e.id, version: byID.get(e.id)?.version ?? 0 })
                }
              />
            ))}
          </div>
        </section>
      </div>
    </div>
  )
}
