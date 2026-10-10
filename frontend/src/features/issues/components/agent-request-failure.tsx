import { Link, useParams } from 'react-router-dom'
import { faultCode } from '@/lib/api-client'
import { workspacePaths } from '@/lib/paths'

const MODEL_CONFIGURATION_FAULTS = new Set([
  'model_default_required',
  'model_credential_required',
  'model_connection_unavailable',
  'invalid_model_default',
])

/** Safe task refusal with an actionable route when a personal model configuration is missing. */
export function AgentRequestFailure({ error }: { error: unknown }) {
  const { workspaceSlug } = useParams()
  const needsModel = MODEL_CONFIGURATION_FAULTS.has(faultCode(error) ?? '')
  return (
    <p role="alert" className="text-xs text-destructive">
      {needsModel ? '请先配置可用的模型连接、API Key 和默认模型。' : '提交失败，请稍后重试。'}
      {needsModel && workspaceSlug && (
        <Link className="ml-1 underline" to={workspacePaths(workspaceSlug).modelConnections}>
          打开模型连接设置
        </Link>
      )}
    </p>
  )
}
