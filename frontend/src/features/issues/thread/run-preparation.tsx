import type { RunPreparation as Preparation } from '@/api/generated.schemas'
import { sessionFailureMessage } from './thread-failure'

/** Displays only known safe classifications, never arbitrary server/provider error text. */
export function SessionFailure({ code }: { code: string }) {
  return (
    <p role="alert" className="text-sm text-destructive">
      {sessionFailureMessage(code)}
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

/** Shows authoritative preparation phases and bounded clone attempts before declaration. */
export function RunPreparation({ progress }: { progress?: Preparation | null }) {
  if (progress?.stage === 'failed')
    return <SessionFailure code={progress.errorCode ?? 'agent_failed'} />
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
