import { useMutation } from '@tanstack/react-query'
import { useCallback } from 'react'
import { useTranslation } from 'react-i18next'
import { localizeContractError } from '@/lib/workflows/host/contract-error'
import { toast } from '@/lib/workflows/host/ui'
import { useChatStore } from '../store'
import { updateRunInput } from './server-runs'

// The desktop hooks the copied Theater components call, over v2-server's run API. Each keeps
// desktop's name and call shape so the copied files change only their import path.

export { agentLabel, type AgentEntry, useAgentCatalog } from '@/lib/workflows/data/use-agent-catalog'
export { useAgents, useInstalledPlugins, useSkills } from '@/lib/workflows/data/catalog-hooks'

/** Shows a failed action as a toast, titled by the action when the caller names it. */
export function useContractErrorToast(): (error: unknown, title?: string) => void {
  const { t } = useTranslation()
  return useCallback(
    (error: unknown, title?: string) => {
      const message = localizeContractError(error, t)
      toast.error(title ?? message, { description: title === undefined ? undefined : message })
    },
    [t],
  )
}

/** Saves the Start node's prompt and values edited in Theater's inspector. */
export function useUpdateWorkflowRunInput() {
  const tenantId = useChatStore((s) => s.tenantId) ?? ''
  return useMutation({
    mutationFn: ({ runId, input, variables }: { runId: string; input: string; variables?: Record<string, unknown> }) =>
      updateRunInput(tenantId, runId, input, variables),
  })
}

/**
 * Answers a human-in-the-loop gate. v2 executes no interactive node, so no gate ever opens; the
 * hook refuses instead of pretending an answer reached a run.
 */
export function useSubmitGraphWorkflowHitl() {
  return useMutation({
    mutationFn: async (_input: { runId: string; requestId: string; payload: Record<string, unknown> }): Promise<never> => {
      throw new Error('v2 不执行需要人工参与的节点')
    },
  })
}

/** v2 has no AI diagnosis; the inspector hides it (see `useWorkspaceSelectionStore`). */
export function useDiagnoseWorkflowNodeFailure() {
  return useMutation({
    mutationFn: async (_input: { runId: string; nodeId: string }): Promise<void> => {
      throw new Error('v2 不提供 AI 诊断')
    },
  })
}

interface DemoSelection {
  selection: { workflowRunId: string | null }
}

/**
 * Desktop's inspector reads the selected run only to offer AI diagnosis. Reporting none keeps
 * that button hidden in v2.
 */
export function useWorkspaceSelectionStore<T>(selector: (state: DemoSelection) => T): T {
  return selector({ selection: { workflowRunId: null } })
}

/** The run's file changes would open the review pane, which the chat page does not have. */
export function useTaskChangesNavigation(): { openDiff: (path: string) => void } | null {
  return null
}

/** Shows an Agent's absolute worktree path relative to the worktree, as desktop's diff list does. */
export function displayPath(path: string): string {
  const managedWorktreePath = path.match(/(?:^|[\\/])worktrees[\\/][^\\/]+[\\/](.+)$/)?.[1]
  return managedWorktreePath?.replaceAll('\\', '/') ?? path
}
