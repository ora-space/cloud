import type { GraphWorkflowNodeState, WorkflowAgentConfig } from '@/lib/workflows/runtime'
import { unappliedAgentSettings, unappliedSettingNames } from './agent-unapplied'

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
  const { executor, model } = attempt
  const unapplied = unappliedAgentSettings(config, attempt.unapplied)
  return (
    <>
      {executor && model && (
        <div className="space-y-1">
          <p className="text-[11px] text-muted-foreground">实际执行</p>
          <div
            data-selectable
            aria-label="实际执行"
            className="space-y-0.5 rounded-lg border border-border/70 bg-muted/25 px-3 py-2 text-xs text-foreground/90"
          >
            <p className="truncate font-mono text-[11px]">
              {executor.name || executor.agentId} · {model.name || model.id}
            </p>
            {executor.pluginVersion !== '' && <p className="text-[11px] text-muted-foreground">插件版本 {executor.pluginVersion}</p>}
            {model.fallback && (
              <p className="text-[11px] text-amber-700 dark:text-amber-300">
                {model.requested === ''
                  ? '节点没有指定模型，使用了 Agent 的默认模型'
                  : `没有找到配置的模型「${model.requested}」，使用了 Agent 的默认模型`}
              </p>
            )}
          </div>
        </div>
      )}
      {unapplied.length > 0 && (
        <div
          role="note"
          aria-label="v2 未应用的设置"
          className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-[11px] leading-4 text-amber-800 dark:text-amber-200"
        >
          <p className="font-medium">v2 未应用：{unapplied.map((setting) => unappliedSettingNames[setting]).join('、')}</p>
          <p className="mt-0.5 opacity-80">这些设置保存在工作流里，v2 执行时不生效，节点照常运行。</p>
        </div>
      )}
    </>
  )
}
