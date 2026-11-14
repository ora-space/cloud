import { useTranslation } from 'react-i18next'
import type { GraphWorkflowNodeState, WorkflowAgentUnappliedCapability } from '@/lib/workflows/runtime'
import { useAnalyzedUnapplied } from './run-analysis-context'

/** The translation key the run inspector names each setting v2 did not apply by. */
const unappliedSettingKeys: Readonly<Record<WorkflowAgentUnappliedCapability, string>> = {
  role: 'workflowRun.v2.setting.role',
  skill: 'workflowRun.v2.setting.skill',
  mcp: 'workflowRun.v2.setting.mcp',
  retry: 'workflowRun.v2.setting.retry',
  interactive: 'workflowRun.v2.setting.interactive',
}

function isKnownSetting(setting: string): setting is WorkflowAgentUnappliedCapability {
  return Object.hasOwn(unappliedSettingKeys, setting)
}

/**
 * v2's lines in the run inspector's Agent settings: the executor and model the attempt actually ran
 * on, which v2-server records once an Agent succeeds, and the settings v2 does not apply, in the
 * server's order. Those are what the attempt recorded (`payload.unapplied`, written on success);
 * without a record (running, failed, cancelled, interrupted, or an old run) they are what the
 * server's analysis of the run's frozen graph lists for the node. The page derives nothing from the
 * node's configuration, so a setting v2 starts applying stops being named without a frontend change.
 */
export function RunActV2Execution({
  nodeId,
  attempt,
}: {
  nodeId?: string
  attempt: Pick<GraphWorkflowNodeState, 'executor' | 'model' | 'unapplied'>
}) {
  const { t } = useTranslation()
  const { executor, model } = attempt
  const analyzed = useAnalyzedUnapplied(nodeId)
  const unapplied: readonly string[] = attempt.unapplied ?? analyzed ?? []
  return (
    <>
      {executor && model && (
        <div className="space-y-1">
          <p className="text-[11px] text-muted-foreground">{t('workflowRun.v2.executedOn')}</p>
          <div
            data-selectable
            aria-label={t('workflowRun.v2.executedOn')}
            className="space-y-0.5 rounded-lg border border-border/70 bg-muted/25 px-3 py-2 text-xs text-foreground/90"
          >
            <p className="truncate font-mono text-[11px]">
              {executor.name || executor.agentId} · {model.name || model.id}
            </p>
            {executor.pluginVersion !== '' && <p className="text-[11px] text-muted-foreground">{t('workflowRun.v2.pluginVersion', { version: executor.pluginVersion })}</p>}
            {model.fallback && (
              <p className="text-[11px] text-warning">
                {model.requested === ''
                  ? t('workflowRun.v2.modelDefault')
                  : t('workflowRun.v2.modelFallback', { model: model.requested })}
              </p>
            )}
          </div>
        </div>
      )}
      {unapplied.length > 0 && (
        <div
          role="note"
          aria-label={t('workflowRun.v2.unappliedLabel')}
          className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-[11px] leading-4 text-warning"
        >
          <p className="font-medium">
            {t('workflowRun.v2.unapplied', { settings: unapplied.map((setting) => (isKnownSetting(setting) ? t(unappliedSettingKeys[setting]) : setting)).join(t('workflowRun.v2.listSeparator')) })}
          </p>
          <p className="mt-0.5 opacity-80">{t('workflowRun.v2.unappliedHint')}</p>
        </div>
      )}
    </>
  )
}
