import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { ScheduledTask, ScheduledTaskSchedule } from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidScheduledTasksStid,
  getApiV1TenantsTidScheduledTasks,
  getApiV1TenantsTidScheduledTasksStidHistory,
  getGetApiV1TenantsTidScheduledTasksQueryKey,
  getGetApiV1TenantsTidScheduledTasksStidHistoryQueryKey,
  patchApiV1TenantsTidScheduledTasksStid,
  postApiV1TenantsTidScheduledTasks,
  postApiV1TenantsTidScheduledTasksStidDisable,
  postApiV1TenantsTidScheduledTasksStidEnable,
  postApiV1TenantsTidScheduledTasksStidRevokeConsent,
} from '@/api/scheduled-tasks/scheduled-tasks'
import {
  getApiV1TenantsTidCollaborationTargets,
  getApiV1TenantsTidIssues,
  getApiV1TenantsTidIssuesIidRunsRidArtifacts,
  getGetApiV1TenantsTidIssuesIidRunsRidArtifactsQueryKey,
} from '@/api/tenants/tenants'
import { faultCode } from '@/lib/api-client'

/** One tenant-scoped definition; both the list and the create/update return this resource. */
export type { ScheduledTask, ScheduledTaskSchedule }

/** Fields the create form collects; `enabled` and the consent are the caller's toggles. */
export type ScheduledTaskDraft = {
  issueId: string
  agentId: string
  name: string
  prompt: string
  model: string
  schedule: ScheduledTaskSchedule
  timezone: string
  enabled: boolean
}

/** Lists every non-deleted definition. The list is not paginated: the backend returns all rows. */
export function useScheduledTasks(tid: string | undefined) {
  return useQuery({
    queryKey: getGetApiV1TenantsTidScheduledTasksQueryKey(tid ?? ''),
    queryFn: ({ signal }) => getApiV1TenantsTidScheduledTasks(tid ?? '', undefined, signal),
    enabled: tid != null,
  })
}

/** Creates a new definition or replaces the editable fields of an existing one. */
export function useSaveScheduledTask(tid: string, existing: ScheduledTask | undefined) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (draft: ScheduledTaskDraft) => {
      const base = {
        name: draft.name,
        prompt: draft.prompt,
        agentId: draft.agentId,
        model: draft.model,
        schedule: draft.schedule,
        timezone: draft.timezone,
      }
      const result = existing
        ? await patchApiV1TenantsTidScheduledTasksStid(tid, existing.id, {
            ...base,
            version: existing.version,
          })
        : await postApiV1TenantsTidScheduledTasks(tid, {
            ...base,
            issueId: draft.issueId,
            enabled: draft.enabled,
            ...(draft.enabled ? { approval: { policy: 'full_auto_in_cloud_workspace' } } : {}),
          })
      return result.resource
    },
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: getGetApiV1TenantsTidScheduledTasksQueryKey(tid) }),
  })
}

/** Soft-deletes a definition; its history and any live run are untouched. */
export function useDeleteScheduledTask(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (taskId: string) => deleteApiV1TenantsTidScheduledTasksStid(tid, taskId, {}),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: getGetApiV1TenantsTidScheduledTasksQueryKey(tid) }),
  })
}

/** Turns a disabled task on or off; enable re-runs the save-time eligibility table. */
export function useSetScheduledTaskEnabled(tid: string, task: ScheduledTask) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ enabled, consented }: { enabled: boolean; consented?: boolean }) =>
      enabled
        ? postApiV1TenantsTidScheduledTasksStidEnable(
            tid,
            task.id,
            consented ? { approval: { policy: 'full_auto_in_cloud_workspace' } } : {},
          )
        : postApiV1TenantsTidScheduledTasksStidDisable(tid, task.id, {}),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: getGetApiV1TenantsTidScheduledTasksQueryKey(tid) }),
  })
}

/** Clears the unattended consent and disables the task; re-enabling requires a fresh consent. */
export function useRevokeScheduledTaskConsent(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (taskId: string) =>
      postApiV1TenantsTidScheduledTasksStidRevokeConsent(tid, taskId, {}),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: getGetApiV1TenantsTidScheduledTasksQueryKey(tid) }),
  })
}

/** Newest-first occurrence records for one task. */
export function useScheduledTaskHistory(tid: string, taskId: string | undefined) {
  return useQuery({
    queryKey: getGetApiV1TenantsTidScheduledTasksStidHistoryQueryKey(tid, taskId ?? ''),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidScheduledTasksStidHistory(tid, taskId ?? '', undefined, signal),
    enabled: taskId != null,
  })
}

