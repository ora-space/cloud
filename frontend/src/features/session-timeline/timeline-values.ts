import type { SessionEvent, SessionTimelineEvent } from '@/features/session-data'
import type { TimeWindow } from './use-time-brush'

/** User-facing names retain every record category, including unsupported and echoed input. */
export const categoryLabels: Record<SessionEvent['category'], string> = {
  user: '用户输入',
  user_echo: '输入回声',
  assistant: '模型文本',
  thought: '思考',
  tool: '工具',
  metadata: '元数据',
  lifecycle: '生命周期',
  gap: '记录缺口',
  unknown: '未知记录',
}

/** The original dashboard's three primary lanes; auxiliary records stay in the records view. */
export const lanes: readonly { kind: SessionTimelineEvent['kind']; label: string }[] = [
  { kind: 'user', label: '用户输入' },
  { kind: 'llm', label: '模型文本' },
  { kind: 'tool', label: '工具' },
]

/** Formats a measured duration without turning missing or invalid evidence into zero. */
export function durationLabel(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return '未知'
  if (value < 1000) return `${value.toFixed(0)}ms`
  if (value < 60000) return `${(value / 1000).toFixed(1)}s`
  return `${Math.floor(value / 60000)}m ${((value % 60000) / 1000).toFixed(1)}s`
}

/** Displays recorded wall-clock time in the viewer's locale; missing time remains explicit. */
export function clockLabel(
  value: number | null,
  precision: 'seconds' | 'milliseconds' = 'seconds',
): string {
  if (value === null || !Number.isFinite(value)) return '时间未知'
  return new Date(value).toLocaleTimeString('zh-CN', {
    hour12: false,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    fractionalSecondDigits: precision === 'milliseconds' ? 3 : undefined,
  })
}

/** Retains unknown starts in lists and includes measured tool intervals crossing a time window. */
export function intersectsWindow(event: SessionTimelineEvent, window: TimeWindow): boolean {
  if (event.startMs === null) return true
  return event.startMs <= window.endMs && event.startMs + (event.durationMs ?? 0) >= window.startMs
}

/** Keeps structured values readable and distinguishes absent evidence from an explicit null. */
export function jsonText(value: unknown): string {
  if (value === undefined) return '未记录'
  return JSON.stringify(value, null, 2) ?? '未记录'
}

const toolActions = new Map([
  ['read', '读取'],
  ['edit', '编辑'],
  ['delete', '删除'],
  ['move', '移动'],
  ['search', '搜索'],
  ['execute', '执行命令'],
  ['think', '思考'],
  ['fetch', '获取内容'],
  ['switch_mode', '切换模式'],
])

/** Labels tool operations by reported kind; their title may name a target, not a tool. */
export function eventTitle(event: SessionEvent): string {
  if (event.tool) {
    const action = toolActions.get(event.tool.kind ?? '') ?? '工具调用'
    const title = event.tool.title?.trim()
    return title && title.toLowerCase() !== event.tool.kind?.toLowerCase()
      ? `${action} · ${title}`
      : action
  }
  const firstLine = event.text?.trim().split('\n')[0]
  if (firstLine) return firstLine.slice(0, 140)
  const type = event.record['type']
  return typeof type === 'string' ? type : categoryLabels[event.category]
}

/** Search covers retained record content, IDs and normalized text, including unknown records. */
export function matchesSearch(event: SessionEvent, query: string): boolean {
  const needle = query.trim().toLocaleLowerCase()
  if (!needle) return true
  return `${event.id}\n${event.text}\n${jsonText(event.raw)}`.toLocaleLowerCase().includes(needle)
}

/** Keeps tool colors stable across filtering and polling; error state is a separate red border. */
export function eventColor(event: SessionEvent): string {
  if (event.category === 'user') return '#64748b'
  if (event.category === 'assistant') return '#2563eb'
  if (!event.tool) return '#a855f7'
  const colors = ['#16a34a', '#0d9488', '#7c3aed', '#d97706', '#0891b2']
  const name = event.tool.kind ?? 'tool'
  const hash = Array.from(name).reduce((sum, char) => sum + char.charCodeAt(0), 0)
  return colors[hash % colors.length] ?? '#16a34a'
}

/** An adjacent observed gap is not a model latency; reordered timestamps yield no invented zero. */
export function adjacentGap(
  event: SessionEvent,
  previous: SessionEvent | undefined,
): number | null {
  if (event.timestampMs === null || previous?.timestampMs === null || !previous) return null
  const elapsed = event.timestampMs - previous.timestampMs
  return elapsed >= 0 ? elapsed : null
}
