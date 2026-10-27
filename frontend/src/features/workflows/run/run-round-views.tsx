import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'
import { formatNodeOutput } from '@/features/workflows/runtime/format-node-output'
import { isNodeWorking } from '@/features/workflows/runtime/run-status-style'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import {
  selectedRound,
  type RunContainer,
  type RunRound,
  type RunRoundSelection,
} from '@/features/workflows/run/run-rounds'

/** A round's number as the reader counts it: Loop rounds from 1, Iteration items from 1. */
function roundLabel(
  container: RunContainer,
  round: RunRound,
): {
  key: 'workflows.run.rounds.loopRound' | 'workflows.run.rounds.iterationRound'
  index: number
} {
  return container.kind === 'loop'
    ? { key: 'workflows.run.rounds.loopRound', index: round.index }
    : { key: 'workflows.run.rounds.iterationRound', index: round.index + 1 }
}

/**
 * The round summary on a container's card: where the Loop is against its ceiling and how its
 * last round ended, or how many Iteration items settled and failed.
 */
export function RunContainerSummary({ container }: { container: RunContainer }) {
  const { t } = useTranslation()
  const last = container.rounds.at(-1)
  const parts =
    container.kind === 'loop'
      ? [t('workflows.run.rounds.loopSummary', { round: last?.index ?? 0, max: container.limit })]
      : [
          t('workflows.run.rounds.iterationSummary', {
            settled: container.settled ?? 0,
            total: container.limit,
          }),
          ...((container.failed ?? 0) > 0
            ? [t('workflows.run.rounds.iterationFailed', { count: container.failed })]
            : []),
        ]
  if (container.kind === 'loop' && last !== undefined) {
    parts.push(t(`workflows.run.rounds.outcome.${last.outcome}`))
  }
  return (
    <span
      data-workflow-run-round-summary
      className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground"
    >
      {parts.join(' · ')}
    </span>
  )
}

/**
 * The inspector section of a selected container: one button per round with its outcome, the
 * chosen round's members (name, status, output, error) and, for an Iteration, the item and the
 * ledger entry it settled into. Picking a round repaints the canvas members.
 *
 * @param props.memberTitles - Frozen member titles by id, in snapshot order.
 */
export function RunContainerRounds({
  container,
  selection,
  memberTitles,
  onSelectRound,
}: {
  container: RunContainer
  selection: RunRoundSelection
  memberTitles: ReadonlyMap<string, string>
  onSelectRound: (containerId: string, index: number) => void
}) {
  const { t } = useTranslation()
  const current = selectedRound(container, selection)
  return (
    <section aria-label={t('workflows.run.rounds.title')} data-workflow-run-rounds>
      <h4 className="mb-2 text-[11px] font-semibold text-muted-foreground">
        {t('workflows.run.rounds.title')}
      </h4>
      {container.reason !== undefined && (
        <p className="mb-2 font-mono text-[11px] text-destructive">
          {t('workflows.run.rounds.reason', { reason: container.reason })}
        </p>
      )}
      {container.rounds.length === 0 ? (
        <p className="rounded-md border border-dashed border-border px-3 py-3 text-[11px] text-muted-foreground">
          {t('workflows.run.rounds.empty')}
        </p>
      ) : (
        <>
          <RoundPicker container={container} current={current} onSelectRound={onSelectRound} />
          {current !== undefined && (
            <RoundDetail container={container} round={current} memberTitles={memberTitles} />
          )}
        </>
      )}
    </section>
  )
}

/**
 * The round switch shown on a selected member: the member's state follows the round chosen
 * for its container, and switching here changes that same choice.
 */
export function RunMemberRoundSwitch({
  container,
  selection,
  onSelectRound,
}: {
  container: RunContainer
  selection: RunRoundSelection
  onSelectRound: (containerId: string, index: number) => void
}) {
  const { t } = useTranslation()
  if (container.rounds.length === 0) {
    return null
  }
  return (
    <section aria-label={t('workflows.run.rounds.viewing')}>
      <h4 className="mb-2 text-[11px] font-semibold text-muted-foreground">
        {t('workflows.run.rounds.viewing')}
      </h4>
      <RoundPicker
        container={container}
        current={selectedRound(container, selection)}
        onSelectRound={onSelectRound}
      />
    </section>
  )
}

