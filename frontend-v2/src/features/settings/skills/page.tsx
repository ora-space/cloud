import { useState } from 'react'
import { Link } from 'react-router'
import { toast } from 'sonner'
import { ArrowUpRight, Download, FileText, Plus, Sparkles } from 'lucide-react'
import type { PluginCatalogEntry, SpacePlugin } from '@/api/generated.schemas'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { useSettingsData } from '../data/hooks'
import type { Skill } from '../data/types'
import { deleteSkill, saveSkill } from '../data/resource-actions'
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
import { SkillForm } from './skill-form'

type Panel = { kind: 'create' } | { kind: 'detail' | 'edit'; id: string } | null
type SkillRow = Skill & {
  entry?: PluginCatalogEntry
  selection?: SpacePlugin
}
function skillStatus(skill: SkillRow, detail = false) {
  if (skill.availability === 'available') return '可用'
  if (skill.selection?.observedState === 'removing') return '移除中'
  if (skill.selection?.observedState === 'failed')
    return skill.selection.desiredState === 'removed' ? '移除失败' : '安装失败'
  if (skill.selection?.desiredState === 'installed') return '等待节点'
  return detail ? '不可用，来源插件未安装' : '来源插件未安装'
}

type Removing =
  | { kind: 'local'; skill: Skill }
  | { kind: 'plugin'; skill: SkillRow }

