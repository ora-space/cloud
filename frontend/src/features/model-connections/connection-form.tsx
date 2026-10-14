import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  type ConnectionDraft,
  type ModelConnection,
  modelConnectionFailure,
  useSaveConnection,
} from './api'
import { ConnectionChoice, ConnectionField } from './fields'

type DraftModel = ConnectionDraft['models'][number] & { key: string }
const PROTOCOLS: readonly { value: ConnectionDraft['protocol']; label: string }[] = [
  { value: 'openai-completions', label: 'OpenAI Chat Completions' },
  { value: 'anthropic-messages', label: 'Anthropic Messages' },
]
const AUTH_MODES: readonly { value: ConnectionDraft['authMode']; label: string }[] = [
  { value: 'bearer', label: 'Bearer' },
  { value: 'x-api-key', label: 'x-api-key' },
]
const EMPTY_MODEL = { id: '', name: '', contextWindow: 128000, maxTokens: 8192 }

function MetadataFields({
  metadata,
  onChange,
}: {
  metadata: Omit<ConnectionDraft, 'models'>
  onChange: (metadata: Omit<ConnectionDraft, 'models'>) => void
}) {
  return (
    <>
      <ConnectionField
        label="连接名称"
        value={metadata.name}
        onChange={(name) => onChange({ ...metadata, name })}
      />
      <ConnectionChoice
        label="协议"
        value={metadata.protocol}
        options={PROTOCOLS}
        onChange={(protocol) => onChange({ ...metadata, protocol, authMode: 'bearer' })}
      />
      <ConnectionField
        label="服务地址"
        type="url"
        value={metadata.baseUrl}
        onChange={(baseUrl) => onChange({ ...metadata, baseUrl })}
      />
      <ConnectionChoice
        label="认证方式"
        value={metadata.authMode}
        options={AUTH_MODES.filter(
          (option) => metadata.protocol === 'anthropic-messages' || option.value === 'bearer',
        )}
        onChange={(authMode) => onChange({ ...metadata, authMode })}
      />
      <label className="flex items-center gap-2 text-sm">
        <Checkbox
          checked={metadata.enabled}
          onCheckedChange={(enabled) => onChange({ ...metadata, enabled })}
        />
        启用连接
      </label>
    </>
  )
}

function ModelFields({
  model,
  onChange,
  onRemove,
}: {
  model: DraftModel
  onChange: (model: DraftModel) => void
  onRemove: () => void
}) {
  return (
    <fieldset className="space-y-3 rounded-md border p-3">
      <legend className="px-1 text-xs">模型</legend>
      <div className="grid gap-3 sm:grid-cols-2">
        <ConnectionField
          label="模型标识"
          value={model.id}
          onChange={(id) => onChange({ ...model, id })}
        />
        <ConnectionField
          label="显示名称"
          value={model.name}
          onChange={(name) => onChange({ ...model, name })}
        />
        <ConnectionField
          label="上下文上限"
          type="number"
          value={model.contextWindow}
          onChange={(value) => onChange({ ...model, contextWindow: Number(value) })}
        />
        <ConnectionField
          label="输出上限"
          type="number"
          value={model.maxTokens}
          onChange={(value) => onChange({ ...model, maxTokens: Number(value) })}
        />
      </div>
      <Button type="button" variant="ghost" size="sm" onClick={onRemove}>
        移除模型
      </Button>
    </fieldset>
  )
}

function useConnectionDraft(connection: ModelConnection | undefined) {
  const [metadata, setMetadata] = useState<Omit<ConnectionDraft, 'models'>>({
    name: connection?.name ?? '',
    protocol: connection?.protocol ?? 'openai-completions',
    baseUrl: connection?.baseUrl ?? '',
    authMode: connection?.authMode ?? 'bearer',
    enabled: connection?.enabled ?? true,
  })
  const [models, setModels] = useState<DraftModel[]>(() =>
    connection
      ? connection.models.map((model) => ({ ...model, key: `saved-${model.id}` }))
      : [{ ...EMPTY_MODEL, key: 'new-0' }],
  )
  const [nextKey, setNextKey] = useState(1)
  function addModel() {
    setModels((current) => [...current, { ...EMPTY_MODEL, key: `new-${nextKey}` }])
    setNextKey((current) => current + 1)
  }
  return { metadata, setMetadata, models, setModels, addModel }
}

/**
 * Edits metadata only. The caller remounts it when the loaded resource version
 * changes; a refused write preserves the draft rather than overwriting it.
 */
export function ConnectionForm({
  connection,
  onClose,
}: {
  connection: ModelConnection | undefined
  onClose: () => void
}) {
  const { metadata, setMetadata, models, setModels, addModel } = useConnectionDraft(connection)
  const save = useSaveConnection(connection)
  function submit(event: React.FormEvent) {
    event.preventDefault()
    const draft = { ...metadata, models: models.map(({ key: _key, ...model }) => model) }
    save.mutate(draft, { onSuccess: onClose })
  }
  return (
    <form className="space-y-3 rounded-lg border p-4" onSubmit={submit}>
      <h3 className="font-medium">{connection ? '编辑连接' : '新建连接'}</h3>
      <MetadataFields metadata={metadata} onChange={setMetadata} />
      {models.map((model) => (
        <ModelFields
          key={model.key}
          model={model}
          onChange={(next) =>
            setModels((current) => current.map((item) => (item.key === model.key ? next : item)))
          }
          onRemove={() => setModels((current) => current.filter((item) => item.key !== model.key))}
        />
      ))}
      <Button type="button" variant="outline" onClick={addModel}>
        添加模型
      </Button>
      {save.isError && (
        <p role="alert" className="text-xs text-destructive">
          {modelConnectionFailure(save.error)}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" disabled={save.isPending || models.length === 0}>
          保存连接
        </Button>
        <Button type="button" variant="ghost" onClick={onClose}>
          取消
        </Button>
      </div>
    </form>
  )
}
