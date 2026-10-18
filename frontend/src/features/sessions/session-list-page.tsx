import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Activity, ArrowUpRight, RefreshCw } from 'lucide-react'
import type {
  AgentSessionSummary,
  GetApiV1TenantsTidAgentSessionsParams,
} from '@/api/generated.schemas'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { workspacePaths } from '@/lib/paths'
import { useSessionList } from './api'
import { SessionFilters } from './session-filters'
import { sessionFailure, sessionStatuses, sessionTime } from './presentation'

function SessionRow({ session, slug }: { session: AgentSessionSummary; slug: string }) {
  const path = workspacePaths(slug)
  return (
    <article className="grid gap-3 rounded-xl border bg-card p-4 md:grid-cols-[1fr_1fr_auto]">
      <div className="min-w-0 space-y-1">
        <Link
          className="font-medium hover:underline"
          to={path.agentSession(session.issueId, session.id)}
        >
          #{session.issueNumber} · {session.issueTitle}
        </Link>
        <p className="truncate text-xs text-muted-foreground">
          {session.projectName ?? '未关联项目'} · {session.agentPluginId ?? 'Agent 插件未知'}
        </p>
        <p className="font-mono text-[11px] text-muted-foreground">{session.id}</p>
      </div>
      <div className="space-y-1 text-xs text-muted-foreground">
        <p>创建于 {sessionTime(session.createdAt)}</p>
        <p className="break-all">模型：{session.model?.modelName ?? '未知'}</p>
        <p>{session.historyAvailable ? '完整归档已登记' : 'Thread 快照'}</p>
      </div>
      <div className="flex items-center gap-4">
        <Badge variant={session.status === 'failed' ? 'destructive' : 'secondary'}>
          {sessionStatuses[session.status]}
        </Badge>
        <Link
          aria-label={`分析会话 ${session.id}`}
          to={path.agentSession(session.issueId, session.id)}
          className="flex items-center gap-1 text-sm text-primary"
        >
          分析
          <ArrowUpRight className="size-4" />
        </Link>
      </div>
    </article>
  )
}

function TenantSessions({ slug }: { slug: string }) {
  const { workspaceSlug = '' } = useParams<{ workspaceSlug: string }>()
  const [filters, setFilters] = useState<GetApiV1TenantsTidAgentSessionsParams>({})
  const query = useSessionList(slug, filters)
  const sessions = query.isError ? [] : (query.data?.pages.flatMap((page) => page.items) ?? [])
  return (
    <>
      <PageHeader
        title="会话分析"
        actions={
          <Button
            variant="ghost"
            size="sm"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            <RefreshCw className="size-4" />
            刷新
          </Button>
        }
      />
      <main className="space-y-5 overflow-auto p-4 md:p-6">
        <div className="space-y-2">
          <h2 className="flex items-center gap-2 text-xl font-semibold">
            <Activity className="size-5 text-primary" />
            会话分析
          </h2>
          <p className="text-sm text-muted-foreground">
            查看当前空间的执行过程、工具记录与完整事件。
          </p>
          <Badge variant="secondary">Agent 任务会话</Badge>
          <p className="text-xs text-muted-foreground">
            首期支持 Cloud Agent 任务会话；聊天、工作流和小队会话将在后续接入。
          </p>
        </div>
        <SessionFilters tid={slug} onApply={setFilters} />
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span>已加载 {sessions.length} 个会话</span>
          <span>按创建时间从新到旧</span>
        </div>
        {query.isPending && <p role="status">正在读取会话…</p>}
        {query.isError && (
          <p role="alert" className="text-sm text-destructive">
            {sessionFailure(query.error)}
          </p>
        )}
        {query.isSuccess && sessions.length === 0 && (
          <p className="rounded-xl border border-dashed p-10 text-center text-muted-foreground">
            没有符合条件的会话。
          </p>
        )}
        <div className="space-y-3">
          {sessions.map((session) => (
            <SessionRow key={session.id} session={session} slug={workspaceSlug} />
          ))}
        </div>
        {query.hasNextPage && !query.isError && (
          <Button
            variant="outline"
            disabled={query.isFetching}
            onClick={() => void query.fetchNextPage()}
          >
            加载更多会话
          </Button>
        )}
      </main>
    </>
  )
}

/** Unified workspace entry; a tenant change resets filters and local form state. */
export function SessionListPage({ slug }: { slug: string }) {
  return <TenantSessions key={slug} slug={slug} />
}