function pluginSkillName(entry: PluginCatalogEntry): string {
  const heading = entry.readme?.match(/^#\s+(.+)$/m)?.[1]?.trim()
  return heading || entry.identifier
}

function pluginSkillRows(
  catalog: PluginCatalogEntry[],
  selection: (entry: PluginCatalogEntry) => SpacePlugin | undefined,
): SkillRow[] {
  return catalog
    .filter((entry) => entry.kind === 'skill')
    .map((entry) => {
      const chosen = selection(entry)
      return {
        id: `plugin-skill-${pluginKey(entry)}`,
        namespace: entry.sourceNamespace,
        name: pluginSkillName(entry),
        description: entry.description,
        content: entry.readme ?? '',
        source: { kind: 'plugin', pluginId: pluginKey(entry) },
        availability: pluginInstalled(chosen) ? 'available' : 'unavailable',
        entry,
        selection: chosen,
      }
    })
}

export default function SkillsPage() {
  const local = useSettingsData()
  const market = usePluginMarketplace()
  const [search, setSearch] = useState('')
  const [source, setSource] = useState('all')
  const [status, setStatus] = useState('all')
  const [order, setOrder] = useState('asc')
  const [panel, setPanel] = useState<Panel>(null)
  const [removing, setRemoving] = useState<Removing>()
  const skills: SkillRow[] = [
    ...(local.query.data?.skills ?? []),
    ...pluginSkillRows(market.catalog, market.selection),
  ]
  const selected =
    panel && 'id' in panel
      ? skills.find((skill) => skill.id === panel.id)
      : undefined
  const filtered = skills
    .filter(
      (skill) =>
        `${skill.name} ${skill.description} ${skill.namespace}`
          .toLowerCase()
          .includes(search.trim().toLowerCase()) &&
        (source === 'all' || skill.source.kind === source) &&
        (status === 'all' || skill.availability === status),
    )
    .sort(
      (a, b) =>
        (order === 'asc' ? 1 : -1) * a.name.localeCompare(b.name, 'zh-CN'),
    )
  const localSkills = skills.filter((skill) => skill.source.kind === 'local')
  const clear = () => {
    setSearch('')
    setSource('all')
    setStatus('all')
  }
  const open = (next: Panel) => {
    local.mutation.reset()
    setPanel(next)
  }
  const create = () => open({ kind: 'create' })
  const install = (skill: SkillRow) => {
    if (!skill.entry) return
    market.resetErrors()
    void market
      .install(skill.entry)
      .then(() => toast.success(`已选择技能 ${skill.name}`))
      .catch(() => undefined)
  }
  const requestPluginRemove = (skill: SkillRow) => {
    market.resetErrors()
    if (skill.selection) setRemoving({ kind: 'plugin', skill })
  }
  const confirmPluginRemove = () => {
    if (removing?.kind !== 'plugin' || !removing.skill.selection) return
    void market
      .remove(removing.skill.selection)
      .then(() => {
        toast.success(`已提交移除技能 ${removing.skill.name}`)
        setRemoving(undefined)
      })
      .catch(() => undefined)
  }
  function pluginActions(skill: SkillRow) {
    if (!skill.entry || !market.canManage) return null
    if (skill.selection?.observedState === 'removing')
      return (
        <span className="text-xs text-muted-foreground">
          正在等待项目节点确认
        </span>
      )
    if (
      skill.selection?.desiredState === 'removed' &&
      skill.selection.observedState === 'failed'
    )
      return (
        <Button
          size="sm"
          variant="outline"
          disabled={market.mutating}
          onClick={() => requestPluginRemove(skill)}
        >
          重试移除
        </Button>
      )
    return skill.selection?.desiredState === 'installed' ? (
      <Button
        variant="outline"
        size="sm"
        disabled={market.mutating}
        onClick={() => requestPluginRemove(skill)}
      >
        移除技能
      </Button>
    ) : (
      <Button
        variant="secondary"
        size="sm"
        disabled={market.mutating}
        onClick={() => install(skill)}
      >
        <Download data-icon="inline-start" />
        安装技能
      </Button>
    )
  }
  return (
    <section>
      <PageHeader
        title="Skills"
        description="管理能复用的说明文档；市场技能随来源插件安装，自建技能仍是本地演示。"
        count={skills.length}
        actions={
          <Button
            disabled={!local.query.data || local.query.isError}
            onClick={create}
          >
            <Plus data-icon="inline-start" />
            新建技能
          </Button>
        }
      />
      <DataState
        pending={local.query.isPending || market.pending}
        error={local.query.error ?? market.error}
        retry={() => {
          void local.query.refetch()
          void market.refetch()
        }}
      >
        <div className="settings-toolbar">
          {market.spaces.length > 1 && (
            <Filter
              label="空间"
              value={market.spaceId}
              onChange={(id) => {
                market.chooseSpace(id)
                setPanel(null)
                setRemoving(undefined)
              }}
              options={market.spaces.map((s) => [s.id, s.name] as const)}
            />
          )}
          <SearchInput label="搜索技能" value={search} onChange={setSearch} />
          <Filter
            label="技能来源"
            value={source}
            onChange={setSource}
            options={[
              ['all', '全部来源'],
              ['local', '自建技能'],
              ['plugin', '市场技能'],
            ]}
          />
          <Filter
            label="技能状态"
            value={status}
            onChange={setStatus}
            options={[
              ['all', '全部状态'],
              ['available', '可用'],
              ['unavailable', '不可用'],
            ]}
          />
          <Filter
            label="技能排序"
            value={order}
            onChange={setOrder}
            options={[
              ['asc', '名称 A–Z'],
              ['desc', '名称 Z–A'],
            ]}
          />
        </div>
        <ActionError error={local.mutation.error ?? market.mutationError} />
        <div className="settings-resource-list">
          {filtered.map((skill) => (
            <article
              key={skill.id}
              aria-label={skill.name}
              className="settings-resource-row"
            >
              <button
                type="button"
                className="settings-resource-main"
                aria-label={`查看技能 ${skill.name}`}
                onClick={() => open({ kind: 'detail', id: skill.id })}
              >
                <span className="settings-resource-icon">
                  {skill.source.kind === 'local' ? (
                    <FileText className="size-5" />
                  ) : (
                    <Sparkles className="size-5" />
                  )}
                </span>
                <span className="min-w-0">
                  <span className="settings-resource-title">{skill.name}</span>
                  <span className="settings-resource-description block">
                    {skill.description || '暂无简介'}
                  </span>
                  <span className="settings-resource-meta">
                    <span>
                      {skill.source.kind === 'local'
                        ? '自建技能 · 本机演示'
                        : '市场技能 · 只读'}
                    </span>
                    <span aria-hidden="true">·</span>
                    <span>{skill.namespace}</span>
                  </span>
                </span>
              </button>
              <div className="settings-resource-actions">
                <Badge
                  variant={
                    skill.availability === 'available' ? 'secondary' : 'outline'
                  }
                >
                  {skillStatus(skill)}
                </Badge>
                {pluginActions(skill)}
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => open({ kind: 'detail', id: skill.id })}
                >
                  查看详情
                  <ArrowUpRight data-icon="inline-end" />
                </Button>
              </div>
            </article>
          ))}
        </div>
        {!filtered.length && (
          <NoResults filtered={skills.length > 0} onClear={clear}>
            <Button onClick={create}>新建技能</Button>
          </NoResults>
        )}
        <div className="settings-list-footer">
          <span>
            显示 {filtered.length} / {skills.length} 个技能 · 共{' '}
            {localSkills.length} 个自建
          </span>
          <span>市场技能跟随插件安装；自建说明不会执行工具</span>
        </div>
      </DataState>
      {panel?.kind === 'create' && (
        <DetailPanel
          title="新建技能"
          description="写下清晰的说明，让相同工作有一致的方法。"
          onClose={() => setPanel(null)}
        >
          <SkillForm
            pending={local.mutation.isPending}
            error={local.mutation.error}
            onCancel={() => setPanel(null)}
            onSave={(draft) =>
              local.save(
                (data) => saveSkill(data, draft),
                '演示技能已保存',
                () => setPanel(null),
              )
            }
          />
        </DetailPanel>
      )}
      {selected && panel?.kind === 'edit' && (
        <DetailPanel
          title={`编辑 ${selected.name}`}
          description="修改只保存在此浏览器的演示空间。"
          onClose={() => setPanel(null)}
        >
          <SkillForm
            key={selected.id}
            skill={selected}
            pending={local.mutation.isPending}
            error={local.mutation.error}
            onCancel={() => open({ kind: 'detail', id: selected.id })}
            onSave={(draft) =>
              local.save(
                (data) => saveSkill(data, draft, selected.id),
                '演示技能已保存',
                () => setPanel({ kind: 'detail', id: selected.id }),
              )
            }
          />
        </DetailPanel>
      )}
      {selected && panel?.kind === 'detail' && (
        <DetailPanel
          title={selected.name}
          description={selected.description || '技能说明文档'}
          onClose={() => setPanel(null)}
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <Badge variant="secondary">
              {selected.source.kind === 'local'
                ? '自建技能 · 本机演示'
                : '市场技能 · 只读'}
            </Badge>
            {selected.source.kind === 'local' ? (
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  onClick={() => open({ kind: 'edit', id: selected.id })}
                >
                  编辑技能
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => {
                    local.mutation.reset()
                    setRemoving({ kind: 'local', skill: selected })
                  }}
                >
                  删除技能
                </Button>
              </div>
            ) : (
              pluginActions(selected)
            )}
          </div>
          <dl className="settings-facts">
            <dt>命名空间</dt>
            <dd>{selected.namespace}</dd>
            <dt>可用状态</dt>
            <dd>{skillStatus(selected, true)}</dd>
            <dt>来源</dt>
            <dd>
              {selected.source.kind === 'local' ? (
                '当前用户的本机演示空间'
              ) : (
                <Link
                  className="underline underline-offset-4"
                  to={`/settings/plugins?plugin=${encodeURIComponent(selected.source.pluginId)}`}
                >
                  {selected.source.pluginId} ↗
                </Link>
              )}
            </dd>
          </dl>
          {selected.availability === 'unavailable' && (
            <p className="rounded-lg bg-muted p-3 text-sm text-muted-foreground">
              说明仍可预览。安装来源插件后，此技能会恢复可用。
            </p>
          )}
          <Separator />
          <MarkdownPreview content={selected.content} />
        </DetailPanel>
      )}
      {removing?.kind === 'local' && (
        <ConfirmDialog
          title={`删除 ${removing.skill.name}？`}
          description="将删除此浏览器中的自建技能和说明文档。此操作不影响市场技能或真实会话。"
          confirmLabel="删除技能"
          pending={local.mutation.isPending}
          error={local.mutation.error}
          onClose={() => setRemoving(undefined)}
          onConfirm={() =>
            local.save(
              (data) => deleteSkill(data, removing.skill.id),
              '演示技能已删除',
              () => {
                setRemoving(undefined)
                setPanel(null)
              },
            )
          }
        />
      )}
      {removing?.kind === 'plugin' && (
        <ConfirmDialog
          title={`移除技能 ${removing.skill.name}？`}
          description="将从空间卸载来源技能包。技能说明仍可预览，但会显示为不可用；重新安装可恢复。"
          confirmLabel="移除技能"
          pending={market.mutating}
          error={market.mutationError}
          onClose={() => setRemoving(undefined)}
          onConfirm={confirmPluginRemove}
        />
      )}
    </section>
  )
}
