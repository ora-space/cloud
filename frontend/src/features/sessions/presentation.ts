import { faultCode } from '@/lib/api-client'

/** Labels describe Cloud run state without inferring provider success from message text. */
export const sessionStatuses: Record<string, string> = {
  queued: '排队中',
  dispatched: '已分派',
  running: '运行中',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
  deferred: '已延后',
}

/** Display an instant in the user's locale; absent or invalid values stay unknown. */
export function sessionTime(value: string | null | undefined): string {
  if (!value || !Number.isFinite(Date.parse(value))) return '未知'
  return new Date(value).toLocaleString()
}

/** Only documented safe fault codes become user-facing diagnostics. */
export function sessionFailure(error: unknown): string {
  const messages: Record<string, string> = {
    membership_required: '当前账号没有此空间的访问权限。',
    forbidden: '当前账号没有此会话的访问权限。',
    not_found: '会话不存在或已不可访问。',
    invalid_session_filter: '筛选条件无效，请检查日期范围和项目。',
    invalid_pagination: '列表游标已失效，请刷新列表。',
    history_unavailable: '完整归档暂不可读取，请稍后重试。',
    history_invalid: '归档校验失败，无法展示完整日志。',
    history_too_large: '归档超过首期读取上限（32 MiB 或 100000 条记录）。',
  }
  return messages[faultCode(error) ?? ''] ?? '读取失败，请重试。'
}
