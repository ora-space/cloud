import { useState } from 'react'
import { Button } from '@/components/ui/button'
import type { ModelConnection } from '@/features/model-connections/api'
import { ConnectionForm } from '@/features/model-connections/connection-form'
import { CredentialForm } from '@/features/model-connections/credential-form'
import { ConnectionChoice } from '@/features/model-connections/fields'
import type { AgentConfiguration } from './api'
import type { AgentConfigurationDraft } from './draft'

/** Selects only enabled connections whose protocol the selected declaration accepts. */
export function AgentConnectionPicker({
  context,
  draft,
  connections,
  onChange,
}: {
  context: AgentConfiguration
  draft: AgentConfigurationDraft
  connections: ModelConnection[]
  onChange: (connectionId: string, modelId: string) => void
}) {
  const compatible = connections.filter(
    (connection) =>
      connection.enabled && context.declaration.modelAccess.protocols.includes(connection.protocol),
  )
  const selected = compatible.find((connection) => connection.id === draft.connectionId)
  return (
    <fieldset className="space-y-3">
      <legend className="text-sm font-medium">个人模型</legend>
      <ConnectionChoice
        label="模型连接"
        value={draft.connectionId}
        options={compatible.map((connection) => ({
          value: connection.id,
          label: connection.name + (connection.credentialConfigured ? '' : '（待配置 API Key）'),
        }))}
        onChange={(id) => onChange(id, '')}
      />
      <ConnectionChoice
        label="模型"
        value={draft.modelId}
        options={(selected?.models ?? []).map((model) => ({
          value: model.id,
          label: model.name + ' (' + model.id + ')',
        }))}
        onChange={(modelId) => onChange(draft.connectionId, modelId)}
      />
      {!selected && (
        <p className="text-xs text-muted-foreground">请选择匹配协议的已启用连接，或在下方新建。</p>
      )}
    </fieldset>
  )
}

/** Reuses the connection library and its write-only credential boundary inside the plugin entry. */
export function AgentConnectionTools({
  context,
  selected,
  onCreated,
}: {
  context: AgentConfiguration
  selected: ModelConnection | undefined
  onCreated: (connection: ModelConnection) => void
}) {
  const [creating, setCreating] = useState(false)
  const protocols = context.declaration.modelAccess.protocols
  const initialProtocol =
    protocols.includes('anthropic-messages') && !protocols.includes('openai-completions')
      ? 'anthropic-messages'
      : 'openai-completions'
  return (
    <section aria-label="个人连接管理" className="space-y-4 border-t pt-4">
      <h3 className="text-sm font-medium">个人连接管理</h3>
      <p className="text-xs text-muted-foreground">
        {selected
          ? `API Key 属于所选连接：${selected.name}`
          : '选择模型连接或新建一个，再配置它的 API Key。'}
      </p>
      <Button type="button" variant="outline" onClick={() => setCreating((current) => !current)}>
        新建个人连接
      </Button>
      {creating && (
        <ConnectionForm
          connection={undefined}
          initialProtocol={initialProtocol}
          onSaved={onCreated}
          onClose={() => setCreating(false)}
        />
      )}
      {selected && (
        <CredentialForm key={selected.id + ':' + selected.version} connection={selected} />
      )}
    </section>
  )
}
