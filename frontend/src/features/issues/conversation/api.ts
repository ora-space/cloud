import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useRef } from 'react'
import type { TaskConversationMessage } from '@/api/generated.schemas'
import {
  getApiV1TenantsTidIssuesIidConversation,
  postApiV1TenantsTidIssuesIidConversationMessages,
  postApiV1TenantsTidIssuesIidConversationMessagesMidCancel,
  postApiV1TenantsTidIssuesIidConversationMessagesMidRetry,
  postApiV1TenantsTidIssuesIidConversationDiscard,
  postApiV1TenantsTidIssuesIidConversationResume,
  postApiV1TenantsTidIssuesIidConversationStop,
} from '@/api/tenants/tenants'
import { faultCode, isForbiddenError, isUnauthorizedError } from '@/lib/api-client'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'

const PAGE_SIZE = 200

/** Cache identity of one task's durable Agent-directed messages. */
export function taskConversationKey(tid: string, issueId: string) {
  return ['task-conversation', tid, issueId] as const
}

/** Reads task history and refreshes a selected personal Agent's gap until an external repair lands. */
export function useTaskConversation(tid: string, issueId: string, selectedAgentId?: string) {
  return useInfiniteQuery({
    queryKey: taskConversationKey(tid, issueId),
    initialPageParam: undefined as number | undefined,
    queryFn: ({ pageParam, signal }) =>
      getApiV1TenantsTidIssuesIidConversation(
        tid,
        issueId,
        { limit: PAGE_SIZE, ...(pageParam === undefined ? {} : { before: pageParam }) },
        undefined,
        signal,
      ),
    getNextPageParam: (page) =>
      page.prevCursor !== null && page.prevCursor > 1 ? page.prevCursor : undefined,
    refetchInterval: (query) => {
      if (isUnauthorizedError(query.state.error) || isForbiddenError(query.state.error))
        return false
      return query.state.data?.pages.some(
        (page) =>
          !!page.activeRunId ||
          page.items.some((message) => message.state === 'waiting') ||
          page.agentReadiness?.some(
            (agent) =>
              agent.agentId === selectedAgentId && agent.requiresPersonalModel && !!agent.errorCode,
          ),
      )
        ? 5000
        : false
    },
  })
}

/** Joins loaded pages by stable message identity; a refetch may return overlapping windows. */
export function loadedTaskMessages(
  pages: readonly { items: readonly TaskConversationMessage[] }[] | undefined,
): TaskConversationMessage[] {
  const byID = new Map<string, TaskConversationMessage>()
  for (const page of pages ?? []) for (const message of page.items) byID.set(message.id, message)
  return [...byID.values()].toSorted((a, b) => a.seq - b.seq)
}

function invalidateTask(
  queryClient: ReturnType<typeof useQueryClient>,
  tid: string,
  issueId: string,
) {
  void queryClient.invalidateQueries({ queryKey: taskConversationKey(tid, issueId) })
  void queryClient.invalidateQueries({ queryKey: ['issue-runs', tid, issueId] })
  void queryClient.invalidateQueries({ queryKey: ['issue-timeline', tid, issueId] })
}

/** Posts an explicitly addressed message with a stable idempotency key across request retries. */
export function useSendTaskMessage(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  const pending = useRef<{ scope: string; input: { agentId: string; body: string } } | null>(null)
  const scope = `${tid}:${issueId}`
  return useMutation({
    mutationFn: (input: { agentId: string; body: string }) => {
      const prior = pending.current
      if (
        !prior ||
        prior.scope !== scope ||
        prior.input.agentId !== input.agentId ||
        prior.input.body !== input.body
      )
        pending.current = { scope, input }
      // A user retry after an unknown network outcome is the same intent. A successful send
      // clears it so an intentionally repeated message receives a fresh identity.
      return postApiV1TenantsTidIssuesIidConversationMessages(tid, issueId, input, {
        headers: mutationHeaders(keyFor(pending.current?.input ?? input)),
      })
    },
    onSuccess: (_result, input) => {
      if (
        pending.current?.scope === scope &&
        pending.current.input.agentId === input.agentId &&
        pending.current.input.body === input.body
      )
        pending.current = null
      invalidateTask(queryClient, tid, issueId)
    },
  })
}

