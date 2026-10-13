import { AlertTriangle } from 'lucide-react'
import type { ProvisioningFailure } from '@/features/projects/provisioning'

/**
 * The banner shown when a project's creation ended in a terminal failure.
 *
 * It renders the cloud's own reason code next to the sentence, and says which
 * actions are available. It deliberately does not offer a retry button: the
 * reasons this banner exists for are not retryable by the member, and the
 * public retry route refuses a `failed` Operation anyway — offering one would
 * promise something the backend rejects.
 */
export function ProvisioningFailureNotice({ failure }: { failure: ProvisioningFailure }) {
  return (
    <div
      role="alert"
      className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm"
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0 text-destructive" />
      <div className="space-y-1">
        <p className="font-medium text-destructive">项目创建失败</p>
        <p className="text-muted-foreground">{failure.message}</p>
        <p className="text-xs text-muted-foreground">
          原因码：<code className="font-mono">{failure.code}</code>
        </p>
      </div>
    </div>
  )
}
