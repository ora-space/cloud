import { MessageAuthor, useAgentSpeaker } from './message-author'
import { Brain, Wrench } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { ConversationMarkdown } from '@/features/conversation-content/markdown'
import type { SessionTool } from '@/features/session-data'
import type { ThreadItem } from './thread-entries'

const TOOL_STATUS: Record<string, string> = {
  pending: '等待中',
  in_progress: '进行中',
  completed: '已完成',
  failed: '失败',
}
const TOOL_KIND: Record<string, string> = {
  read: '读取',
  edit: '编辑',
  execute: '执行',
  search: '搜索',
  fetch: '获取',
  delete: '删除',
  move: '移动',
  think: '分析',
  other: '工具调用',
}

function ToolStep({ tool }: { tool: SessionTool }) {
  return (
    <details className="min-w-0 rounded-lg border bg-background">
      <summary className="flex cursor-pointer list-none items-center gap-2 px-3 py-2 text-sm focus-visible:outline-ring">
        <Wrench className="size-4 shrink-0 text-muted-foreground" />
        <span className="min-w-0 flex-1 break-words">
          {TOOL_KIND[tool.kind ?? ''] ?? '工具调用'}
          {tool.title ? ` · ${tool.title}` : ''}
        </span>
        <span className="shrink-0 text-xs text-muted-foreground">
          {TOOL_STATUS[tool.status ?? ''] ?? '状态未知'}
          {tool.durationMs === null ? ' · 耗时未知' : ` · ${tool.durationMs}ms`}
        </span>
      </summary>
      <div className="min-w-0 space-y-3 border-t p-3">
        {tool.rawInputPresent && (
          <div>
            <p className="text-xs text-muted-foreground">输入</p>
            <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words text-xs">
              {JSON.stringify(tool.rawInput, null, 2)}
            </pre>
          </div>
        )}
        {tool.output && (
          <div>
            <p className="text-xs text-muted-foreground">输出</p>
            <ConversationMarkdown text={tool.output} />
          </div>
        )}
        {tool.rawOutputPresent && (
          <details>
            <summary className="cursor-pointer text-xs text-muted-foreground">完整工具结果</summary>
            <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words text-xs">
              {JSON.stringify(tool.rawOutput, null, 2)}
            </pre>
          </details>
        )}
        {!tool.output && !tool.rawOutputPresent && (
          <p className="text-xs text-muted-foreground">尚无工具结果</p>
        )}
      </div>
    </details>
  )
}

/** One readable conversation record; infrastructure prompts remain available in a disclosure. */
export function ThreadRow({
  item,
  onQuote,
  userName = '用户',
  at,
}: {
  userName?: string
  at?: string | undefined
  item: ThreadItem
  onQuote?: ((text: string) => void) | undefined
}) {
  const agentName = useAgentSpeaker()
  switch (item.variant) {
    case 'prompt':
      return (
        <li>
          <details className="rounded-lg border border-dashed p-3 text-muted-foreground">
            <summary className="cursor-pointer text-xs">执行上下文</summary>
            <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words text-xs">
              {item.text}
            </pre>
          </details>
        </li>
      )
    case 'user':
      return (
        <li data-message-anchor className="rounded-lg bg-muted/40 px-3 py-3">
          <MessageAuthor name={userName} at={at} />
          <ConversationMarkdown text={item.text} />
          {item.status && <p className="mt-2 text-xs text-muted-foreground">{item.status}</p>}
          {onQuote && (
            <Button type="button" size="xs" variant="ghost" onClick={() => onQuote(item.text)}>
              引用回复
            </Button>
          )}
        </li>
      )
    case 'agent':
      return (
        <li data-message-anchor className="min-w-0 py-2">
          <MessageAuthor name={agentName} type="agent" at={at} />
          <ConversationMarkdown text={item.text} />
          {onQuote && (
            <Button type="button" size="xs" variant="ghost" onClick={() => onQuote(item.text)}>
              引用回复
            </Button>
          )}
        </li>
      )
    case 'thought':
      return (
        <li>
          <details open={item.live ?? false} className="rounded-lg bg-muted/30 px-3 py-2">
            <summary className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
              <Brain className="size-4" />
              思考与进展
            </summary>
            <div className="mt-2">
              <ConversationMarkdown text={item.text} />
            </div>
          </details>
        </li>
      )
    case 'tool':
      return (
        <li className="min-w-0">
          <ToolStep tool={item.tool} />
        </li>
      )
    case 'separator':
      return <li aria-hidden="true" className="border-t border-border/50" />
    default:
      return <li className="text-xs text-muted-foreground">{item.text}</li>
  }
}
