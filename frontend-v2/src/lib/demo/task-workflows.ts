/**
 * Handling a task with a workflow, used only when `VITE_DEMO_MOCK=1` (see `flags.ts`).
 *
 * The run itself is real: it goes through the workflow API with the requirement as its input. What is
 * faked is that it belongs to a task, which the server cannot record yet: the run carries the task's
 * title as its name in the task's project, and only the task's assignee can start one, so that
 * person is its starter. Nothing moves the task when the run ends; the assignee submits it by hand.
 */
import { useQuery } from '@tanstack/react-query'
import { createWorkflowRun, listWorkflowRuns, listWorkflows, startWorkflowRun, updateWorkflowRunInput } from '@/lib/workflows/api/generated'
import { demoMockEnabled } from './flags'

/** The workflow the dialog picks when the workspace has it. */
export const DEMO_HANDLING_WORKFLOW = '需求开发'

/** A task the demo can hand to a workflow. */
export interface DemoHandledTask {
  id: string
  title: string
  description: string
  projectId: string
  assignee?: { displayName: string } | null
}

/** One workflow run that handles a task, in the shape the task page lists its workflow runs. */
export interface DemoTaskWorkflowRun {
  id: string
  workflowName: string
  workflowRunId: string
  status: 'running' | 'succeeded' | 'failed'
  failure: string
  createdCount: number
  startedByName: string
  createdAt: string
  handling: true
}

const statusOf = (status: string): DemoTaskWorkflowRun['status'] =>
  status === 'succeeded' ? 'succeeded' : status === 'failed' || status === 'cancelled' ? 'failed' : 'running'

export const demoTaskRunsKey = (tenantId: string, taskId: string) => ['demo-task-workflow-runs', tenantId, taskId]

/** The workflow runs handling this task, newest first; read again while one still executes. */
export function useDemoTaskRuns(tenantId: string, task: DemoHandledTask | undefined) {
  const query = useQuery({
    queryKey: demoTaskRunsKey(tenantId, task?.id ?? ''),
    enabled: demoMockEnabled() && !!tenantId && !!task,
    queryFn: async ({ signal }): Promise<DemoTaskWorkflowRun[]> => {
      const [{ runs }, { workflows }] = await Promise.all([
        listWorkflowRuns(tenantId, { projectId: task!.projectId }, undefined, signal),
        listWorkflows(tenantId, undefined, signal),
      ])
      return runs
        .filter(run => run.name === task!.title)
        .sort((a, b) => b.createdAt - a.createdAt)
        .map(run => ({
          id: run.id,
          workflowName: workflows.find(w => w.id === run.workflowId)?.name ?? '工作流',
          workflowRunId: run.id,
          status: statusOf(run.status),
          failure: '',
          createdCount: 0,
          startedByName: task!.assignee?.displayName ?? '',
          createdAt: new Date(run.createdAt).toISOString(),
          handling: true,
        }))
    },
    refetchInterval: q => (q.state.data?.some(run => run.status === 'running') ? 3000 : false),
  })
  return query.data ?? []
}

/** The requirement the dialog opens with: the task as written, and room for the assignee's own judgment. */
export function demoPromptFor(task: DemoHandledTask): string {
  return [`# ${task.title}`, task.description, '## 约束\n- '].filter(Boolean).join('\n\n')
}

/** Starts a published workflow on the task with `prompt` as its input, and returns the run's id. */
export async function startDemoTaskWorkflow(tenantId: string, task: DemoHandledTask, workflowId: string, prompt: string): Promise<string> {
  const { run } = await createWorkflowRun(tenantId, workflowId, { projectId: task.projectId, name: task.title })
  await updateWorkflowRunInput(tenantId, run.id, { input: prompt })
  await startWorkflowRun(tenantId, run.id)
  return run.id
}
