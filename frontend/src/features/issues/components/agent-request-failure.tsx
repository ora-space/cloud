import { Link, useParams } from 'react-router-dom'
import { faultCode } from '@/lib/api-client'
import { workspacePaths } from '@/lib/paths'

const MODEL_CONFIGURATION_FAULTS = new Set([
  'model_default_required',
  'model_credential_required',
  'model_connection_unavailable',
  'invalid_model_default',
])

/** Safe task refusal with actionable project, target or personal-model guidance. */
export function AgentRequestFailure({ error }: { error: unknown }) {
  const { workspaceSlug } = useParams()
  const code = faultCode(error)
  const needsModel = MODEL_CONFIGURATION_FAULTS.has(code ?? '')
  const needsProject = code === 'issue_project_required'
  let message = '提交失败，请稍后重试。'
  if (needsModel) message = '请先配置可用的模型连接、API Key 和默认模型。'
  if (needsProject) message = '请关联可用项目，并选择与项目同空间的 Agent。'
  if (code === 'target_not_found') message = '协作对象已不可用，请重新选择可用的协作对象。'
  return (
    <p role="alert" className="text-xs text-destructive">
      {message}
      {needsProject && (
        <a className="ml-1 underline" href="#issue-project">
          选择关联项目
        </a>
      )}
      {needsModel && workspaceSlug && (
        <Link className="ml-1 underline" to={workspacePaths(workspaceSlug).modelConnections}>
          打开模型连接设置
        </Link>
      )}
    </p>
  )
}