/** Cancels or retries only the author's still-undelivered message. */
export function useUpdateTaskMessage(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: (input: { id: string; action: 'cancel' | 'retry' }) =>
      input.action === 'cancel'
        ? postApiV1TenantsTidIssuesIidConversationMessagesMidCancel(
            tid,
            issueId,
            input.id,
            {},
            { headers: mutationHeaders(keyFor(input)) },
          )
        : postApiV1TenantsTidIssuesIidConversationMessagesMidRetry(
            tid,
            issueId,
            input.id,
            {},
            { headers: mutationHeaders(keyFor(input)) },
          ),
    onSuccess: () => invalidateTask(queryClient, tid, issueId),
  })
}

/** Acknowledges a failed/cancelled run only after Cloud proves its saved result and release. */
export function useResumeTaskConversation(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: (input: { runId: string }) =>
      postApiV1TenantsTidIssuesIidConversationResume(tid, issueId, input, {
        headers: mutationHeaders(keyFor(input)),
      }),
    onSuccess: () => invalidateTask(queryClient, tid, issueId),
  })
}

/** Explicitly resumes from the last saved Revision after an undelivered result is abandoned. */
export function useDiscardUndeliveredTaskRun(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: (input: { runId: string }) =>
      postApiV1TenantsTidIssuesIidConversationDiscard(
        tid,
        issueId,
        { runId: input.runId, acknowledgeLostWork: true },
        { headers: mutationHeaders(keyFor(input)) },
      ),
    onSuccess: () => invalidateTask(queryClient, tid, issueId),
  })
}

/** Stops the current execution and persists a task pause without dropping later messages. */
export function useStopTaskConversation(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: (input: { runId: string }) =>
      postApiV1TenantsTidIssuesIidConversationStop(tid, issueId, input, {
        headers: mutationHeaders(keyFor(input)),
      }),
    onSuccess: () => invalidateTask(queryClient, tid, issueId),
  })
}

const MESSAGE_FAULTS: Record<string, string> = {
  agent_declaration_required: '这个插件版本尚未提供个人配置，请联系空间管理员升级插件。',
  agent_declaration_invalid: '这个插件版本的配置声明不可用，请联系空间管理员。',
  agent_configuration_required: '请在插件的“我的配置”中选择个人模型连接和模型。',
  agent_configuration_repair_required: '插件配置要求已变化，请在“我的配置”中确认修复。',
  agent_model_protocol_mismatch: '模型连接协议与此插件不匹配，请在“我的配置”中选择兼容连接。',
  agent_release_conflict: '空间选用的插件版本已变化，请刷新并核对插件配置。',
  agent_adapter_unavailable: '当前服务尚未启用此插件的模型接入能力，请联系部署管理员。',
  agent_target_unavailable: '当前插件没有适合此运行环境的安装包，请联系空间管理员。',
  agent_plugin_unavailable: '运行所需的插件尚未安装就绪，请检查插件安装状态。',
  model_not_found: '所选模型已不可用，请重新选择',
  content_too_large: '消息过长，请缩短后重试',
  invalid_target: '请选择可用的 Agent',
  issue_project_required: '请先关联项目',
  model_run_initiator_required: '此会话由另一位成员的模型发起；消息将等待该会话结束',
  model_connection_unavailable: '模型连接不可用，请更新个人设置',
  model_credential_required: '模型凭据缺失，请更新个人设置',
  model_default_required: '请先配置个人默认模型',
  workspace_unreleased: '前一次执行仍有资源待释放',
  run_not_resumable: '没有可安全恢复的成果，请检查前一次执行',
  lost_work_acknowledgement_required: '请明确确认放弃未交付成果',
  model_run_end_forbidden: '只有运行发起者或管理员可以停止和恢复此执行',
  run_already_stopped: '当前执行已结束，请刷新后查看成果状态',
  stale_run: '当前执行已变化，请刷新后重试',
  task_paused: '请先处理前一次执行的暂停或未保存成果',
}

/** Maps a persisted safe refusal code to the same copy as a refused request. */
export function taskConversationFailureCode(code: string | null): string {
  return MESSAGE_FAULTS[code ?? ''] ?? '任务执行暂不可用，请稍后重试'
}

/** Maps public Fault codes to actionable copy without displaying provider internals. */
export function taskConversationError(error: unknown): string {
  return MESSAGE_FAULTS[faultCode(error) ?? ''] ?? '操作失败，请稍后重试'
}
