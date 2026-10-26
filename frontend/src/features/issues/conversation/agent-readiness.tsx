import { Link } from 'react-router-dom'
import type { TaskAgentReadiness } from '@/api/generated.schemas'
import { taskConversationFailureCode } from './api'

const PLUGIN_GAPS = new Set([
  'agent_declaration_required',
  'agent_declaration_invalid',
  'agent_configuration_required',
  'agent_configuration_repair_required',
  'agent_model_protocol_mismatch',
  'agent_release_conflict',
  'agent_target_unavailable',
  'agent_plugin_unavailable',
])

const MODEL_GAPS = new Set([
  'model_connection_unavailable',
  'model_credential_required',
  'model_default_required',
  'model_not_found',
])

/** Shows only safe current-user configuration gaps; it grants no authority to append another model. */
export function AgentReadiness({
  readiness,
  settingsPath,
  pluginsPath,
}: {
  readiness: TaskAgentReadiness | undefined
  settingsPath: string | undefined
  pluginsPath?: string | undefined
}) {
  if (!readiness) return null
  const pluginGap = PLUGIN_GAPS.has(readiness.errorCode ?? '')
  const modelGap = MODEL_GAPS.has(readiness.errorCode ?? '')
  const actionPath = pluginGap ? pluginsPath : settingsPath
  if (readiness.errorCode)
    return (
      <div role="alert" className="space-y-1 text-xs text-destructive">
        <p>{taskConversationFailureCode(readiness.errorCode)}</p>
        {readiness.requiresPersonalModel && (pluginGap || modelGap) && actionPath && (
          <Link className="underline" to={actionPath} target="_blank" rel="noopener noreferrer">
            {pluginGap ? '前往插件设置' : '配置个人模型'}
          </Link>
        )}
      </div>
    )
  if (readiness.requiresPersonalModel)
    return <p className="text-xs text-muted-foreground">后续新运行使用你的个人模型配置。</p>
  return null
}
