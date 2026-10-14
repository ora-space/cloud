import type { VariantProps } from 'class-variance-authority'
import type { badgeVariants } from '@/components/ui/badge'
import type { ProvisioningPhase } from '@/features/projects/provisioning'
import type { Project } from '@/mocks/data/types'

/** The badge variants a project status may use, taken from the badge itself. */
type ProjectStatusVariant = NonNullable<VariantProps<typeof badgeVariants>['variant']>

export const PROJECT_STATUS_LABELS: Record<Project['status'], string> = {
  planned: '规划中',
  in_progress: '进行中',
  completed: '已完成',
  paused: '已暂停',
}

export const PROJECT_STATUS_VARIANT: Record<Project['status'], ProjectStatusVariant> = {
  planned: 'secondary',
  in_progress: 'default',
  completed: 'secondary',
  paused: 'outline',
}

/**
 * Text for each phase of the Operation that creates a project. `provisioning`
 * and `succeeded` read exactly like the lifecycle statuses they stand for,
 * because they mean the same thing to a member; `retrying` and `failed` are the
 * two the lifecycle alone cannot express.
 */
export const PROVISIONING_PHASE_LABELS: Record<ProvisioningPhase, string> = {
  provisioning: '规划中',
  retrying: '重试中',
  failed: '创建失败',
  succeeded: '进行中',
}

export const PROVISIONING_PHASE_VARIANT: Record<ProvisioningPhase, ProjectStatusVariant> = {
  provisioning: 'secondary',
  retrying: 'outline',
  failed: 'destructive',
  succeeded: 'default',
}

/**
 * The badge a project shows: the phase of the Operation creating it when that
 * phase is known, and the project's own lifecycle status otherwise.
 *
 * The phase wins because the lifecycle has no failure state: a project whose
 * creation ended in a terminal failure keeps `lifecycle = 'provisioning'` for
 * good, so the lifecycle alone would label it "规划中" forever. A phase that is
 * not known — no Workspace yet, or an Operation state this build has not been
 * taught — leaves the lifecycle status in place rather than claiming a state
 * the frontend cannot prove.
 */
export function projectStatusBadge(
  status: Project['status'],
  phase: ProvisioningPhase | undefined,
): { label: string; variant: ProjectStatusVariant } {
  if (phase) {
    return { label: PROVISIONING_PHASE_LABELS[phase], variant: PROVISIONING_PHASE_VARIANT[phase] }
  }
  return { label: PROJECT_STATUS_LABELS[status], variant: PROJECT_STATUS_VARIANT[status] }
}
