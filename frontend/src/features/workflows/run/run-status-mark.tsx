import { Ban, Check, Loader2, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { isNodeWorking, runStatusTone } from '@/features/workflows/runtime/run-status-style'
import type {
  GraphWorkflowNodeStatus,
  GraphWorkflowRunStatus,
} from '@/features/workflows/runtime/types'

type RunStatus = GraphWorkflowRunStatus | GraphWorkflowNodeStatus

type TerminalStatus = Extract<RunStatus, 'succeeded' | 'failed' | 'cancelled'>

function isTerminal(status: RunStatus): status is TerminalStatus {
  return status === 'succeeded' || status === 'failed' || status === 'cancelled'
}

const ICON_BOX = 'size-3.5'
const ICON_GLYPH = 'size-2.5'

/**
 * The status glyph — pick exactly one language per surface:
 * - `live`: a spinner (the working cue on the running card);
 * - terminal + not quiet: a check / cross / ban glyph;
 * - otherwise: a plain colour dot (always safe, never implies motion).
 */
export function RunStatusMark({
  status,
  live = false,
  quiet = false,
  className,
}: {
  status: RunStatus
  live?: boolean
  quiet?: boolean
  className?: string
}) {
  const tone = runStatusTone(status)

  if (live && isNodeWorking(status)) {
    return (
      <span
        className={cn(
          'inline-flex shrink-0 items-center justify-center rounded-full text-white',
          ICON_BOX,
          tone.dot,
          className,
        )}
        aria-hidden
      >
        <Loader2 className={cn(ICON_GLYPH, 'motion-safe:animate-spin')} strokeWidth={2.5} />
      </span>
    )
  }

  if (!quiet && isTerminal(status)) {
    return (
      <span
        className={cn(
          'inline-flex shrink-0 items-center justify-center rounded-full text-white',
          ICON_BOX,
          terminalSurface(status),
          className,
        )}
        aria-hidden
      >
        <TerminalGlyph status={status} className={ICON_GLYPH} />
      </span>
    )
  }

  return (
    <span
      className={cn('inline-flex size-1.5 shrink-0 rounded-full', tone.dot, className)}
      aria-hidden
    />
  )
}

/** One mark plus its label. Pass `live` only on the card that owns the working cue. */
export function RunStatusBadge({
  status,
  live = false,
  quiet = false,
  className,
}: {
  status: RunStatus
  live?: boolean
  quiet?: boolean
  className?: string
}) {
  const translate = useWorkflowTranslator()
  const tone = runStatusTone(status)
  return (
    <Badge
      variant="outline"
      className={cn('gap-1.5 border transition-colors duration-200', tone.badge, className)}
    >
      <RunStatusMark status={status} live={live} quiet={quiet} />
      {translate(tone.labelKey)}
    </Badge>
  )
}

/** The terminal glyph matching each finished state. */
function TerminalGlyph({ status, className }: { status: TerminalStatus; className: string }) {
  switch (status) {
    case 'succeeded':
      return <Check className={className} strokeWidth={3} />
    case 'failed':
      return <X className={className} strokeWidth={3} />
    case 'cancelled':
      return <Ban className={className} strokeWidth={2.5} />
    default:
      return null
  }
}

function terminalSurface(status: RunStatus): string {
  switch (status) {
    case 'succeeded':
      return 'bg-emerald-500'
    case 'failed':
      return 'bg-rose-500'
    case 'cancelled':
      return 'bg-zinc-400 dark:bg-zinc-500'
    default:
      return 'bg-muted text-muted-foreground'
  }
}
