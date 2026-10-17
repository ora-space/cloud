import type { RunPreparation as Preparation } from '@/api/generated.schemas'
import { sessionFailureMessage } from './thread-failure'

/** The clone step ended because the Project's latest repository sync failed. */
const REPOSITORY_SYNC_FAILED = 'repository_sync_failed'

/** The Workspace waits for the Project's first repository sync to produce a snapshot. */
const SNAPSHOT_NOT_READY = 'snapshot_not_ready'

/** Sync error codes are short snake_case classifications; anything else is never displayed. */
const SAFE_SYNC_ERROR_CODE = /^[a-z][a-z0-9_]{0,63}$/

function safeSyncErrorCode(code: unknown): string | null {
  return typeof code === 'string' && SAFE_SYNC_ERROR_CODE.test(code) ? code : null
}

/**
 * Displays only known safe classifications, never arbitrary server/provider error text.
 * `syncErrorCode` is the failed repository sync's code; it is shown only when it is a short
 * snake_case code, so an unexpected value is dropped rather than rendered.
 */
export function SessionFailure({
  code,
  syncErrorCode,
}: {
  code: string
  syncErrorCode?: string | null
}) {
  const syncCode = safeSyncErrorCode(syncErrorCode)
  return (
    <p role="alert" className="text-sm text-destructive">
      {sessionFailureMessage(code)}
      {syncCode && <> 同步错误码：{syncCode}</>}
    </p>
  )
}

const STAGE_LABELS: Record<Preparation['stage'], string> = {
  waiting: '等待 Agent 会话启动…',
  environment: '正在准备工作环境…',
  clone: '正在获取仓库…',
  plugin: '正在准备 Agent…',
  start: '正在启动 Agent 会话…',
  failed: 'Agent 启动失败',
  cancelled: '运行已取消',
}

/**
 * Shows authoritative preparation phases and bounded clone attempts before declaration. A
 * missing snapshot is a wait, not a failure; a failed repository sync is a failure whatever the
 * stage says, because no retry of the clone step can succeed without a new sync.
 */
export function RunPreparation({ progress }: { progress?: Preparation | null }) {
  if (progress?.stage === 'failed' || progress?.errorCode === REPOSITORY_SYNC_FAILED)
    return (
      <SessionFailure
        code={progress.errorCode ?? 'agent_failed'}
        syncErrorCode={progress.syncErrorCode}
      />
    )
  if (progress?.errorCode === SNAPSHOT_NOT_READY)
    return (
      <p role="status" className="text-sm text-muted-foreground">
        正在等待项目首次同步仓库，同步完成后自动继续。
      </p>
    )
  return (
    <p role="status" className="text-sm text-muted-foreground">
      {STAGE_LABELS[progress?.stage ?? 'waiting']}
      {progress?.stage === 'clone' && (
        <>
          {' '}
          尝试 {progress.cloneAttempts} / {progress.maxCloneAttempts}
        </>
      )}
      {progress?.retryAt && <> 等待重试，稍后自动继续。</>}
    </p>
  )
}