/** One toggle per round, labelled with its number and outcome. */
function RoundPicker({
  container,
  current,
  onSelectRound,
}: {
  container: RunContainer
  current: RunRound | undefined
  onSelectRound: (containerId: string, index: number) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="mb-3 flex flex-wrap gap-1.5" role="group">
      {container.rounds.map((round) => {
        const label = roundLabel(container, round)
        const pressed = round.index === current?.index
        return (
          <button
            key={round.index}
            type="button"
            aria-pressed={pressed}
            className={cn(
              'rounded-md border px-2 py-1 text-[11px] outline-none focus-visible:ring-2 focus-visible:ring-ring',
              pressed ? 'border-foreground/40 bg-muted font-semibold' : 'border-border',
              (round.outcome === 'failed' || round.outcome === 'absorbed') && 'text-destructive',
            )}
            onClick={() => onSelectRound(container.id, round.index)}
          >
            {t(label.key, { index: label.index })}
            <span className="ml-1 text-muted-foreground">
              {t(`workflows.run.rounds.outcome.${round.outcome}`)}
            </span>
          </button>
        )
      })}
    </div>
  )
}

/** The chosen round: its item and ledger entry (Iteration) and every member's state. */
function RoundDetail({
  container,
  round,
  memberTitles,
}: {
  container: RunContainer
  round: RunRound
  memberTitles: ReadonlyMap<string, string>
}) {
  const { t } = useTranslation()
  return (
    <div className="space-y-3">
      {container.kind === 'iteration' && 'item' in round && (
        <div>
          <p className="mb-1 text-[11px] text-muted-foreground">{t('workflows.run.rounds.item')}</p>
          <pre className="max-h-32 overflow-auto rounded-md bg-muted/60 px-3 py-2 font-mono text-[11px] leading-5 whitespace-pre-wrap break-words">
            {formatNodeOutput(round.item)}
          </pre>
        </div>
      )}
      {round.entry?.status === 'succeeded' && (
        <div>
          <p className="mb-1 text-[11px] text-muted-foreground">
            {t('workflows.run.rounds.collected')}
          </p>
          <pre className="max-h-32 overflow-auto rounded-md bg-muted/60 px-3 py-2 font-mono text-[11px] leading-5 whitespace-pre-wrap break-words">
            {formatNodeOutput(round.entry.output)}
          </pre>
        </div>
      )}
      {round.entry?.error != null && (
        <pre className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 font-mono text-[11px] leading-5 whitespace-pre-wrap break-words text-destructive">
          {round.entry.error}
        </pre>
      )}
      <div>
        <p className="mb-1 text-[11px] text-muted-foreground">
          {t('workflows.run.rounds.members')}
        </p>
        <ul className="space-y-2">
          {container.memberIds.map((memberId) => (
            <RoundMember
              key={memberId}
              title={memberTitles.get(memberId) ?? memberId}
              entry={round.states[memberId]}
            />
          ))}
        </ul>
      </div>
    </div>
  )
}

/** One member row: name and status; output and error when it ran. */
function RoundMember({
  title,
  entry,
}: {
  title: string
  entry: RunRound['states'][string] | undefined
}) {
  const { t } = useTranslation()
  const status = entry?.status ?? 'inactive'
  const output = entry?.output === undefined ? '' : formatNodeOutput(entry.output)
  return (
    <li className="rounded-md border border-border px-2.5 py-2" data-workflow-run-round-member>
      <div className="flex items-center justify-between gap-2">
        <span className="min-w-0 truncate text-xs font-medium">{title}</span>
        {status === 'inactive' ? (
          <span className="shrink-0 text-[10px] text-muted-foreground">
            {t('workflows.run.rounds.notRun')}
          </span>
        ) : (
          <RunStatusBadge
            status={status}
            live={isNodeWorking(status)}
            className="shrink-0 px-1.5 py-0 text-[9px]"
          />
        )}
      </div>
      {output !== '' && (
        <pre className="mt-1.5 max-h-32 overflow-auto rounded bg-muted/60 px-2 py-1 font-mono text-[10px] leading-4 whitespace-pre-wrap break-words">
          {output}
        </pre>
      )}
      {entry?.error !== undefined && entry.error !== '' && (
        <p className="mt-1.5 font-mono text-[10px] break-words text-destructive">{entry.error}</p>
      )}
    </li>
  )
}
