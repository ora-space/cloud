import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowUpRight, RefreshCw } from 'lucide-react'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { SessionRecords, SessionTimeline, ToolSummary } from '@/features/session-timeline'
import type { SessionSnapshot } from '@/features/session-data'
import { isForbiddenError, isUnauthorizedError } from '@/lib/api-client'
import { workspacePaths } from '@/lib/paths'
import { useSessionContent, type SessionRef } from './api'
import { useSessionMetrics } from './metrics-api'
import { sessionFailure, sessionStatuses } from './presentation'
import { SessionMetricsPanel } from './session-metrics-panel'
import { SessionOverview } from './session-overview'

const tabs = ['概览', '模型请求', '时间轴', '工具统计', '完整事件'] as const
type DetailTab = (typeof tabs)[number]

type SessionContent = ReturnType<typeof useSessionContent>

function contentError({ run, thread, archive }: SessionContent): unknown {
  if (run.error) return run.error
  if (isForbiddenError(thread.error) || isUnauthorizedError(thread.error)) return thread.error
  return run.data?.revision ? archive.error : thread.error
}

function contentPending({ run, thread, archive }: SessionContent): boolean {
  return run.isPending || (run.data?.revision ? archive.isPending : thread.isPending)
}

function waitingForThread({ run, thread }: SessionContent): boolean {
  return !run.data?.revision && thread.data?.pages[0] === null
}

function ContentState({ content }: { content: SessionContent }) {
  const error = contentError(content)
  if (error)
    return (
      <p
        role="alert"
        className="rounded-lg border border-destructive/30 p-4 text-sm text-destructive"
      >
        {sessionFailure(error)}
      </p>
    )
  if (content.run.data && content.run.data.executorType !== 'agent')
    return <p>首期仅支持 Agent 任务会话。</p>
  if (contentPending(content)) return <p role="status">正在读取会话数据…</p>
  if (waitingForThread(content)) return <p>会话尚未开始，点击刷新查看最新状态。</p>
  if (content.run.data?.revision && content.thread.isError)
    return (
      <p role="status" className="text-xs text-muted-foreground">
        归档已读取；模型元信息暂不可用，点击刷新重试。
      </p>
    )
  return null
}

function SourceSummary({ model }: { model: SessionSnapshot }) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
      <Badge variant="secondary">
        {model.source === 'archive' ? '完整归档 · 已校验' : 'Thread 快照 · 非完整归档'}
      </Badge>
      <span>
        {model.stats.rawRecordCount} 条原始记录 · {model.stats.recordCount} 条重放事件
      </span>
      {model.source === 'thread' && <span>当前快照可能缺少大记录详情；点击刷新更新。</span>}
    </div>
  )
}

function DetailBody({
  content,
  tab,
}: {
  content: ReturnType<typeof useSessionContent>
  tab: DetailTab
}) {
  const { model, run, thread } = content
  if (!model || !run.data || contentError(content) || run.data.executorType !== 'agent') return null
  if (waitingForThread(content)) return null
  return (
    <div className="space-y-5">
      <SourceSummary model={model} />
      {model.source === 'thread' && thread.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          disabled={thread.isFetching}
          onClick={() => void thread.fetchNextPage()}
        >
          加载更早事件
        </Button>
      )}
      {tab === '概览' && <SessionOverview model={model} run={run.data} />}
      {tab === '时间轴' && <SessionTimeline model={model} />}
      {tab === '工具统计' && <ToolSummary model={model} />}
      {tab === '完整事件' && <SessionRecords model={model} />}
    </div>
  )
}

function DetailContent({
  content,
  metrics,
  tab,
  accessError,
}: {
  content: SessionContent
  metrics: ReturnType<typeof useSessionMetrics>
  tab: DetailTab
  accessError: unknown
}) {
  if (accessError)
    return (
      <p role="alert" className="text-sm text-destructive">
        {sessionFailure(accessError)}
      </p>
    )
  if (tab === '模型请求') return <SessionMetricsPanel content={content} metrics={metrics} />
  return (
    <>
      <ContentState content={content} />
      <DetailBody content={content} tab={tab} />
    </>
  )
}

function AgentSessionDetail({
  scope,
  workspaceSlug,
}: {
  scope: SessionRef
  workspaceSlug: string
}) {
  const [tab, setTab] = useState<DetailTab>('概览')
  const content = useSessionContent(scope)
  const metrics = useSessionMetrics(
    scope,
    tab === '模型请求' && content.run.isSuccess && content.run.data.executorType === 'agent',
  )
  const paths = workspacePaths(workspaceSlug)
  const busy =
    content.run.isFetching ||
    content.thread.isFetching ||
    content.archive.isFetching ||
    metrics.statistics.isFetching ||
    metrics.calls.isFetching
  const accessError = [
    content.run.error,
    content.thread.error,
    content.archive.error,
    metrics.accessError,
  ].find((error) => isForbiddenError(error) || isUnauthorizedError(error))
  const refresh = async () => {
    if (await content.refresh()) {
      if (tab === '模型请求' || metrics.statistics.isFetched) await metrics.refresh()
    }
  }
  return (
    <>
      <PageHeader
        title="Agent 会话"
        breadcrumb={{ label: '会话分析', to: paths.sessions }}
        actions={
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void refresh()}>
            <RefreshCw className="size-4" />
            刷新
          </Button>
        }
      />
      <main className="space-y-5 overflow-auto p-4 md:p-6">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-2">
            <h2 className="text-xl font-semibold">会话详情</h2>
            <p className="break-all font-mono text-xs text-muted-foreground">{scope.runId}</p>
            {content.run.data && !contentError(content) && !accessError && (
              <Badge variant="secondary">{sessionStatuses[content.run.data.status]}</Badge>
            )}
          </div>
          <Link
            className="flex items-center gap-1 text-sm text-primary hover:underline"
            to={paths.issueDetail(scope.issueId)}
          >
            返回原任务
            <ArrowUpRight className="size-4" />
          </Link>
        </div>
        <div
          role="tablist"
          aria-label="会话分析视图"
          className="flex flex-wrap gap-1 border-b pb-2"
        >
          {tabs.map((name) => (
            <Button
              key={name}
              role="tab"
              aria-selected={tab === name}
              variant={tab === name ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => setTab(name)}
            >
              {name}
            </Button>
          ))}
        </div>
        <DetailContent content={content} metrics={metrics} tab={tab} accessError={accessError} />
      </main>
    </>
  )
}

/** A direct URL is scoped again by Cloud; switching runs resets transient view state. */
export function SessionDetailPage({ slug }: { slug: string }) {
  const {
    workspaceSlug = '',
    issueId,
    runId,
  } = useParams<{ workspaceSlug: string; issueId: string; runId: string }>()
  if (!issueId || !runId) return <p>会话链接不完整。</p>
  return (
    <AgentSessionDetail
      key={`${slug}:${runId}`}
      scope={{ tid: slug, issueId, runId }}
      workspaceSlug={workspaceSlug}
    />
  )
}
