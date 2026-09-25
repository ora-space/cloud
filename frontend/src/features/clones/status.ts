import type { VariantProps } from 'class-variance-authority'
import type { badgeVariants } from '@/components/ui/badge'
import type { CloneOperation, CloneStateReason } from '@/api/generated.schemas'

/**
 * Where a clone stands, derived only from Cloud's facts. Cloud reports queued and dispatched
 * requests both as `pending`; a recorded `executionId` is what tells them apart. There is no
 * "running" stage: Cloud does not know whether Git is running.
 */
export type CloneStage = 'queued' | 'dispatched' | 'succeeded' | 'failed'

/** Maps an operation onto its {@link CloneStage}. */
export function cloneStage(operation: CloneOperation): CloneStage {
  if (operation.state.kind === 'succeeded') return 'succeeded'
  if (operation.state.kind === 'failed') return 'failed'
  return operation.executionId === null ? 'queued' : 'dispatched'
}

/** Badge text per stage. */
export const CLONE_STAGE_LABELS: Record<CloneStage, string> = {
  queued: '排队中',
  dispatched: '已派发',
  succeeded: '已完成',
  failed: '失败',
}

/** Badge variant per stage; only a terminal failure uses the destructive style. */
export const CLONE_STAGE_VARIANT: Record<
  CloneStage,
  NonNullable<VariantProps<typeof badgeVariants>['variant']>
> = {
  queued: 'outline',
  dispatched: 'secondary',
  succeeded: 'default',
  failed: 'destructive',
}

/** Member-facing text for each failure reason the Node reports. */
export const CLONE_FAILURE_LABELS: Record<CloneStateReason, string> = {
  sourceUnavailable: '仓库暂不可用',
  branchNotFound: '找不到指定分支',
  destinationConflict: '目标目录冲突',
  operationFailed: 'Git 操作失败',
  interrupted: '执行被中断，可重新提交',
  unspecified: '未说明原因',
}
