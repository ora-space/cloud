import { useTranslation } from 'react-i18next'
import type { GraphWorkflowNodeState, WorkflowAgentConfig } from '@/lib/workflows/runtime'
import { unappliedAgentSettings, unappliedSettingKeys } from './agent-unapplied'

/**
 * v2's lines in the run inspector's Agent settings: the executor and model the attempt actually ran
 * on, which v2-server records once an Agent succeeds, and the configured settings v2 did not apply,
 * so none of them is silently ignored.
 */
export function RunActV2Execution({
  config,
  attempt,
}: {
  config: WorkflowAgentConfig
  attempt: Pick<GraphWorkflowNodeState, 'executor' | 'model' | 'unapplied'>
}) {
  const { t } = useTranslation()
  const { executor, model } = attempt
  const unapplied = unappliedAgentSettings(config, attempt.unapplied)
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
              <p className="text-[11px] text-amber-700 dark:text-amber-300">
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
          className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-[11px] leading-4 text-amber-800 dark:text-amber-200"
        >
          <p className="font-medium">
            {t('workflowRun.v2.unapplied', { settings: unapplied.map((setting) => t(unappliedSettingKeys[setting])).join(t('workflowRun.v2.listSeparator')) })}
          </p>
          <p className="mt-0.5 opacity-80">{t('workflowRun.v2.unappliedHint')}</p>
        </div>
      )}
    </>
  )
}
