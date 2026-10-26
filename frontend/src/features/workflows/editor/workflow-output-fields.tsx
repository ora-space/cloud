import { useTranslation } from 'react-i18next'
import { Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { WorkflowCatalogSelector } from '@/features/workflows/editor/workflow-variable-select'
import type { WorkflowNodeData, WorkflowOutputBinding } from '@/features/workflows/runtime/types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'

/** Edits the persisted named outputs; unset rows remain drafts until publication validates them. */
export function OutputNodeFields({
  data,
  onChange,
  catalog,
}: {
  data: WorkflowNodeData
  onChange: (data: WorkflowNodeData) => void
  catalog: readonly WorkflowDecoratedVariable[]
}) {
  const { t } = useTranslation()
  const outputs = data.outputs ?? []
  const update = (next: WorkflowOutputBinding[]): void => onChange({ ...data, outputs: next })
  return (
    <section className="space-y-3" aria-label={t('workflows.output.bindings')}>
      <p className="text-[11px] leading-5 text-muted-foreground">
        {t('workflows.output.description')}
      </p>
      {outputs.map((binding, index) => (
        // oxlint-disable-next-line react/no-array-index-key -- names are editable and new rows have no persisted id; ordinal keys preserve focus while typing.
        <div key={index} className="space-y-2 rounded-lg border border-border p-2">
          <div className="flex gap-2">
            <Input
              aria-label={`${t('workflows.output.name')} ${index + 1}`}
              placeholder={t('workflows.output.namePlaceholder')}
              value={binding.name}
              onChange={(event) =>
                update(
                  outputs.map((row, i) =>
                    i === index ? { ...row, name: event.target.value } : row,
                  ),
                )
              }
            />
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              aria-label={`${t('workflows.output.remove')} ${index + 1}`}
              onClick={() => update(outputs.filter((_, i) => i !== index))}
            >
              <Trash2 />
            </Button>
          </div>
          <WorkflowCatalogSelector
            htmlId={`workflow-output-selector-${index}`}
            value={binding.variableSelector}
            placeholder={t('workflows.output.sourcePlaceholder')}
            choices={catalog}
            catalog={catalog}
            ariaLabel={`${t('workflows.output.source')} ${index + 1}`}
            onPick={(variableSelector) =>
              update(outputs.map((row, i) => (i === index ? { ...row, variableSelector } : row)))
            }
          />
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => update([...outputs, { name: '', variableSelector: [] }])}
      >
        <Plus />
        {t('workflows.output.add')}
      </Button>
    </section>
  )
}
