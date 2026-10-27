import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import {
  configuredParameters,
  NodeParameterSummary,
} from '@/features/workflows/editor/node-parameter-summary'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { formatNodeOutput } from '@/features/workflows/runtime/format-node-output'
import { isNodeWorking } from '@/features/workflows/runtime/run-status-style'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import type { RunNodeEntry } from '@/features/workflows/run/run-node-state'

/**
 * The read-only side panel of a run's Overview: everything the reader may know
 * about one executed node, with no edit affordances.
 *
 * Config is summarized from the frozen snapshot node; the execution sections come
 * from the run trace. A node the run never reached shows only its identity and an
 * idle badge — no fabricated output or timings.
 *
 * @param props.data - The frozen node definition the run pinned.
 * @param props.entry - The run's row for the node, or `undefined` when it never ran. A container
 * member's row is the one from its container's selected round.
 * @param props.rounds - The round section of a container or of a container member, when the
 * node belongs to one.
 */
export function RunInspector({
  data,
  entry,
  rounds,
}: {
  data: WorkflowNodeData
  entry: RunNodeEntry | undefined
  rounds?: ReactNode
}) {
  return (
    <div
      data-workflow-run-inspector
      className="flex h-full min-h-0 flex-col gap-4 overflow-y-auto border-l border-border p-4"
    >
      <InspectorIdentity data={data} entry={entry} />
      {rounds}
      <InspectorConfig data={data} />
      <InspectorExecution entry={entry} />
      <InspectorOutput entry={entry} />
      <InspectorError entry={entry} />
    </div>
  )
}

/** The node's identity plus the one thing the run says about it: its status. */
export function InspectorIdentity({
  data,
  entry,
}: {
  data: WorkflowNodeData
  entry: RunNodeEntry | undefined
}) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const metadata = workflowNodeMetadata(data.kind)
  const Icon = metadata.icon
  const kindLabel = translate(workflowNodeTypeDefinition(data.kind).labelKey)
  const status = entry?.status ?? 'idle'
  return (
    <header className="flex items-start gap-3">
      <span
        className={cn(
          'flex size-10 shrink-0 items-center justify-center rounded-xl',
          metadata.tone,
        )}
      >
        <Icon className="size-5" strokeWidth={1.9} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <h3 className="min-w-0 truncate font-sans text-sm font-bold">{data.title}</h3>
          <RunStatusBadge
            status={status}
            live={isNodeWorking(status)}
            className="shrink-0 px-1.5 py-0 text-[9px]"
          />
        </div>
        <p className="mt-0.5 text-[11px] text-muted-foreground">{kindLabel}</p>
        {data.description !== '' && (
          <p className="mt-1.5 text-xs leading-5 text-muted-foreground">{data.description}</p>
        )}
        {entry?.displayName !== undefined && entry.displayName !== data.title && (
          <p className="mt-1 text-[10px] text-muted-foreground">
            {t('workflows.run.inspector.displayName', { name: entry.displayName })}
          </p>
        )}
      </div>
    </header>
  )
}

/**
 * The frozen configuration the run's node executed with.
 *
 * A node that wrote no editable configuration under either summary shows a quiet
 * "none recorded" line; container kinds (iteration, loop) communicate their content
 * through the frame itself, so they skip the empty box.
 */
function InspectorConfig({ data }: { data: WorkflowNodeData }) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const isContainer = data.kind === 'iteration' || data.kind === 'loop'
  const isEmpty = !isContainer && configuredParameters(data, translate).length === 0
  return (
    <section aria-label={t('workflows.run.inspector.config')}>
      <InspectorSectionTitle>{t('workflows.run.inspector.config')}</InspectorSectionTitle>
      {isEmpty ? (
        <p className="rounded-md border border-dashed border-border px-3 py-3 text-[11px] text-muted-foreground">
          {t('workflows.run.inspector.unconfigured')}
        </p>
      ) : (
        <NodeParameterSummary data={data} />
      )}
    </section>
  )
}

/** The execution row: status timings and any recorded error live alongside output. */
export function InspectorExecution({ entry }: { entry: RunNodeEntry | undefined }) {
  const { t } = useTranslation()
  if (entry === undefined) {
    return null
  }
  const rows: Array<[string, string]> = []
  if (entry.attempt !== undefined) {
    rows.push([t('workflows.run.inspector.attempt'), String(entry.attempt)])
  }
  if (entry.startedAt !== undefined) {
    rows.push([t('workflows.run.inspector.startedAt'), formatClock(entry.startedAt)])
  }
  if (entry.retryAt !== undefined) {
    // The retry instant is the only forward-looking row: it tells the reader when the
    // node will try again instead of leaving a failed attempt looking final.
    rows.push([t('workflows.run.inspector.retryAt'), formatClock(entry.retryAt)])
  }
  if (entry.finishedAt !== undefined) {
    rows.push([t('workflows.run.inspector.finishedAt'), formatClock(entry.finishedAt)])
  }
  if (rows.length === 0) {
    return null
  }
  return (
    <section aria-label={t('workflows.run.inspector.execution')}>
      <InspectorSectionTitle>{t('workflows.run.inspector.execution')}</InspectorSectionTitle>
      <dl className="space-y-1.5">
        {rows.map(([label, value]) => (
          <div key={label} className="flex items-baseline justify-between gap-3 text-xs">
            <dt className="shrink-0 text-muted-foreground">{label}</dt>
            <dd className="m-0 text-right font-mono text-[11px] tabular-nums">{value}</dd>
          </div>
        ))}
      </dl>
    </section>
  )
}

/** The node's output rendered readably, or a quiet empty state when none exists. */
export function InspectorOutput({ entry }: { entry: RunNodeEntry | undefined }) {
  const { t } = useTranslation()
  const output =
    entry !== undefined && entry.output !== undefined ? formatNodeOutput(entry.output) : ''
  return (
    <section aria-label={t('workflows.run.inspector.output')}>
      <InspectorSectionTitle>{t('workflows.run.inspector.output')}</InspectorSectionTitle>
      {output === '' ? (
        <p className="rounded-md border border-dashed border-border px-3 py-3 text-[11px] text-muted-foreground">
          {t('workflows.run.inspector.noOutput')}
        </p>
      ) : (
        <pre className="max-h-72 overflow-auto rounded-md bg-muted/60 px-3 py-2 font-mono text-[11px] leading-5 whitespace-pre-wrap break-words text-foreground/85">
          {output}
        </pre>
      )}
    </section>
  )
}

/** The node's error, rendered only when the trace recorded one. */
export function InspectorError({ entry }: { entry: RunNodeEntry | undefined }) {
  const { t } = useTranslation()
  const error = entry?.error
  if (error === undefined || error === '') {
    return null
  }
  return (
    <section aria-label={t('workflows.run.inspector.error')}>
      <InspectorSectionTitle>{t('workflows.run.inspector.error')}</InspectorSectionTitle>
      <pre className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 font-mono text-[11px] leading-5 whitespace-pre-wrap break-words text-destructive">
        {error}
      </pre>
    </section>
  )
}

/** The tiny section header every inspector section shares. */
function InspectorSectionTitle({ children }: { children: string }) {
  return <h4 className="mb-2 text-[11px] font-semibold text-muted-foreground">{children}</h4>
}

/** Formats an ISO timestamp as local `HH:MM:SS`, or a dash when it is unreadable. */
function formatClock(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '—'
  }
  return date.toLocaleTimeString([], { hour12: false })
}