/** Every collaboration issue, for the create form's binding picker. */
export function useScheduledTaskIssues(tid: string | undefined) {
  return useQuery({
    queryKey: ['scheduled-task-issues', tid],
    queryFn: async ({ signal }) => {
      const page = await getApiV1TenantsTidIssues(tid ?? '', undefined, signal)
      return page.items
    },
    enabled: tid != null,
  })
}

/** Every collaboration agent, for the create/update form's target picker. */
export function useScheduledTaskAgents(tid: string | undefined) {
  return useQuery({
    queryKey: ['scheduled-task-agents', tid],
    queryFn: async ({ signal }) => {
      const page = await getApiV1TenantsTidCollaborationTargets(tid ?? '', undefined, signal)
      return page.items.filter((target) => target.type === 'agent')
    },
    enabled: tid != null,
  })
}

const DAY_NAMES: Record<string, string> = {
  sunday: '周日',
  monday: '周一',
  tuesday: '周二',
  wednesday: '周三',
  thursday: '周四',
  friday: '周五',
  saturday: '周六',
}

/** Renders a schedule in the task's own words, for a list row or history header. */
export function scheduleSummary(schedule: ScheduledTaskSchedule): string {
  const clock = `${String(schedule.hour ?? 0).padStart(2, '0')}:${String(schedule.minute ?? 0).padStart(2, '0')}`
  switch (schedule.kind) {
    case 'every_hours':
      return `每 ${schedule.intervalHours ?? 0} 小时`
    case 'daily':
      return `每天 ${clock}`
    case 'weekly':
      return `每周 ${(schedule.weekdays ?? []).map((day) => DAY_NAMES[day] ?? day).join('、')} ${clock}`
    default:
      return clock
  }
}

/**
 * Renders the server-projected next due point in the task's own timezone ("2026-10-28 09:00").
 * The value is display-only and recomputed by Cloud on every read; an unknown zone falls back to
 * the viewer's locale rather than hiding the time.
 */
export function nextDueSummary(nextDueAt: string, timezone: string): string {
  const at = new Date(nextDueAt)
  const options: Intl.DateTimeFormatOptions = {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  }
  let parts: Intl.DateTimeFormatPart[]
  try {
    parts = new Intl.DateTimeFormat('en-CA', { ...options, timeZone: timezone }).formatToParts(at)
  } catch {
    return at.toLocaleString()
  }
  const part = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((p) => p.type === type)?.value ?? ''
  return `${part('year')}-${part('month')}-${part('day')} ${part('hour')}:${part('minute')}`
}

const FAULT_MESSAGES: Record<string, string> = {
  artifact_unavailable: '产物已不可用，可能已过期或被清理',
  artifact_invalid: '产物下载地址不可用，请重试',
  object_store_unavailable: '产物存储暂不可用，请稍后重试',
  invalid_request: '请检查名称、时间、时区与计划填写',
  not_found: '任务或绑定对象不存在',
  target_unavailable: '目标智能体或创建者资格当前不可用',
  model_unavailable: '请先为创建者配置可用的模型连接',
  approval_policy_invalid: '无人值守授权已失效，请重新确认',
  scheduled_task_acceptance_pending: '定时任务正在进行端到端验收，暂不可保存或启用',
  version_conflict: '任务已在别处修改，请刷新后重试',
}

/** Maps public fault codes to bounded messages without rendering upstream error details. */
export function scheduledTaskFailure(error: unknown): string {
  const code = faultCode(error) ?? (error instanceof Error ? error.message : '')
  return FAULT_MESSAGES[code] ?? '操作失败，请稍后重试'
}

/** Reads registered metadata only; opening a list never grants object access. */
export function useScheduledRunArtifacts(tid: string, issueId: string, runId: string) {
  return useQuery({
    queryKey: getGetApiV1TenantsTidIssuesIidRunsRidArtifactsQueryKey(tid, issueId, runId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRidArtifacts(tid, issueId, runId, undefined, signal),
  })
}

/** Builds a same-origin attachment route; Cloud checks current access on every click. */
export function scheduledArtifactDownloadPath(
  tid: string,
  issueId: string,
  runId: string,
  index: number,
) {
  return `/api/v1/tenants/${encodeURIComponent(tid)}/issues/${encodeURIComponent(issueId)}/runs/${encodeURIComponent(runId)}/artifacts/${index}`
}
