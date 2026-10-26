import { workflowAdmissionKeys } from '@/features/workflows/publish-error-translations'
import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import type { Error as ApiError } from '@/api/generated.schemas'
import { workflowErrorKey } from '@/features/workflows/error-messages'
import { asJsonRecord } from '@/features/workflows/runtime/json-record'

/** Shows safe, actionable publish problems without rendering raw server messages or fault codes. */
export function WorkflowPublishError({ fault }: { fault: ApiError | undefined }) {
  const { t } = useTranslation()
  const raw = fault?.params?.['problems']
  const messages =
    fault?.code === 'workflow_graph_invalid' && Array.isArray(raw)
      ? [...new Set(raw.flatMap((problem: unknown) => problemMessage(problem, t)))]
      : []
  const summary =
    messages.length > 0 ? t('workflows.errors.graphProblems') : t(workflowErrorKey(fault?.code))

  return (
    <div role="alert" className="space-y-1 text-xs text-destructive">
      <p>{summary}</p>
      {messages.length > 0 && (
        <ul className="max-h-48 list-disc space-y-1 overflow-auto pl-4">
          {messages.map((message) => (
            <li key={message}>{message}</li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** Narrows the public problem shape; arbitrary server text never becomes interface copy. */
function problemMessage(value: unknown, t: TFunction): string[] {
  const problem = asJsonRecord(value)
  if (problem === undefined || typeof problem['code'] !== 'string') return []
  const node = problem['nodeId']
  const field = problem['field']
  const location = [
    typeof node === 'string' ? t('workflows.errors.problemNode', { node }) : '',
    ...['field', 'slot', 'name'].flatMap((key) => {
      const part = problem[key]
      if (key === 'field' && part === 'outputContract') {
        return [t('workflows.inspector.field.structuredOutput')]
      }
      return typeof part === 'string' ? [part] : []
    }),
  ]
    .filter(Boolean)
    .join(' · ')
  let reason = t(workflowAdmissionKeys[problem['code']] ?? 'workflows.errors.problemCheck')
  if (problem['code'] === 'node_config_unsupported') {
    reason =
      field === 'outputContract'
        ? t('workflows.errors.structuredOutputUnsupported')
        : t('workflows.errors.configUnsupported')
  } else if (problem['code'] === 'agent_schema_version_unsupported') {
    reason = t('workflows.errors.agentSchemaVersionUnsupported')
  } else if (problem['code'] === 'node_kind_unsupported') {
    reason = t('workflows.errors.nodeKindUnsupported')
  }
  return [location === '' ? reason : `${location}：${reason}`]
}
